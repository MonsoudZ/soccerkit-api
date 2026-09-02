package api_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/monsoudz/soccerkit-api/internal/api"
	"github.com/monsoudz/soccerkit-api/internal/config"
)

// TestHealthAnswersLivenessAndReadyAnswersReachability — /health used to be the only
// probe, and it reports on the process rather than on whether the process can do
// anything: a database that went away after boot left it saying "ok" while every request
// returned 500.
func TestHealthAnswersLivenessAndReadyAnswersReachability(t *testing.T) {
	if r := do(t, http.MethodGet, "/health", "", nil); r.status != http.StatusOK {
		t.Errorf("/health: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodGet, "/ready", "", nil); r.status != http.StatusOK {
		t.Fatalf("/ready with a working database: %d %s", r.status, r.raw)
	} else if r.body["status"] != "ready" {
		t.Errorf("/ready body: %s", r.raw)
	}
}

// TestReadyReportsAnUnreachableDatabase builds a server whose pool points nowhere.
// pgxpool connects lazily, so this is the shape of a process whose database has gone
// away underneath it — the case database.Connect's boot-time ping cannot cover.
func TestReadyReportsAnUnreachableDatabase(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgresql://nobody:nobody@127.0.0.1:1/nowhere?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("build a pool: %v", err)
	}
	defer pool.Close()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := api.NewServer(cfg, pool)

	req, _ := http.NewRequest(http.MethodGet, "/ready", nil)
	rec := newRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/ready with an unreachable database: got %d, want 503 (body %s)",
			rec.Code, rec.Body.String())
	}

	// Liveness must not follow it down: the process is fine, its dependency is not, and
	// restarting it would not help.
	livenessReq, _ := http.NewRequest(http.MethodGet, "/health", nil)
	livenessRec := newRecorder()
	srv.Router().ServeHTTP(livenessRec, livenessReq)
	if livenessRec.Code != http.StatusOK {
		t.Errorf("/health should still report the process alive, got %d", livenessRec.Code)
	}
}

// TestDocsPinsSwaggerUIWithIntegrity — /docs used to load swagger-ui-dist@5 from a
// third-party CDN: a floating major with no integrity attribute, so whatever that URL
// returned ran on this origin with this origin's privileges. See docs/AUDIT-2.md L1.
func TestDocsPinsSwaggerUIWithIntegrity(t *testing.T) {
	r := do(t, http.MethodGet, "/docs", "", nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /docs: %d", r.status)
	}
	page := string(r.raw)

	if strings.Contains(page, "swagger-ui-dist@5/") {
		t.Error("/docs still asks for a floating major version")
	}
	if n := strings.Count(page, "integrity=\"sha384-"); n != 2 {
		t.Errorf("expected an SRI hash on both the stylesheet and the script, found %d", n)
	}
	// SRI is only enforced on a cross-origin fetch when the request is anonymous —
	// without this the browser cannot read the response to check it, and skips the
	// check rather than failing.
	if n := strings.Count(page, `crossorigin="anonymous"`); n != 2 {
		t.Errorf("both subresources need crossorigin=anonymous for SRI to be enforced, found %d", n)
	}
	// An exact version, not a range: asserted by shape so bumping the pin does not
	// break this, but going back to a floating tag does.
	pinned := regexp.MustCompile(`swagger-ui-dist@\d+\.\d+\.\d+/`)
	if n := len(pinned.FindAllString(page, -1)); n != 2 {
		t.Errorf("both subresources must name an exact version, found %d", n)
	}
}
