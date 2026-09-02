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

	// maxSyncTypeLen and maxSyncIDLen bound the two client-chosen strings that become
	// part of a sync_documents key. Any type outside the seven projected ones becomes a
	// row carrying that type verbatim, and nothing bounded it: the namespace was as wide
	// as a 4 MiB body allowed. See docs/AUDIT-2.md L6.
	//
	// Deliberately a bound and a shape, not an allowlist of known types. The app syncs
	// more types than this service projects and the rest ride as opaque documents on
	// purpose — TestContractUnprojectedTypesRoundTrip exists to stop them being dropped,
	// because a newer app against an older server would lose exactly the types the older
	// server had never heard of. An allowlist would turn that forward compatibility into
	// silent data loss on every app release, which is a worse failure than the one it
	// would prevent.
	maxSyncTypeLen = 64
	maxSyncIDLen   = 128

	// maxSyncRecordBytes caps one record's payload. Nothing capped it: the only limit
	// was maxBodyBytes, so a single record could be just under 4 MiB. That is the
	// remaining half of the paging problem — a pull's row limit and its byte budget only
	// balance at maxSyncPageBytes/maxSyncPage (~4.2 KiB), and the further a payload is
	// above that, the more of each 500-row window is scanned to deliver a handful of
	// rows. At 4 MiB the ratio is ~1000x and a drain of N records costs ~N^2/2r row
	// reads. See docs/AUDIT-5.md M1 (3).
	//
	// 256 KiB is chosen to bound the worst case without rejecting anything real. It puts
	// a ceiling of maxSyncPage*maxSyncRecordBytes = 128 MiB on what one page can read
	// and ~62x on the over-scan ratio, against ~2 GiB and ~1000x before. It is also
	// roughly sixty times the largest payload this service has been observed to store,
	// so no record the app writes today comes close.
	//
	// Tightening it toward 16-32 KiB is what would make a drain close to linear, and
	// that decision wants evidence rather than nerve: it should follow a
	// pg_column_size(payload) percentile over a real database. syncPayloadWatchBytes
	// exists to gather exactly that, without rejecting anything, in the meantime.
	//
	// The reason not to be braver here: an offline-first client retries the batch it
	// failed to push, so a cap that rejects a record a coach already has on their phone
	// stops that device syncing until the record changes. A cap that is too low is a
	// worse failure than the one it prevents.
	maxSyncRecordBytes = 256 << 10 // 256 KiB

	// syncPayloadWatchBytes is a log threshold, not a limit. Records above it are
	// recorded so the real payload-size distribution becomes visible before anybody
	// picks a tighter maxSyncRecordBytes.
	syncPayloadWatchBytes = 32 << 10 // 32 KiB

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
