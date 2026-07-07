package api_test

import (
	"net/http"
	"testing"
)

func TestRegisterAndMe(t *testing.T) {
	resetDB(t)

	r := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "New@Example.com", "password": "password123", "displayName": "New User",
	})
	if r.status != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", r.status, r.raw)
	}
	user := r.body["user"].(map[string]any)
	if user["email"] != "new@example.com" {
		t.Errorf("email should be lowercased, got %v", user["email"])
	}
	if _, leaked := user["passwordHash"]; leaked {
		t.Error("passwordHash must not be exposed")
	}

	token := r.body["accessToken"].(string)
	me := do(t, http.MethodGet, "/api/v1/me", token, nil)
	if me.status != http.StatusOK || me.body["email"] != "new@example.com" {
		t.Fatalf("GET /me failed: %d %s", me.status, me.raw)
	}
}

func TestRegisterDuplicateAndValidation(t *testing.T) {
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

func TestLogin(t *testing.T) {
	resetDB(t)
	do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "log@e.com", "password": "password123", "displayName": "Log",
	})

	ok := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "log@e.com", "password": "password123",
	})
	if ok.status != http.StatusOK || ok.body["accessToken"] == nil {
		t.Fatalf("valid login failed: %d %s", ok.status, ok.raw)
	}

	bad := do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "log@e.com", "password": "wrongpassword",
	})
	if bad.status != http.StatusUnauthorized {
		t.Errorf("expected 401 on bad password, got %d", bad.status)
	}
}

func TestRefreshRotation(t *testing.T) {
	resetDB(t)
	reg := do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": "rot@e.com", "password": "password123", "displayName": "Rot",
	})
	refresh := reg.body["refreshToken"].(string)

	first := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh})
	if first.status != http.StatusOK {
		t.Fatalf("first refresh failed: %d %s", first.status, first.raw)
	}
	if first.body["refreshToken"] == refresh {
		t.Error("refresh token should rotate")
	}

	reuse := do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh})
	if reuse.status != http.StatusUnauthorized {
		t.Errorf("reusing rotated token should 401, got %d", reuse.status)
	}
}

func TestProtectedRouteRequiresAuth(t *testing.T) {
	resetDB(t)
	if r := do(t, http.MethodGet, "/api/v1/me", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", r.status)
	}
}
