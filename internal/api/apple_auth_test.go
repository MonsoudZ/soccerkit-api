package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// devIdentityToken forges an unsigned Apple-style identity token. Accepted only
// because the test server runs with DEV_APPLE_BYPASS=true. email_verified is set
// because Apple sends it on real identity tokens for genuine Apple IDs.
func devIdentityToken(t *testing.T, sub, email string) string {
	t.Helper()
	return devIdentityTokenVerified(t, sub, email, true)
}

func devIdentityTokenVerified(t *testing.T, sub, email string, verified bool) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://appleid.apple.com", "sub": sub, "email": email,
		"email_verified": verified,
	})
	s, err := tok.SignedString([]byte("irrelevant-in-bypass"))
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}
	return s
}

func appleSignIn(t *testing.T, sub, email string, fullName any) resp {
	t.Helper()
	body := map[string]any{"identityToken": devIdentityToken(t, sub, email)}
	if fullName != nil {
		body["fullName"] = fullName
	}
	return do(t, http.MethodPost, "/api/v1/auth/apple", "", body)
}

// TestAppleAuthProvisionsAndReturnsPerson checks that a first Apple sign-in
// provisions a Person and that the returned token authenticates /me.
func TestAppleAuthProvisionsAndReturnsPerson(t *testing.T) {
	resetDB(t)

	r := appleSignIn(t, "apple-sub-new", "coach@example.com", "Sam Coach")
	if r.status != http.StatusOK {
		t.Fatalf("apple auth: status %d body %s", r.status, r.raw)
	}
	token, _ := r.body["token"].(string)
	personID, _ := r.body["personID"].(string)
	if token == "" || personID == "" {
		t.Fatalf("expected token and personID, got %s", r.raw)
	}

	// The token must authenticate a protected route, and /me must be the same person.
	me := do(t, http.MethodGet, "/api/v1/me", token, nil)
	if me.status != http.StatusOK {
		t.Fatalf("/me with apple token: status %d body %s", me.status, me.raw)
	}
	person, _ := me.body["person"].(map[string]any)
	if id, _ := person["id"].(string); id != personID {
		t.Fatalf("/me person %q != auth personID %q", id, personID)
	}
	// Provisioning should have created a personal org membership.
	if mems, _ := me.body["memberships"].([]any); len(mems) == 0 {
		t.Fatalf("expected memberships from provisioning, got %s", me.raw)
	}
}

// TestAppleAuthIsIdempotentPerSub checks that signing in again with the same
// Apple sub returns the same Person, and a different sub a different Person.
func TestAppleAuthIsIdempotentPerSub(t *testing.T) {
	resetDB(t)

	first := appleSignIn(t, "apple-sub-x", "x@example.com", nil)
	second := appleSignIn(t, "apple-sub-x", "x@example.com", nil)
	if first.body["personID"] != second.body["personID"] {
		t.Fatalf("same sub should map to same person: %v vs %v",
			first.body["personID"], second.body["personID"])
	}

	other := appleSignIn(t, "apple-sub-y", "y@example.com", nil)
	if other.body["personID"] == first.body["personID"] {
		t.Fatal("different subs should map to different persons")
	}
}

