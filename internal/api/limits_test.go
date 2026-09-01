package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	h := l.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}))

	call := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
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

func TestClientIPIgnoresEphemeralPort(t *testing.T) {
	for addr, want := range map[string]string{
		"203.0.113.7:41234":  "203.0.113.7",
		"[2001:db8::1]:8080": "[2001:db8::1]",
		"no-port":            "no-port",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		if got := clientIP(req); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", addr, got, want)
		}
	}
}
