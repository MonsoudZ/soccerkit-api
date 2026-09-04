package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// Notifier tells a person about something they would otherwise have to go looking for.
//
// An interface, and a narrow one, because the delivery half is the part this package
// should not know: it needs Apple's credentials, speaks HTTP/2 to a host that may be
// down, and must never be able to fail a request that has already succeeded. The API
// records what happened and says who to tell; internal/push decides how, and cmd/api is
// where the two are joined. A server built without one still works -- every test does
// exactly that.
type Notifier interface {
	Notify(ctx context.Context, personID uuid.UUID, note Notification)
}

// Notification is one message, in the terms a handler can express.
type Notification struct {
	Title string
	Body  string
	Data  map[string]string
}

// noopNotifier is what a Server has until something installs a real one. Notifications
// are a convenience over data the API already serves, so dropping them changes nothing
// a caller could not find by asking.
type noopNotifier struct{}

func (noopNotifier) Notify(context.Context, uuid.UUID, Notification) {}

// notify is the single call site's worth of indirection that keeps every handler from
// having to nil-check.
func (s *Server) notify(ctx context.Context, personID uuid.UUID, note Notification) {
	if s.notifier == nil {
		return
	}
	s.notifier.Notify(ctx, personID, note)
}

type registerDeviceRequest struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
}

// handleRegisterDevice records where to reach this person.
//
// Registering the same token again is the normal case, not an error: the app registers
// on every launch, and Apple reissues the same token for the same install. It also
// re-points the token at the caller, which is the behaviour that matters when a device
// changes hands -- see 0012_device_tokens.sql.
func (s *Server) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	var req registerDeviceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	token, err := normalizeDeviceToken(req.Token)
	if err != nil {
		writeError(w, err)
		return
	}
	platform := req.Platform
	if platform == "" {
		platform = "ios"
	}
	if platform != "ios" {
		writeError(w, errValidation("platform must be ios"))
		return
	}
	if _, err := s.store.UpsertDeviceToken(r.Context(), store.UpsertDeviceTokenParams{
		Token: token, PersonID: personIDFrom(r.Context()), Platform: platform,
	}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUnregisterDevice stops pushes to one device, which is what sign-out should do.
// Idempotent: a token already gone is the state the caller asked for.
func (s *Server) handleUnregisterDevice(w http.ResponseWriter, r *http.Request) {
	token, err := normalizeDeviceToken(chi.URLParam(r, "token"))
	if err != nil {
		writeError(w, err)
		return
	}
	// Scoped to the caller in the statement, so holding someone else's token is not
	// enough to silence their phone.
	if _, err := s.store.DeleteDeviceToken(r.Context(), store.DeleteDeviceTokenParams{
		Token: token, PersonID: personIDFrom(r.Context()),
	}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// normalizeDeviceToken checks the shape APNs uses: 32 bytes as lower-case hex. Validated
// rather than stored as given because the token becomes a path segment in the request to
// Apple, and because a token of the wrong shape is a client bug worth reporting now
// instead of a delivery failure logged later.
func normalizeDeviceToken(raw string) (string, error) {
	token := strings.ToLower(strings.TrimSpace(raw))
	if token == "" {
		return "", errValidation("token is required")
	}
	if len(token) != 64 {
		return "", errValidation("token must be a 64-character hex APNs device token")
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return "", errValidation("token must be a 64-character hex APNs device token")
		}
	}
	return token, nil
}
