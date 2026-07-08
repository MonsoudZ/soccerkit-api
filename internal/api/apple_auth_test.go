package api_test

import (
	"net/http"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// devIdentityToken forges an unsigned Apple-style identity token. Accepted only
// because the test server runs with DEV_APPLE_BYPASS=true.
func devIdentityToken(t *testing.T, sub, email string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://appleid.apple.com", "sub": sub, "email": email,
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

// TestAppleAuthLinksExistingEmailAccount checks that an Apple sign-in whose email
// matches an existing password account links to that same Person.
func TestAppleAuthLinksExistingEmailAccount(t *testing.T) {
	resetDB(t)

	_, personID := registerUser(t, "linkme@example.com")

	r := appleSignIn(t, "apple-sub-link", "linkme@example.com", nil)
	if r.status != http.StatusOK {
		t.Fatalf("apple auth: status %d body %s", r.status, r.raw)
	}
	if got, _ := r.body["personID"].(string); got != personID {
		t.Fatalf("apple sign-in should link to existing person %q, got %q", personID, got)
	}
}

func TestAppleAuthRejectsMissingToken(t *testing.T) {
	resetDB(t)
	r := do(t, http.MethodPost, "/api/v1/auth/apple", "", map[string]any{})
	if r.status != http.StatusBadRequest {
		t.Fatalf("missing identityToken: status %d, want 400", r.status)
	}
}
