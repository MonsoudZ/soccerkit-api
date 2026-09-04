package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// fakeDevices stands in for the store. The delivery questions worth asking here -- what
// gets posted, and which rejections retire a token -- are not questions about Postgres.
type fakeDevices struct {
	mu      sync.Mutex
	tokens  []string
	deleted []string
}

func (f *fakeDevices) ListDeviceTokensForPerson(context.Context, uuid.UUID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...), nil
}

func (f *fakeDevices) DeleteDeviceTokenAnyOwner(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, token)
	return nil
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// newTestSender wires a sender at a stub Apple.
func newTestSender(t *testing.T, devices *fakeDevices, handler http.HandlerFunc) (*Sender, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewSender(devices, Config{
		KeyID: "KEY123", TeamID: "TEAM456", BundleID: "com.example.app",
		PrivateKey: testKey(t), Host: srv.URL,
	}), srv
}

// TestSenderPostsAnAlert pins the request Apple actually receives. Every part of it is
// something Apple rejects if it is wrong, and none of it is visible from this side
// except in a delivery that silently never arrives.
func TestSenderPostsAnAlert(t *testing.T) {
	const token = "aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233"
	devices := &fakeDevices{tokens: []string{token}}

	var gotPath, gotTopic, gotAuth, gotPushType string
	var gotBody map[string]any
	sender, _ := newTestSender(t, devices, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTopic = r.URL.Path, r.Header.Get("apns-topic")
		gotAuth, gotPushType = r.Header.Get("authorization"), r.Header.Get("apns-push-type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	})

	sender.deliver(context.Background(), delivery{
		personID: uuid.New(),
		note: Notification{
			Title: "Invitation to Riverside FC",
			Body:  "You have been invited to join Riverside FC as coach.",
			Data:  map[string]string{"type": "invitation", "invitationId": "abc"},
		},
	})

	if want := "/3/device/" + token; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotTopic != "com.example.app" {
		t.Errorf("apns-topic = %q; Apple rejects a push whose topic is not the bundle id", gotTopic)
	}
	if !strings.HasPrefix(gotAuth, "bearer ") || strings.Count(gotAuth, ".") != 2 {
		t.Errorf("authorization should be a bearer JWT, got %q", gotAuth)
	}
	if gotPushType != "alert" {
		t.Errorf("apns-push-type = %q, want alert", gotPushType)
	}

	aps, _ := gotBody["aps"].(map[string]any)
	alert, _ := aps["alert"].(map[string]any)
	if alert["title"] != "Invitation to Riverside FC" {
		t.Errorf("alert title = %v", alert["title"])
	}
	if alert["body"] == "" || alert["body"] == nil {
		t.Errorf("alert body missing: %v", gotBody)
	}
	// Data rides beside aps, not inside it, which is where the app reads it from.
	if gotBody["type"] != "invitation" || gotBody["invitationId"] != "abc" {
		t.Errorf("data keys should sit at the top level, got %v", gotBody)
	}
}

// TestSenderDropsAnApsDataKey — a data key called "aps" would overwrite the alert and
// send a push with no message in it.
func TestSenderDropsAnApsDataKey(t *testing.T) {
	payload, err := buildPayload(Notification{
		Title: "T", Body: "B", Data: map[string]string{"aps": "hijack", "keep": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["aps"].(map[string]any); !ok {
		t.Errorf("aps was overwritten by a data key: %s", payload)
	}
	if got["keep"] != "yes" {
		t.Errorf("other data keys should survive: %s", payload)
	}
}

// TestSenderPrunesTokensAppleRejects covers the two rejections that mean "this row will
// never deliver again". Keeping such a token guarantees one failed request per
// notification, forever.
func TestSenderPrunesTokensAppleRejects(t *testing.T) {
	for _, reason := range []string{"Unregistered", "BadDeviceToken"} {
		t.Run(reason, func(t *testing.T) {
			const token = "dead11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff001122"
			devices := &fakeDevices{tokens: []string{token}}
			status := http.StatusGone
			if reason == "BadDeviceToken" {
				status = http.StatusBadRequest
			}
			sender, _ := newTestSender(t, devices, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"reason":"` + reason + `"}`))
			})
			sender.deliver(context.Background(), delivery{personID: uuid.New(), note: Notification{Title: "t"}})

			if len(devices.deleted) != 1 || devices.deleted[0] != token {
				t.Errorf("a %s token should be pruned, deleted=%v", reason, devices.deleted)
			}
		})
	}
}

// TestSenderKeepsTokensOnATransientFailure is the other half, and the one that costs
// something to get wrong: a 429 or 503 is Apple asking for patience, and deleting a live
// token over one silently stops a working device forever.
func TestSenderKeepsTokensOnATransientFailure(t *testing.T) {
	const token = "beef11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff001122"
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		devices := &fakeDevices{tokens: []string{token}}
		sender, _ := newTestSender(t, devices, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"reason":"TooManyRequests"}`))
		})
		sender.deliver(context.Background(), delivery{personID: uuid.New(), note: Notification{Title: "t"}})
		if len(devices.deleted) != 0 {
			t.Errorf("status %d must not retire a token, deleted=%v", status, devices.deleted)
		}
	}
}

// TestSenderReusesItsAuthToken — Apple rate-limits an app that mints a provider token
// per push, so the token is cached between sends.
func TestSenderReusesItsAuthToken(t *testing.T) {
	const token = "cafe11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff001122"
	devices := &fakeDevices{tokens: []string{token}}
	var mu sync.Mutex
	var seen []string
	sender, _ := newTestSender(t, devices, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	for i := 0; i < 3; i++ {
		sender.deliver(context.Background(), delivery{personID: uuid.New(), note: Notification{Title: "t"}})
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 sends, got %d", len(seen))
	}
	if seen[0] != seen[1] || seen[1] != seen[2] {
		t.Errorf("the provider token should be reused between sends, got %d distinct", len(seen))
	}
}

// TestNotifyNeverBlocks — a full queue drops the notification rather than holding up the
// request that produced it. An invitation must be created whether or not Apple is
// reachable, and /me/invitations remains the authority on what was offered.
func TestNotifyNeverBlocks(t *testing.T) {
	sender := NewSender(&fakeDevices{}, Config{BundleID: "x", PrivateKey: testKey(t)})
	// Nothing is draining the queue, so this fills it and then discards.
	for i := 0; i < queueDepth+10; i++ {
		sender.Notify(context.Background(), uuid.New(), Notification{Title: "t"})
	}
	if got := len(sender.queue); got != queueDepth {
		t.Errorf("queue should cap at %d, got %d", queueDepth, got)
	}
}
