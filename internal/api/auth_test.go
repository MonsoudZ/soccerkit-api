package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestRegisterCreatesIdentityGraph(t *testing.T) {
	resetDB(t)

	r := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "Coach@Example.com", "password": "password123", "displayName": "Coach Kim",
	})
	if r.status != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", r.status, r.raw)
	}
	me := r.body["me"].(map[string]any)
	person := me["person"].(map[string]any)
	if person["displayName"] != "Coach Kim" {
		t.Errorf("unexpected person: %v", person)
	}
	// Personal org with admin+director+coach memberships.
	memberships := me["memberships"].([]any)
	if len(memberships) != 3 {
		t.Fatalf("expected 3 memberships (admin/director/coach), got %d", len(memberships))
	}
	roles := map[string]bool{}
	for _, m := range memberships {
		roles[m.(map[string]any)["role"].(string)] = true
	}
	for _, want := range []string{"admin", "director", "coach"} {
		if !roles[want] {
			t.Errorf("missing role %q", want)
		}
	}
	if r.body["accessToken"] == nil || r.body["refreshToken"] == nil {
		t.Error("expected tokens")
	}
}

func TestRegisterSeedsTemplates(t *testing.T) {
	resetDB(t)
	token, _ := registerUser(t, "seeded@e.com")

	list := do(t, http.MethodGet, "/api/v1/templates", token, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list templates: %d %s", list.status, list.raw)
	}
	templates := list.arr()
	if len(templates) < 2 {
		t.Fatalf("expected seeded pre/post-game templates, got %d", len(templates))
	}
	contexts := map[string]bool{}
	for _, tpl := range templates {
		contexts[tpl.(map[string]any)["context"].(string)] = true
	}
	if !contexts["pre_game"] || !contexts["post_game"] {
		t.Errorf("expected pre_game and post_game seed templates, got %v", contexts)
	}
}

func TestDuplicateEmailAndValidation(t *testing.T) {
	resetDB(t)
	payload := map[string]any{"email": "dup@e.com", "password": "password123", "displayName": "Dup"}
	do(t, http.MethodPost, "/api/v1/auth/register", "", payload)

	if r := do(t, http.MethodPost, "/api/v1/auth/register", "", payload); r.status != http.StatusConflict {
		t.Errorf("expected 409 on duplicate, got %d", r.status)
	}
	bad := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "notanemail", "password": "short", "displayName": "Z",
	})
	if bad.status != http.StatusBadRequest {
		t.Errorf("expected 400 on invalid input, got %d", bad.status)
	}
}

func TestLoginAndRefreshRotation(t *testing.T) {
	resetDB(t)
	reg := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "log@e.com", "password": "password123", "displayName": "Log",
	})
	refresh := reg.body["refreshToken"].(string)

	if ok := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "log@e.com", "password": "password123",
	}); ok.status != http.StatusOK || ok.body["accessToken"] == nil {
		t.Fatalf("valid login failed: %d %s", ok.status, ok.raw)
	}
	if bad := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "log@e.com", "password": "wrongpassword",
	}); bad.status != http.StatusUnauthorized {
		t.Errorf("expected 401 on bad password, got %d", bad.status)
	}

	first := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh})
	if first.status != http.StatusOK || first.body["refreshToken"] == refresh {
		t.Fatalf("refresh should rotate: %d %s", first.status, first.raw)
	}
	if reuse := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh}); reuse.status != http.StatusUnauthorized {
		t.Errorf("reusing rotated token should 401, got %d", reuse.status)
	}
}

func TestProtectedRouteRequiresAuth(t *testing.T) {
	resetDB(t)
	if r := do(t, http.MethodGet, "/api/v1/me", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", r.status)
	}
}

