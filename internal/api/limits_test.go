package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

func TestRateLimiterSpendsAndRefills(t *testing.T) {
	l := newIPRateLimiter(60, 3) // 1/sec sustained, 3 at once
	start := time.Now()

	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4", start) {
			t.Fatalf("burst request %d should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4", start) {
		t.Error("a fourth simultaneous request should be refused")
	}
	// A different caller has their own bucket.
	if !l.allow("5.6.7.8", start) {
		t.Error("an unrelated IP must not be affected")
	}
	// One token per second comes back.
	if !l.allow("1.2.3.4", start.Add(time.Second)) {
		t.Error("a token should have refilled after a second")
	}
	if l.allow("1.2.3.4", start.Add(time.Second)) {
		t.Error("only one token should have refilled")
	}
	// Refill is capped at the burst size rather than accruing forever.
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4", start.Add(time.Hour)) {
			t.Fatalf("request %d after a long idle should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4", start.Add(time.Hour)) {
		t.Error("idle time must not accrue more than the burst")
	}
}

func TestRateLimiterMiddlewareReturns429(t *testing.T) {
	l := newIPRateLimiter(60, 1)
	var served int
	h := middleware.ClientIPFromRemoteAddr(l.middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			served++
			w.WriteHeader(http.StatusOK)
		})))

	call := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/apple", nil)
		req.RemoteAddr = "9.9.9.9:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("first call: got %d, want 200", got)
	}
	if got := call(); got != http.StatusTooManyRequests {
		t.Fatalf("second call: got %d, want 429", got)
	}
	if served != 1 {
		t.Errorf("the throttled request reached the handler anyway (served=%d)", served)
	}
}

// The limiter used to key on r.RemoteAddr after middleware.RealIP had overwritten
// it from a request header, so a caller who varied X-Forwarded-For got a fresh
// bucket every time and the credential endpoints were unthrottled in practice.
// With no trusted proxy configured, the forwarding headers must not move the key
// off the TCP peer at all.
func TestForwardingHeadersCannotChooseTheBucket(t *testing.T) {
	h := middleware.ClientIPFromRemoteAddr(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if got := clientIP(r); got != "198.51.100.4" {
				t.Errorf("clientIP = %q, want the TCP peer 198.51.100.4", got)
			}
		}))

	for _, header := range []string{"X-Forwarded-For", "X-Real-IP", "True-Client-IP"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/apple", nil)
		req.RemoteAddr = "198.51.100.4:41234"
		req.Header.Set(header, "203.0.113.99")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// Behind a declared proxy the client is the first X-Forwarded-For entry outside
// the trusted ranges, walking from the right. Entries to the left of that are
// whatever the caller chose to send and must not become the key.
func TestTrustedProxyTakesTheFirstUntrustedHop(t *testing.T) {
	var got string
	h := middleware.ClientIPFromXFF("10.0.0.0/8")(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { got = clientIP(r) }))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/apple", nil)
	req.RemoteAddr = "10.0.0.7:41234"
	// The caller forged the first entry; our load balancer appended the real one.
	req.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.4, 10.0.0.7")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != "198.51.100.4" {
		t.Errorf("clientIP = %q, want 198.51.100.4 (the hop our proxy vouches for)", got)
	}
}

// A request whose address cannot be established still has to be counted, so it
// gets a bucket rather than a pass.
func TestUnidentifiedClientStillGetsABucket(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/apple", nil)
	if got := clientIP(req); got != unidentifiedClient {
		t.Errorf("clientIP with no middleware = %q, want %q", got, unidentifiedClient)
	}
}
