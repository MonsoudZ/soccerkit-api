package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// appleAuthRequest mirrors the iOS app's AppleAuthRequest: the identity token to
// verify, plus the one-time authorization code and the full name (Apple only
// sends the name on the very first authorization).
type appleAuthRequest struct {
	IdentityToken     string  `json:"identityToken"`
	AuthorizationCode *string `json:"authorizationCode"`
	FullName          *string `json:"fullName"`
}

// appleAuthResponse mirrors the app's AuthResponse: the session token and the
// Person the account maps to.
type appleAuthResponse struct {
	Token    string `json:"token"`
	PersonID string `json:"personID"`
}

// handleAppleAuth verifies a Sign in with Apple identity token and resolves it to
// a Person: an existing account linked by Apple sub, an existing account matched
// by email (linked on the spot), or a freshly provisioned identity (Person +
// UserAccount + personal Org + admin/director/coach memberships + seed
// templates — the same provisioning as email registration). Returns an access
// token identifying the Person, matching the app's `{ token, personID }` shape.
func (s *Server) handleAppleAuth(w http.ResponseWriter, r *http.Request) {
	var req appleAuthRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(req.IdentityToken) == "" {
		writeError(w, errValidation("identityToken is required"))
		return
	}

	ctx := r.Context()
	identity, err := s.apple.verify(ctx, req.IdentityToken)
	if err != nil {
		writeError(w, errUnauthorized("Apple sign-in could not be verified"))
		return
	}

	// 1. Returning user — already linked by Apple sub.
	if account, err := s.store.GetUserAccountByAppleSub(ctx, &identity.Sub); err == nil {
		person, err := s.store.GetPerson(ctx, account.PersonID)
		if err != nil {
			writeError(w, err)
			return
		}
		s.respondAppleToken(w, person.ID, account.Email)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}

	// 2. First Apple sign-in — link an existing email account or provision anew,
	// atomically.
	email := appleEmail(identity)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	q := s.store.WithTx(tx)

	var personID uuid.UUID
	var accountEmail string

	existing, err := q.GetUserAccountByEmail(ctx, email)
	switch {
	case err == nil:
		// An email/password account already exists for this address — link it.
		if err := q.LinkAppleSub(ctx, store.LinkAppleSubParams{ID: existing.ID, AppleSub: &identity.Sub}); err != nil {
			writeError(w, err)
			return
		}
		personID, accountEmail = existing.PersonID, existing.Email
	case errors.Is(err, pgx.ErrNoRows):
		person, account, perr := s.provisionAppleIdentity(ctx, q, identity, req.FullName, email)
		if perr != nil {
			writeError(w, perr)
			return
		}
		personID, accountEmail = person.ID, account.Email
	default:
		writeError(w, err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeError(w, err)
		return
	}
	s.respondAppleToken(w, personID, accountEmail)
}

// provisionAppleIdentity creates the full identity for a first-time Apple user,
// mirroring email registration (Person + UserAccount + personal Org + roles +
// seeded templates).
func (s *Server) provisionAppleIdentity(
	ctx context.Context, q *store.Queries, identity appleIdentity, fullName *string, email string,
) (store.Person, store.UserAccount, error) {
	displayName := appleDisplayName(fullName, email)

	person, err := q.CreatePerson(ctx, store.CreatePersonParams{DisplayName: displayName, Email: &email})
	if err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	sub := identity.Sub
	account, err := q.CreateUserAccount(ctx, store.CreateUserAccountParams{
		PersonID: person.ID, Email: email, AppleSub: &sub,
	})
	if err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: displayName + "'s Club", Kind: "personal",
	})
	if err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	for _, role := range []string{"admin", "director", "coach"} {
		if _, err := q.CreateMembership(ctx, store.CreateMembershipParams{
			PersonID: person.ID, OrganizationID: org.ID, Role: role,
		}); err != nil {
			return store.Person{}, store.UserAccount{}, err
		}
	}
	if err := seedDefaultTemplates(ctx, q, org.ID, person.ID); err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	return person, account, nil
}

func (s *Server) respondAppleToken(w http.ResponseWriter, personID uuid.UUID, email string) {
	token, err := s.signAccessToken(personID, email)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, appleAuthResponse{Token: token, PersonID: personID.String()})
}

// appleEmail returns the token's email, or a stable synthesized address when the
// user chose to hide it (Apple omits email on later sign-ins and for hidden
// relays), so the NOT NULL UNIQUE user_accounts.email constraint always holds.
func appleEmail(identity appleIdentity) string {
	if e := strings.ToLower(strings.TrimSpace(identity.Email)); e != "" {
		return e
	}
	return fmt.Sprintf("apple_%s@users.soccercoachkit.app", identity.Sub)
}

// appleDisplayName prefers the name Apple sent on first authorization, else the
// email local part, else a neutral default.
func appleDisplayName(fullName *string, email string) string {
	if fullName != nil {
		if t := strings.TrimSpace(*fullName); t != "" {
			return t
		}
	}
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" {
		return local
	}
	return "Coach"
}
