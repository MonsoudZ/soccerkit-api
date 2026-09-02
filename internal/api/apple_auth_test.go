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

// TestAppleSignInWillNotTakeOverAnExistingAddress — an address is not proof of
// anything, so it never grants a sign-in to an account it does not already belong to.
//
// This was reachable: POST /auth/register verified no address, and /auth/apple linked a
// first-time Apple identity to whatever account held a matching one, so registering an
// address you did not own handed you that person's account the moment they first signed
// in with Apple (docs/AUDIT-3.md C1). Registration is gone now, which removes the way to
// plant an address, and the guard remains for the invariant rather than for that route:
// the account below has to be created directly, because no endpoint can produce this
// state any more.
func TestAppleSignInWillNotTakeOverAnExistingAddress(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	const email = "victim-takeover@example.com"

	// An account already holding the address, belonging to some other Apple identity.
	var personID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO persons (display_name, email) VALUES ('Someone Else', $1) RETURNING id`,
		email).Scan(&personID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO user_accounts (person_id, email, apple_sub) VALUES ($1, $2, 'someone-elses-sub')`,
		personID, email); err != nil {
		t.Fatal(err)
	}

	// A different Apple identity presenting that address is refused, not merged.
	r := appleSignIn(t, "victim-apple-sub", email, "Real Victim")
	if r.status != http.StatusConflict {
		t.Fatalf("apple sign-in over a claimed address: got %d %s, want 409", r.status, r.raw)
	}
	if code, _ := errCode(r); code != "EMAIL_ALREADY_REGISTERED" {
		// The client has to tell this apart from the other 409 on this endpoint (a
		// pre-claimed Person id), so the code carries the distinction the status cannot.
		t.Errorf("error code %q, want EMAIL_ALREADY_REGISTERED", code)
	}
	if _, ok := r.body["token"]; ok {
		t.Error("a refused sign-in must not return a session")
	}

	// Nothing was linked or provisioned onto the existing account.
	if n := countRows(t, `SELECT count(*) FROM user_accounts`); n != 1 {
		t.Errorf("expected only the existing account, found %d", n)
	}
	if n := countRows(t,
		`SELECT count(*) FROM user_accounts WHERE apple_sub = 'victim-apple-sub'`); n != 0 {
		t.Errorf("the refused identity was attached to an account anyway, found %d", n)
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
// user_accounts.email is UNIQUE and is what /auth/apple keys its refusal on, so storing
// one nobody vouched for would plant an address the account holder may not own. That an
// unverified address could become an account's identity is the shape of the takeover in
// docs/AUDIT-3.md C1, from the other direction.
func TestAppleAuthIgnoresAnUnverifiedAddress(t *testing.T) {
	resetDB(t)
	const claimed = "someone-elses@example.com"

	r := do(t, http.MethodPost, "/api/v1/auth/apple", "", map[string]any{
		"identityToken": devIdentityTokenVerified(t, "unverified-sub", claimed, false),
	})
	if r.status != http.StatusOK {
		t.Fatalf("unverified sign-in: got %d %s, want 200", r.status, r.raw)
	}
	if n := countRows(t, `SELECT count(*) FROM user_accounts WHERE email = $1`, claimed); n != 0 {
		t.Errorf("an unverified address was stored as an account's identity, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM user_accounts WHERE apple_sub = $1 AND email = $2`,
		"unverified-sub", "apple_unverified-sub@users.soccercoachkit.app"); n != 1 {
		t.Error("an unverified address should fall back to the synthesized one")
	}

	// A verified claim is stored, which is what makes the distinction load-bearing.
	if r := appleSignIn(t, "verified-sub", "verified@example.com", nil); r.status != http.StatusOK {
		t.Fatalf("verified sign-in: %d %s", r.status, r.raw)
	}
	if n := countRows(t, `SELECT count(*) FROM user_accounts WHERE email = $1`,
		"verified@example.com"); n != 1 {
		t.Errorf("a verified address should be stored, found %d", n)
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
	attacker, attackerPerson := signInCoach(t, "attacker@e.com")
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
