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
)

func TestMain(m *testing.M) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://postgres:postgres@localhost:5432/soccerkit_test?sslmode=disable"
	}
	os.Setenv("DATABASE_URL", dbURL)
	os.Setenv("JWT_ACCESS_SECRET", "test-access-secret")
	os.Setenv("JWT_REFRESH_SECRET", "test-refresh-secret")

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
	testServer = httptest.NewServer(srv.Router())

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
			player_match_stats, fixtures, league_teams, leagues,
			team_members, teams, match_rsvps, matches, venues,
			refresh_tokens, users
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset db: %v", err)
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

// registerUser creates a user and returns (accessToken, userID).
func registerUser(t *testing.T, email string) (string, string) {
	t.Helper()
	r := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": email, "password": "password123", "displayName": email,
	})
	if r.status != http.StatusCreated {
		t.Fatalf("register %s: status %d body %s", email, r.status, r.raw)
	}
	token, _ := r.body["accessToken"].(string)
	user, _ := r.body["user"].(map[string]any)
	id, _ := user["id"].(string)
	return token, id
}