// TestAppleSignInWillNotTakeOverAnExistingAddress is the whole pre-hijack, asserted
// closed.
//
// POST /auth/register sends no verification mail, so an address in user_accounts is one
// somebody typed. /auth/apple used to link a first-time Apple identity to whatever
// account held a matching address and sign the caller into it, which meant registering
// an address you do not own handed you the account of whoever later signed in with
// Apple at it — and kept handing it to you, since the password stayed yours.
func TestAppleSignInWillNotTakeOverAnExistingAddress(t *testing.T) {
	resetDB(t)
	const victimEmail = "victim-takeover@example.com"

	// The attacker registers an address they do not own.
	_, attackerPerson := registerUser(t, victimEmail)

	// The victim's first Apple sign-in at that address is refused, not merged.
	r := appleSignIn(t, "victim-apple-sub", victimEmail, "Real Victim")
	if r.status != http.StatusConflict {
		t.Fatalf("apple sign-in over a registered address: got %d %s, want 409", r.status, r.raw)
	}
	if code, _ := errCode(r); code != "EMAIL_ALREADY_REGISTERED" {
		// The client has to tell this apart from the other 409 on this endpoint (a
		// pre-claimed Person id) to know the next step is a password sign-in.
		t.Errorf("error code %q, want EMAIL_ALREADY_REGISTERED", code)
	}
	if _, ok := r.body["token"]; ok {
		t.Error("a refused sign-in must not return a session")
	}

	// Nothing was linked, so the attacker never gains the victim's Apple identity.
	if n := countRows(t, `SELECT count(*) FROM user_accounts WHERE apple_sub IS NOT NULL`); n != 0 {
		t.Errorf("an Apple identity was linked to the pre-registered account, found %d", n)
	}
	// And no second identity was quietly provisioned onto the attacker's Person either.
	if n := countRows(t, `SELECT count(*) FROM user_accounts`); n != 1 {
		t.Errorf("expected only the attacker's own account, found %d", n)
	}
	_ = attackerPerson
}

// TestAppleLinkRequiresTheAccountsOwnSession covers the sanctioned replacement for that
// merge: proof of control comes from a session, which the attacker in the test above
// does not have and the real account holder does.
func TestAppleLinkRequiresTheAccountsOwnSession(t *testing.T) {
	resetDB(t)
	const email = "linkme@example.com"

	token, personID := registerUser(t, email)

	// Unauthenticated, the endpoint is not reachable at all.
	if r := do(t, http.MethodPost, "/api/v1/me/apple-link", "", map[string]any{
		"identityToken": devIdentityToken(t, "apple-sub-link", email),
	}); r.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated link: got %d %s, want 401", r.status, r.raw)
	}

	// Signed in, the coach links their Apple ID to their own account.
	link := do(t, http.MethodPost, "/api/v1/me/apple-link", token, map[string]any{
		"identityToken": devIdentityToken(t, "apple-sub-link", email),
	})
	if link.status != http.StatusOK {
		t.Fatalf("link: got %d %s, want 200", link.status, link.raw)
	}
	// Retrying a lost response must not fail.
	again := do(t, http.MethodPost, "/api/v1/me/apple-link", token, map[string]any{
		"identityToken": devIdentityToken(t, "apple-sub-link", email),
	})
	if again.status != http.StatusOK {
		t.Errorf("re-linking the same Apple ID: got %d %s, want 200", again.status, again.raw)
	}

	// From then on Sign in with Apple resolves to that same Person — the outcome the
	// merge used to reach by guessing.
	signIn := appleSignIn(t, "apple-sub-link", email, nil)
	if signIn.status != http.StatusOK {
		t.Fatalf("apple sign-in after linking: got %d %s", signIn.status, signIn.raw)
	}
	if got, _ := signIn.body["personID"].(string); got != personID {
		t.Errorf("apple sign-in resolved to %q, want the linked person %q", got, personID)
	}

	// A different Apple ID cannot be stacked onto the same account...
	other := do(t, http.MethodPost, "/api/v1/me/apple-link", token, map[string]any{
		"identityToken": devIdentityToken(t, "some-other-sub", email),
	})
	if other.status != http.StatusConflict {
		t.Errorf("linking a second Apple ID: got %d %s, want 409", other.status, other.raw)
	}
	// ...and one Apple ID cannot be linked to two accounts, which is what keeps
	// GetUserAccountByAppleSub unambiguous.
	stranger, _ := registerUser(t, "stranger@example.com")
	taken := do(t, http.MethodPost, "/api/v1/me/apple-link", stranger, map[string]any{
		"identityToken": devIdentityToken(t, "apple-sub-link", "stranger@example.com"),
	})
	if taken.status != http.StatusConflict {
		t.Errorf("linking an Apple ID that belongs to another account: got %d %s, want 409",
			taken.status, taken.raw)
	}
}

