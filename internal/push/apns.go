// Package push delivers notifications to a person's registered devices.
//
// One provider today, Apple's, because there is one client. The seam that matters is not
// between providers but between "something happened" and "somebody was told": the API
// records the event and hands it here, and nothing in a request path waits for Apple or
// fails because of them.
package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Production and development APNs hosts. Which one a token is valid for depends on how
// the app was built, and a token from one is rejected by the other -- a sandbox build
// pointed at production is the usual cause of BadDeviceToken on a token that looks fine.
const (
	ProductionHost = "https://api.push.apple.com"
	SandboxHost    = "https://api.sandbox.push.apple.com"
)

// Config is what Apple needs to believe a push came from this app.
type Config struct {
	KeyID      string
	TeamID     string
	BundleID   string
	PrivateKey *ecdsa.PrivateKey
	Host       string
}

// Notification is one message, in the terms the sender cares about.
type Notification struct {
	Title string
	Body  string
	// Data rides alongside the alert so the app can act on the tap rather than just
	// opening. Keys are flattened into the top level of the APNs payload, beside "aps".
	Data map[string]string
}

// Sender delivers notifications to Apple, off the request path.
//
// Notify never blocks and never fails: a club's invitation must be created whether or
// not Apple is reachable, and a caller who could see the difference would be reporting
// Apple's health as their own. Deliveries are queued and drained by a worker; if the
// queue is full the notification is dropped with a log line, which is the right trade
// for a message whose whole purpose is convenience -- /me/invitations remains the
// authority on what someone has been offered.
// Devices is the slice of the store this package needs: where to reach someone, and how
// to forget a token Apple has rejected.
//
// An interface rather than *store.Queries so the delivery logic can be exercised without
// a database -- the parts worth testing here are the payload, the headers and which
// rejections prune a token, none of which are questions about Postgres. *store.Queries
// satisfies it as written.
type Devices interface {
	ListDeviceTokensForPerson(ctx context.Context, personID uuid.UUID) ([]string, error)
	DeleteDeviceTokenAnyOwner(ctx context.Context, token string) error
}

type Sender struct {
	store  Devices
	cfg    Config
	client *http.Client
	queue  chan delivery

	mu         sync.Mutex
	authToken  string
	authIssued time.Time
}

type delivery struct {
	personID uuid.UUID
	note     Notification
}

// queueDepth bounds the backlog. Deep enough to absorb a coach inviting a squad's worth
// of parents in one sitting, shallow enough that a wedged Apple connection cannot grow
// the process without limit.
const queueDepth = 256

// NewSender builds a sender. The caller owns its lifetime: nothing is delivered until
// Run is called, and Run returns when its context is cancelled.
func NewSender(q Devices, cfg Config) *Sender {
	if cfg.Host == "" {
		cfg.Host = ProductionHost
	}
	return &Sender{
		store: q,
		cfg:   cfg,
		// APNs speaks HTTP/2 only. net/http negotiates it over ALPN for https with the
		// default transport, so no explicit http2 wiring is needed -- but the timeout is,
		// because a stalled connection to Apple must not hold a worker forever.
		client: &http.Client{Timeout: 15 * time.Second},
		queue:  make(chan delivery, queueDepth),
	}
}

// Notify queues a notification for every device the person has registered.
//
// The signature returns nothing on purpose. There is no answer a caller could act on:
// the person may have no devices, Apple may be down, the token may have been revoked
// since it was registered, and none of those make the thing that just happened not have
// happened.
func (s *Sender) Notify(_ context.Context, personID uuid.UUID, note Notification) {
	select {
	case s.queue <- delivery{personID: personID, note: note}:
	default:
		log.Printf("push: queue full, dropping notification %q for person %s", note.Title, personID)
	}
}

// Run drains the queue until ctx is cancelled. One worker: the volume is a handful of
// messages per invitation, and serialising them keeps the auth token refresh trivially
// correct without a second lock around it.
func (s *Sender) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-s.queue:
			s.deliver(ctx, d)
		}
	}
}

func (s *Sender) deliver(ctx context.Context, d delivery) {
	tokens, err := s.store.ListDeviceTokensForPerson(ctx, d.personID)
	if err != nil {
		log.Printf("push: looking up devices for %s: %v", d.personID, err)
		return
	}
	for _, token := range tokens {
		if err := s.send(ctx, token, d.note); err != nil {
			log.Printf("push: sending to %s: %v", redactToken(token), err)
		}
	}
}

// send posts one notification to one device, and prunes the token when Apple says it is
// dead.
//
// The two rejections worth acting on are Unregistered (410 -- the app was deleted) and
// BadDeviceToken (400 -- the token is not valid for this environment or app). Both mean
// this row will never deliver again, so keeping it only guarantees a failed request per
// notification forever. Every other failure is left alone: a 429 or a 503 is Apple
// asking for patience, and deleting a live token over one would silently stop a working
// device.
func (s *Sender) send(ctx context.Context, token string, note Notification) error {
	payload, err := buildPayload(note)
	if err != nil {
		return err
	}
	authToken, err := s.bearer()
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(s.cfg.Host, "/") + "/3/device/" + token
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+authToken)
	req.Header.Set("apns-topic", s.cfg.BundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("content-type", "application/json")

	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	var apnsErr struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &apnsErr)
	if apnsErr.Reason == "Unregistered" || apnsErr.Reason == "BadDeviceToken" {
		if err := s.store.DeleteDeviceTokenAnyOwner(ctx, token); err != nil {
			log.Printf("push: pruning dead token %s: %v", redactToken(token), err)
		} else {
			log.Printf("push: pruned dead token %s (%s)", redactToken(token), apnsErr.Reason)
		}
		return nil
	}
	return fmt.Errorf("apns %d: %s", res.StatusCode, apnsErr.Reason)
}

// buildPayload renders the APNs body. Data keys sit beside "aps" rather than inside it,
// which is where the app reads them from; a key named "aps" would collide, so it is
// dropped rather than allowed to overwrite the alert.
func buildPayload(note Notification) ([]byte, error) {
	body := map[string]any{
		"aps": map[string]any{
			"alert": map[string]any{"title": note.Title, "body": note.Body},
			"sound": "default",
		},
	}
	for k, v := range note.Data {
		if k == "aps" {
			continue
		}
		body[k] = v
	}
	return json.Marshal(body)
}

// bearer returns the provider token, minting a new one when the current one is old.
//
// Apple rejects a token younger than 20 minutes on refresh and one older than an hour
// outright, so the window is narrow at both ends. Fifty minutes sits inside it with room
// for a slow request, and reusing the token in between is not an optimisation -- minting
// one per push is what gets an app rate-limited by Apple.
func (s *Sender) bearer() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authToken != "" && time.Since(s.authIssued) < 50*time.Minute {
		return s.authToken, nil
	}
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": s.cfg.TeamID,
		"iat": now.Unix(),
	})
	tok.Header["kid"] = s.cfg.KeyID
	signed, err := tok.SignedString(s.cfg.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("signing apns token: %w", err)
	}
	s.authToken, s.authIssued = signed, now
	return signed, nil
}

// redactToken keeps a device token out of the logs while leaving enough to correlate
// one. The token is what addresses a person's phone; a log aggregator is not where it
// should be recoverable from.
func redactToken(token string) string {
	if len(token) <= 8 {
		return "…"
	}
	return token[:4] + "…" + token[len(token)-4:]
}
