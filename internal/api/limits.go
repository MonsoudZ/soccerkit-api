package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// Request limits. None of these existed: an unauthenticated caller could POST an
// unbounded body, hammer the credential endpoints, or push a sync batch of any size
// into a single transaction.
const (
	// maxBodyBytes caps any request body. The largest legitimate body is a sync push,
	// which is JSON payloads mirrored from a phone.
	maxBodyBytes = 4 << 20 // 4 MiB

	// maxSyncBatch caps the records in one push. Each is a statement inside one
	// transaction, so an unbounded batch holds it open for as long as it takes.
	maxSyncBatch = 1000

	// maxSyncPage caps the records in one pull, and maxSyncPageBytes caps what they
	// weigh. A pull used to return the whole delta: nothing limits how many pushes an
	// account makes, so its delta grows without bound and every since=0 pull — what a
	// reinstall sends — became an unbounded allocation.
	//
	// Both limits are needed. Rows alone do not bound the response, because a single
	// payload may be as large as a push body allows; bytes alone do not bound the work,
	// because the query still has to produce the rows. Neither is a wire change: the
	// client resumes from the cursor until it stops moving.
	maxSyncPage      = 500
	maxSyncPageBytes = 2 << 20 // 2 MiB

	// authRate is the sustained requests-per-minute allowed per client IP on the
	// credential endpoints, and authBurst how many may arrive at once. A coach signing
	// in with Apple, or a device renewing a session, stays far below this.
	authRate  = 20
	authBurst = 10
)

// limitBody caps the request body for every route. MaxBytesReader makes the overrun
// surface as a read error, which decodeJSON already reports as a 400.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// ipRateLimiter is a token bucket per client IP.
//
// It is deliberately in-process: this service runs as a single instance today, and a
// shared-state limiter would mean a new dependency and a Redis to run it against. That
// makes it a per-instance limit, so it is a brake on brute force rather than a quota —
// if this is ever run behind more than one replica the budget multiplies by the replica
// count, and it should move to shared state.
type ipRateLimiter struct {
	rate   float64 // tokens per second
	burst  float64
	mu     sync.Mutex
	perIP  map[string]*bucket
	lastGC time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newIPRateLimiter(perMinute, burst int) *ipRateLimiter {
	return &ipRateLimiter{
		rate:   float64(perMinute) / 60,
		burst:  float64(burst),
		perIP:  make(map[string]*bucket),
		lastGC: time.Now(),
	}
}

// allow reports whether this key has a token to spend, refilling first.
func (l *ipRateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Drop buckets nobody has touched for a while, so the map cannot grow without
	// bound on a stream of distinct source addresses.
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.perIP {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.perIP, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.perIP[key]
	if !ok {
		b = &bucket{tokens: l.burst, seen: now}
		l.perIP[key] = b
	}
	b.tokens += now.Sub(b.seen).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// middleware throttles by client IP, as resolved by the ClientIPFrom* middleware
// the router mounts (see Server.clientIP).
func (l *ipRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "60")
			writeError(w, &apiError{http.StatusTooManyRequests, "RATE_LIMITED",
				"Too many attempts. Try again in a minute."})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// unidentifiedClient is the bucket every request whose address could not be
// established shares. That is deliberately one bucket rather than none: an
// unattributable request must still be counted, and sharing a bucket is the
// restrictive reading, not the permissive one.
const unidentifiedClient = "unidentified"

// clientIP is the rate-limit key: the address chi's ClientIPFrom* middleware
// established, already normalized (port stripped, v4-mapped IPv6 folded to v4,
// zone removed) so one client is one bucket however they connect.
func clientIP(r *http.Request) string {
	if ip := middleware.GetClientIP(r.Context()); ip != "" {
		return ip
	}
	return unidentifiedClient
}

// hasAuthLimiter reports whether credential endpoints are throttled, so the router's
// decision is expressed in one place and can be asserted in a test.
func (s *Server) hasAuthLimiter() bool { return s.cfg.IsDeployed() }
