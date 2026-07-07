package api_test

import (
	"net/http"
	"testing"
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