// TestAppleAuthReturnsUsableRefreshToken pins the fix for a session that could
// not be renewed: /auth/apple used to sign a bare access token and never create a
// refresh row, so with JWT_ACCESS_TTL at 15m an Apple-signed-in coach was logged
// out mid-training-session with no recovery but a full re-authorization. The
// token must not just be present — it must actually redeem at /auth/refresh.
func TestAppleAuthReturnsUsableRefreshToken(t *testing.T) {
	resetDB(t)

	r := appleSignIn(t, "apple-sub-refresh", "refresh@example.com", nil)
	if r.status != http.StatusOK {
		t.Fatalf("apple auth: status %d body %s", r.status, r.raw)
	}
	refresh, _ := r.body["refreshToken"].(string)
	if refresh == "" {
		t.Fatalf("apple sign-in returned no refreshToken: %s", r.raw)
	}

	rotated := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refreshToken": refresh,
	})
	if rotated.status != http.StatusOK {
		t.Fatalf("redeeming the apple refresh token: status %d body %s", rotated.status, rotated.raw)
	}
	access, _ := rotated.body["accessToken"].(string)
	if access == "" {
		t.Fatalf("refresh returned no accessToken: %s", rotated.raw)
	}

	// The renewed access token must authenticate as the same coach.
	me := do(t, http.MethodGet, "/api/v1/me", access, nil)
	if me.status != http.StatusOK {
		t.Fatalf("renewed token on /me: status %d body %s", me.status, me.raw)
	}
	person, _ := me.body["person"].(map[string]any)
	if id, _ := person["id"].(string); id != r.body["personID"] {
		t.Fatalf("renewed session is a different person: %v vs %v", id, r.body["personID"])
	}
}

func TestAppleAuthRejectsMissingToken(t *testing.T) {
	resetDB(t)
	r := do(t, http.MethodPost, "/api/v1/auth/apple", "", map[string]any{})
	if r.status != http.StatusBadRequest {
		t.Fatalf("missing identityToken: status %d, want 400", r.status)
	}
}

