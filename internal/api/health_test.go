package api_test

import (
	"context"
	"net/http"
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
