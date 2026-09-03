package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/monsoudz/soccerkit-api/internal/api"
	"github.com/monsoudz/soccerkit-api/internal/config"
	"github.com/monsoudz/soccerkit-api/internal/database"
)

var (
	testServer *httptest.Server
	testPool   *pgxpool.Pool
	// testRouter is the same handler testServer wraps, kept as a chi router so
	// TestOpenAPISpecMatchesTheRouter can walk the routes it mounts.
	testRouter http.Handler
)

func TestMain(m *testing.M) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://postgres:postgres@localhost:5432/soccerkit_test?sslmode=disable"
	}
	os.Setenv("ENV", "test") // ENV is required now; it has no default
	os.Setenv("DATABASE_URL", dbURL)
	os.Setenv("JWT_ACCESS_SECRET", "test-access-secret")
	os.Setenv("DEV_APPLE_BYPASS", "true") // accept crafted identity tokens in tests

	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	testPool, err = database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		panic("connect test db: " + err.Error())
	}
	if err := database.Migrate(ctx, testPool); err != nil {
		panic("migrate test db: " + err.Error())
	}

	srv := api.NewServer(cfg, testPool)
	testRouter = srv.Router()
	testServer = httptest.NewServer(testRouter)

	code := m.Run()

	testServer.Close()
	testPool.Close()
	os.Exit(code)
}

// resetDB truncates all tables so each test starts clean.
func resetDB(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `
		TRUNCATE TABLE
			sync_documents, players, events, diagrams,
			form_answers, form_instances, form_fields, form_templates,
			share_grants, session_blocks, sessions, drills, games,
			roster_memberships, teams, guardianships, memberships,
			refresh_tokens, user_accounts, persons, organizations
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset db: %v", err)
	}
	// Rewind the sync cursor sequence so cursors are deterministic per test.
	if _, err := testPool.Exec(context.Background(), `ALTER SEQUENCE sync_seq RESTART WITH 1`); err != nil {
		t.Fatalf("reset sync_seq: %v", err)
	}
}

// --- HTTP helpers ---------------------------------------------------------

type resp struct {
	status int
	body   map[string]any
	raw    []byte
}

func (r resp) arr() []any {
	var a []any
	_ = json.Unmarshal(r.raw, &a)
	return a
}

func do(t *testing.T, method, path, token string, payload any) resp {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, testServer.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// newRecorder is httptest's recorder, used by the few tests that drive the router
// directly rather than through the shared test server.
func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// signInCoach creates (or resumes) a coach the only way anyone gets an account — Sign
// in with Apple — and returns (accessToken, personID). The Apple subject is derived from
// the address so each test coach is distinct and stable across calls, and the identity
// token is unsigned, which the server accepts because the harness sets
// DEV_APPLE_BYPASS=true.
//
// This used to POST /auth/register. Registration and password login are gone: nothing
// shipped used them, and they were an unauthenticated way to create an account at any
// address (see docs/AUDIT-3.md C1 and L5).
func signInCoach(t *testing.T, email string) (string, string) {
	t.Helper()
	r := appleSignIn(t, "sub-"+email, email, email)
	if r.status != http.StatusOK {
		t.Fatalf("sign in %s: status %d body %s", email, r.status, r.raw)
	}
	token, _ := r.body["token"].(string)
	id, _ := r.body["personID"].(string)
	if token == "" || id == "" {
		t.Fatalf("sign in %s returned no session: %s", email, r.raw)
	}
	return token, id
}
