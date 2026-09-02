package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// TestSignInCreatesIdentityGraph — a first Sign in with Apple provisions the whole
// identity: a Person, an account, a personal organization, and the three top roles in
// it. This is the only path that creates an account; email+password registration is
// gone (docs/AUDIT-3.md C1, L5).
func TestSignInCreatesIdentityGraph(t *testing.T) {
	resetDB(t)

	r := appleSignIn(t, "graph-sub", "Coach@Example.com", "Coach Kim")
	if r.status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", r.status, r.raw)
	}
	if r.body["token"] == nil || r.body["refreshToken"] == nil {
		t.Error("expected a session")
	}
	token, _ := r.body["token"].(string)

	me := do(t, http.MethodGet, "/api/v1/me", token, nil)
	if me.status != http.StatusOK {
		t.Fatalf("/me: %d %s", me.status, me.raw)
	}
	person := me.body["person"].(map[string]any)
	if person["displayName"] != "Coach Kim" {
		t.Errorf("unexpected person: %v", person)
	}
	// Personal org with admin+director+coach memberships.
	memberships := me.body["memberships"].([]any)
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
}

func TestNewAccountSeedsTemplates(t *testing.T) {
	resetDB(t)
	token, _ := signInCoach(t, "seeded@e.com")

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

func TestRefreshRotation(t *testing.T) {
	resetDB(t)
	r := appleSignIn(t, "rotate-sub", "log@e.com", nil)
	refresh := r.body["refreshToken"].(string)

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
	r := appleSignIn(t, "hashed-sub", "hashed@e.com", nil)
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
	r := appleSignIn(t, "replay-sub", "replay@e.com", nil)
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
	if again := appleSignIn(t, "replay-sub", "replay@e.com", nil); again.status != http.StatusOK {
		t.Errorf("signing in again after the cascade: %d %s", again.status, again.raw)
	}
}

// TestRefreshRetryWithinGraceDoesNotCascade — this backs an offline-first phone app, so
// a refresh whose response was lost gets retried with the same token. That must cost
// one failed retry, not every session on every device.
func TestRefreshRetryWithinGraceDoesNotCascade(t *testing.T) {
	resetDB(t)
	r := appleSignIn(t, "retry-sub", "retry@e.com", nil)
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
	first := appleSignIn(t, "twodevices-sub", "twodevices@e.com", nil)
	deviceA, _ := first.body["refreshToken"].(string)

	// The same coach signs in on a second device: same Apple subject, new session.
	second := appleSignIn(t, "twodevices-sub", "twodevices@e.com", nil)
	deviceB, _ := second.body["refreshToken"].(string)
	if deviceB == "" || deviceB == deviceA {
		t.Fatalf("the second device should get its own token: %s", second.raw)
	}

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

// TestConcurrentFirstSignInsResolveToOneAccount — provisioning races itself when a
// coach's first sign-in arrives twice at once (a second device, or a client re-sending
// a request whose response it lost). The loser fails on the winner's committed rows,
// and on the Person id in particular that failure is indistinguishable from the
// pre-claimed row CreatePersonWithID exists to refuse. Both must resolve to one
// account, and both callers must end up signed in to it.
func TestConcurrentFirstSignInsResolveToOneAccount(t *testing.T) {
	resetDB(t)
	const n = 4

	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	persons := map[string]bool{}
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := appleSignIn(t, "concurrent-sub", "concurrent@e.com", nil)
			id, _ := r.body["personID"].(string)
			mu.Lock()
			statuses[r.status]++
			if id != "" {
				persons[id] = true
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if statuses[http.StatusOK] != n {
		t.Errorf("every concurrent first sign-in should succeed, got %v", statuses)
	}
	if len(persons) != 1 {
		t.Errorf("they must all resolve to one Person, got %d distinct: %v", len(persons), persons)
	}
	if got := countRows(t, `SELECT count(*) FROM user_accounts`); got != 1 {
		t.Errorf("expected exactly one account, found %d", got)
	}
	if got := countRows(t, `SELECT count(*) FROM organizations`); got != 1 {
		t.Errorf("expected exactly one organization, found %d", got)
	}
}

// TestConcurrentSignInsAtOneAddressStayATypedConflict — the recheck that resolves a
// provisioning race asks who owns the *subject*. When the collision is on the address
// instead, the winner holds a different subject, the recheck finds nothing, and the raw
// unique violation used to fall through to a 500 — while the very same request made a
// moment later answers 409. Timing must not change the answer.
//
// Two Apple IDs do not share a verified address, so this is not reachable through Apple;
// it is here because an unmapped error reaching the caller as a 500 is the defect
// docs/AUDIT-3.md M3, L1 and L2 were all instances of, and the fix for C1 put one back.
func TestConcurrentSignInsAtOneAddressStayATypedConflict(t *testing.T) {
	resetDB(t)
	const email = "shared@example.com"

	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	codes := map[string]int{}
	start := make(chan struct{})
	for _, sub := range []string{"sub-one", "sub-two", "sub-three", "sub-four"} {
		wg.Add(1)
		go func(sub string) {
			defer wg.Done()
			<-start
			r := appleSignIn(t, sub, email, nil)
			code, _ := errCode(r)
			mu.Lock()
			statuses[r.status]++
			if code != "" {
				codes[code]++
			}
			mu.Unlock()
		}(sub)
	}
	close(start)
	wg.Wait()

	if statuses[http.StatusInternalServerError] != 0 {
		t.Errorf("a racing sign-in reported a server fault: %v", statuses)
	}
	if statuses[http.StatusOK] != 1 {
		t.Errorf("exactly one subject should get the address, got %v", statuses)
	}
	if got := statuses[http.StatusConflict]; got != 3 {
		t.Errorf("the other three should be told the address is taken, got %v", statuses)
	}
	if codes["EMAIL_ALREADY_REGISTERED"] != 3 {
		t.Errorf("and told it with the code the sequential path uses, got %v", codes)
	}
}

// TestOneRefreshTokenRedeemsOnce pins rotation's single-use invariant against
// concurrency. handleRefresh used to read the row, decide it was live, and revoke it in
// a separate unconditional statement — a check-then-act that simultaneous
// presentations of one token all passed before any of them wrote. Measured against that
// code: 32 concurrent redemptions of a single token produced six independent live
// families, and none of them tripped the replay cascade, which is precisely the case
// the cascade exists for.
func TestOneRefreshTokenRedeemsOnce(t *testing.T) {
	resetDB(t)

	reg := appleSignIn(t, "race-sub", "race@example.com", nil)
	if reg.status != http.StatusOK {
		t.Fatalf("sign in: %d %s", reg.status, reg.raw)
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