// TestRefreshTokensAreStoredHashed — the column held the token verbatim, so a copy of
// the database was a set of working credentials for every account.
func TestRefreshTokensAreStoredHashed(t *testing.T) {
	resetDB(t)
	r := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "hashed@e.com", "password": "password123", "displayName": "H",
	})
	refresh, _ := r.body["refreshToken"].(string)
	if refresh == "" {
		t.Fatal("no refresh token issued")
	}

	var stored string
	if err := testPool.QueryRow(context.Background(),
		`SELECT token_hash FROM refresh_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == refresh {
		t.Fatal("the refresh token is stored in plaintext")
	}
	// The stored value must still identify the token, or refresh would not work.
	if got := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": refresh,
	}); got.status != http.StatusOK {
		t.Fatalf("hashed lookup broke refresh: %d %s", got.status, got.raw)
	}
}

// TestReplayedRefreshTokenRevokesTheFamily — rotation already refused a replayed
// token, but left the live chain valid for whoever held it. A replay is evidence the
// chain leaked, so the whole family goes.
func TestReplayedRefreshTokenRevokesTheFamily(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	r := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "replay@e.com", "password": "password123", "displayName": "R",
	})
	stolen, _ := r.body["refreshToken"].(string)

	rotated := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refreshToken": stolen})
	if rotated.status != http.StatusOK {
		t.Fatalf("first refresh: %d %s", rotated.status, rotated.raw)
	}
	live, _ := rotated.body["refreshToken"].(string)

	// Age the rotation past the retry grace window, so this reads as a replay rather
	// than a client re-sending a request whose response it lost.
	if _, err := testPool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() - interval '5 minutes'
		  WHERE revoked_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}

	if replay := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": stolen,
	}); replay.status != http.StatusUnauthorized {
		t.Fatalf("replay: got %d, want 401", replay.status)
	}

	// The legitimate chain is now dead too — both parties must sign in again.
	if after := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": live,
	}); after.status != http.StatusUnauthorized {
		t.Errorf("the live chain should be revoked after a replay, got %d %s", after.status, after.raw)
	}

	// Signing in fresh still works.
	if login := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "replay@e.com", "password": "password123",
	}); login.status != http.StatusOK {
		t.Errorf("re-login after the cascade: %d %s", login.status, login.raw)
	}
}

// TestRefreshRetryWithinGraceDoesNotCascade — this backs an offline-first phone app, so
// a refresh whose response was lost gets retried with the same token. That must cost
// one failed retry, not every session on every device.
func TestRefreshRetryWithinGraceDoesNotCascade(t *testing.T) {
	resetDB(t)
	r := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "retry@e.com", "password": "password123", "displayName": "T",
	})
	first, _ := r.body["refreshToken"].(string)

	rotated := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refreshToken": first})
	live, _ := rotated.body["refreshToken"].(string)

	// The client retries the request it thinks failed, immediately.
	if retry := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": first,
	}); retry.status != http.StatusUnauthorized {
		t.Fatalf("retry: got %d, want 401", retry.status)
	}
	// Its real session survives.
	if after := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": live,
	}); after.status != http.StatusOK {
		t.Errorf("a prompt retry must not revoke the live chain, got %d %s", after.status, after.raw)
	}
}

// TestLogoutDoesNotCascadeToOtherDevices — logout deletes its row rather than revoking
// it, so a signed-out token can never be mistaken for a rotation replay.
func TestLogoutDoesNotCascadeToOtherDevices(t *testing.T) {
	resetDB(t)
	reg := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "twodevices@e.com", "password": "password123", "displayName": "D",
	})
	deviceA, _ := reg.body["refreshToken"].(string)

	login := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "twodevices@e.com", "password": "password123",
	})
	deviceB, _ := login.body["refreshToken"].(string)

	if out := do(t, http.MethodPost, "/api/v1/auth/logout", "", map[string]any{
		"refreshToken": deviceA,
	}); out.status != http.StatusOK {
		t.Fatalf("logout: %d %s", out.status, out.raw)
	}
	// Device A's token is dead, and the app retrying it must not take B down.
	do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refreshToken": deviceA})
	if b := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": deviceB,
	}); b.status != http.StatusOK {
		t.Errorf("signing out one device must not sign out the others, got %d %s", b.status, b.raw)
	}
}

// TestLoginDoesNotRevealWhichEmailsExist — login returned immediately on an unknown
// address and spent a bcrypt comparison on a known one, and that difference is
// measurable, so it told an attacker which addresses have accounts.
func TestLoginDoesNotRevealWhichEmailsExist(t *testing.T) {
	resetDB(t)
	registerUser(t, "known@e.com")

	timeLogin := func(email string) time.Duration {
		start := time.Now()
		r := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
			"email": email, "password": "definitely-wrong-password",
		})
		if r.status != http.StatusUnauthorized {
			t.Fatalf("login %s: got %d, want 401", email, r.status)
		}
		return time.Since(start)
	}
	// Warm the lazily-built dummy hash so it is not charged to the first call.
	timeLogin("unknown@e.com")

	known, unknown := timeLogin("known@e.com"), timeLogin("unknown@e.com")
	ratio := float64(known) / float64(unknown)
	if ratio > 3 || ratio < 0.33 {
		t.Errorf("login timing differs by %.1fx between a known and unknown address "+
			"(known %v, unknown %v) — that is an enumeration oracle", ratio, known, unknown)
	}
}

// TestConcurrentRegistrationConflicts — the existence check races the insert, and the
// loser hit the unique constraint and surfaced as a 500.
func TestConcurrentRegistrationConflicts(t *testing.T) {
	resetDB(t)
	const n = 4
	body := map[string]any{"email": "race@e.com", "password": "password123", "displayName": "Race"}

	statuses := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- do(t, http.MethodPost, "/api/v1/auth/register", "", body).status
		}()
	}
	wg.Wait()
	close(statuses)

	created, conflict := 0, 0
	for st := range statuses {
		switch st {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d — a duplicate registration should be 409", st)
		}
	}
	if created != 1 {
		t.Errorf("exactly one registration should succeed, got %d", created)
	}
	if conflict != n-1 {
		t.Errorf("the rest should be 409, got %d", conflict)
	}
}

// TestOneRefreshTokenRedeemsOnce pins rotation's single-use invariant against
// concurrency. handleRefresh used to read the row, decide it was live, and revoke it in
// a separate unconditional statement — a check-then-act that simultaneous presentations
// of one token all passed before any of them wrote. Measured against that code: 32
// concurrent redemptions of a single token produced six independent live families, and
// none of them tripped the replay cascade, which is precisely the case the cascade
// exists for.
func TestOneRefreshTokenRedeemsOnce(t *testing.T) {
	resetDB(t)

	reg := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "race@example.com", "password": "password123", "displayName": "Race",
	})
	if reg.status != http.StatusCreated {
		t.Fatalf("register: %d %s", reg.status, reg.raw)
	}
	refresh, _ := reg.body["refreshToken"].(string)

	const attempts = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var issued []string
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
				"refreshToken": refresh,
			})
			if res.status == http.StatusOK {
				tok, _ := res.body["refreshToken"].(string)
				mu.Lock()
				issued = append(issued, tok)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(issued) != 1 {
		t.Fatalf("%d of %d concurrent redemptions of one token succeeded; a refresh token "+
			"is single-use, so exactly one must win", len(issued), attempts)
	}
	// The one that won is a working chain — refusing them all would also satisfy the
	// count above and would log the coach out for refreshing twice.
	next := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": issued[0],
	})
	if next.status != http.StatusOK {
		t.Errorf("the surviving chain should still rotate: %d %s", next.status, next.raw)
	}
}