// TestAppleAuthIgnoresAnUnverifiedAddress — an address Apple has not vouched for is
// treated as no address at all, so it is never written to user_accounts.email.
//
// The linking this used to guard is gone (see
// TestAppleSignInWillNotTakeOverAnExistingAddress), which closes the takeover it was
// written for outright. The claim still does work: email is UNIQUE and is what
// /auth/login and /auth/apple key on, so storing one nobody vouched for would plant an
// address the account holder may not own.
func TestAppleAuthIgnoresAnUnverifiedAddress(t *testing.T) {
	resetDB(t)
	const email = "victim-link@example.com"

	if r := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": email, "password": "password123", "displayName": "Victim",
	}); r.status != http.StatusCreated {
		t.Fatalf("register victim: %d %s", r.status, r.raw)
	}

	// A sign-in claiming that address without Apple's verification provisions its own
	// account under a synthesized address and leaves the victim's alone.
	unverified := do(t, http.MethodPost, "/api/v1/auth/apple", "", map[string]any{
		"identityToken": devIdentityTokenVerified(t, "attacker-sub", email, false),
	})
	if unverified.status != http.StatusOK {
		t.Fatalf("unverified sign-in: got %d %s, want 200", unverified.status, unverified.raw)
	}
	var linked int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_accounts WHERE email = $1 AND apple_sub IS NOT NULL`,
		email).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 0 {
		t.Error("an unverified Apple identity was attached to the account at that address")
	}
	if n := countRows(t,
		`SELECT count(*) FROM user_accounts WHERE apple_sub = $1 AND email = $2`,
		"attacker-sub", "apple_attacker-sub@users.soccercoachkit.app"); n != 1 {
		t.Error("an unverified address should be stored as the synthesized one")
	}

	// The victim's password login still works, untouched.
	if r := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": email, "password": "password123",
	}); r.status != http.StatusOK {
		t.Errorf("password login: %d %s", r.status, r.raw)
	}
}

// TestAppleAuthRefusesAPreClaimedPersonID covers the takeover that CreatePersonWithID's
// old ON CONFLICT DO UPDATE allowed.
//
// The coach's Person id is UUIDv5(a namespace published in this repo, apple_sub), so it
// is computable by anyone who knows the subject, and POST /sync will insert a persons
// row at any id an account names. Provisioning used to adopt that row — keeping its
// sync_account_id — so the victim signed in successfully onto a Person the attacker
// owned, and could then have their display name, emergency contact and medical notes
// rewritten, their Person tombstoned, and the row streamed into the attacker's pull.
func TestAppleAuthRefusesAPreClaimedPersonID(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	const sub = "000123.victim-apple-sub.4242"

	// Learn the id this subject derives to the way an attacker would compute it,
	// without hard-coding the namespace constant here.
	probe := appleSignIn(t, sub, "victim@example.com", nil)
	if probe.status != http.StatusOK {
		t.Fatalf("probe sign-in: %d %s", probe.status, probe.raw)
	}
	derived, _ := probe.body["personID"].(string)
	if derived == "" {
		t.Fatalf("no personID in probe response: %s", probe.raw)
	}
	resetDB(t)

	// An unrelated account claims that id before the victim ever signs in.
	attacker, attackerPerson := registerUser(t, "attacker@e.com")
	push := do(t, http.MethodPost, "/api/v1/sync", attacker, map[string]any{
		"upserts": []map[string]any{
			{"type": "Person", "id": derived, "payload": map[string]any{
				"name": "pre-claimed", "medicalNotes": "attacker text",
			}},
		},
	})
	if push.status != http.StatusOK {
		t.Fatalf("attacker push: %d %s", push.status, push.raw)
	}

	// The victim's first sign-in must refuse rather than build an account on that row.
	victim := appleSignIn(t, sub, "victim@example.com", nil)
	if victim.status != http.StatusConflict {
		t.Fatalf("pre-claimed sign-in: expected 409, got %d %s", victim.status, victim.raw)
	}

	// Nothing was provisioned onto the attacker's row, and the whole transaction rolled
	// back: no account, no organization, no memberships.
	if n := countRows(t, `SELECT count(*) FROM user_accounts WHERE person_id = $1`, derived); n != 0 {
		t.Errorf("an account was linked to the pre-claimed Person, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM memberships WHERE person_id = $1`, derived); n != 0 {
		t.Errorf("the pre-claimed Person gained memberships, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM organizations`); n != 1 {
		t.Errorf("expected only the attacker's own org, found %d", n)
	}

	// The attacker's row is untouched — in particular the sign-in did not rewrite
	// display_name, which is what the old DO UPDATE did on its way to adopting it.
	var name string
	var syncAcct *string
	if err := testPool.QueryRow(ctx,
		`SELECT display_name, sync_account_id::text FROM persons WHERE id = $1`, derived).
		Scan(&name, &syncAcct); err != nil {
		t.Fatalf("read pre-claimed person: %v", err)
	}
	if name != "pre-claimed" {
		t.Errorf("display_name = %q; the refused sign-in should not have written to the row", name)
	}
	if syncAcct == nil || *syncAcct != attackerPerson {
		t.Errorf("sync_account_id = %v, want the attacker %s — ownership must not move", syncAcct, attackerPerson)
	}

	// And once the row is gone, the victim can sign in normally.
	if _, err := testPool.Exec(ctx, `DELETE FROM persons WHERE id = $1`, derived); err != nil {
		t.Fatalf("remove the squatted row: %v", err)
	}
	if again := appleSignIn(t, sub, "victim@example.com", nil); again.status != http.StatusOK {
		t.Fatalf("sign-in after cleanup: expected 200, got %d %s", again.status, again.raw)
	}
}

// errCode reads the machine-readable code out of the standard error envelope.
func errCode(r resp) (string, bool) {
	envelope, ok := r.body["error"].(map[string]any)
	if !ok {
		return "", false
	}
	code, ok := envelope["code"].(string)
	return code, ok
}
