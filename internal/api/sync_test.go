package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

type syncPull struct {
	Records []struct {
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Payload json.RawMessage `json:"payload"`
	} `json:"records"`
	Deletes []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"deletes"`
	Cursor string `json:"cursor"`
}

// appleToken signs in via Sign in with Apple (dev bypass) and returns the token.
func appleToken(t *testing.T, sub, email string) string {
	t.Helper()
	r := appleSignIn(t, sub, email, nil)
	if r.status != http.StatusOK {
		t.Fatalf("apple sign-in: status %d body %s", r.status, r.raw)
	}
	tok, _ := r.body["token"].(string)
	if tok == "" {
		t.Fatalf("apple sign-in returned no token: %s", r.raw)
	}
	return tok
}

func pullSync(t *testing.T, token, since string) syncPull {
	t.Helper()
	path := "/api/v1/sync"
	if since != "" {
		path += "?since=" + since
	}
	r := do(t, http.MethodGet, path, token, nil)
	if r.status != http.StatusOK {
		t.Fatalf("pull: status %d body %s", r.status, r.raw)
	}
	var out syncPull
	if err := json.Unmarshal(r.raw, &out); err != nil {
		t.Fatalf("decode pull: %v (%s)", err, r.raw)
	}
	return out
}

// TestSyncRoundTrip covers a projected type (Drill) and a generic-fallback type
// (Diagram) through push → pull → tombstone, and verifies the projected row
// actually lands in the domain table (not just the payload store).
func TestSyncRoundTrip(t *testing.T) {
	resetDB(t)
	token := appleToken(t, "sub-sync-1", "sync1@example.com")

	drillID := "11111111-1111-1111-1111-111111111111"
	diagramID := "22222222-2222-2222-2222-222222222222"

	push := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{
			{"type": "Drill", "id": drillID, "payload": map[string]any{"id": drillID, "title": "Rondo 4v2"}},
			{"type": "Diagram", "id": diagramID, "payload": map[string]any{"id": diagramID, "title": "4-3-3 press"}},
		},
		"deletes": []any{},
		"cursor":  nil,
	})
	if push.status != http.StatusOK {
		t.Fatalf("push: status %d body %s", push.status, push.raw)
	}

	pull := pullSync(t, token, "")
	if len(pull.Records) != 2 {
		t.Fatalf("expected 2 records, got %d: %s", len(pull.Records), mustJSON(pull))
	}
	if pull.Cursor == "" {
		t.Fatal("expected a non-empty cursor")
	}
	if got := payloadField(t, pull, "Drill", "title"); got != "Rondo 4v2" {
		t.Fatalf("drill payload title = %q, want Rondo 4v2", got)
	}
	if got := payloadField(t, pull, "Diagram", "title"); got != "4-3-3 press" {
		t.Fatalf("diagram payload title = %q, want 4-3-3 press", got)
	}

	// The projected Drill must be a real row in the drills table with name projected.
	var name string
	if err := testPool.QueryRow(context.Background(),
		`SELECT name FROM drills WHERE id = $1`, drillID).Scan(&name); err != nil {
		t.Fatalf("projected drill row not found: %v", err)
	}
	if name != "Rondo 4v2" {
		t.Fatalf("projected drills.name = %q, want Rondo 4v2", name)
	}

	// A pull at head cursor is empty.
	if empty := pullSync(t, token, pull.Cursor); len(empty.Records)+len(empty.Deletes) != 0 {
		t.Fatalf("expected empty delta at head cursor, got %s", mustJSON(empty))
	}

	// Delete both; they should return as tombstones past the old cursor.
	del := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []any{},
		"deletes": []map[string]any{
			{"type": "Drill", "id": drillID},
			{"type": "Diagram", "id": diagramID},
		},
	})
	if del.status != http.StatusOK {
		t.Fatalf("delete push: status %d body %s", del.status, del.raw)
	}
	after := pullSync(t, token, pull.Cursor)
	if len(after.Deletes) != 2 {
		t.Fatalf("expected 2 tombstones, got %s", mustJSON(after))
	}
}

// TestSyncIsolatedPerAccount proves one account never sees another's records.
func TestSyncIsolatedPerAccount(t *testing.T) {
	resetDB(t)
	alice := appleToken(t, "sub-alice", "alice@example.com")
	bob := appleToken(t, "sub-bob", "bob@example.com")

	do(t, http.MethodPost, "/api/v1/sync", alice, map[string]any{
		"upserts": []map[string]any{
			{"type": "Diagram", "id": "aaaa1111-1111-1111-1111-111111111111", "payload": map[string]any{"x": 1}},
		},
	})

	if got := pullSync(t, bob, ""); len(got.Records) != 0 {
		t.Fatalf("bob should see none of alice's records, got %d", len(got.Records))
	}
	if got := pullSync(t, alice, ""); len(got.Records) != 1 {
		t.Fatalf("alice should see her own record, got %d", len(got.Records))
	}
}

// TestSyncPushCannotOverwriteAnotherAccount proves a push may not write a row
// this account does not own. Isolation on pull is not enough: the upsert is
// keyed on the bare id, so without an owner guard Bob could overwrite Alice's
// row by id and reassign it to himself — and she would never learn.
func TestSyncPushCannotOverwriteAnotherAccount(t *testing.T) {
	resetDB(t)
	alice := appleToken(t, "sub-alice-own", "alice-own@example.com")
	bob := appleToken(t, "sub-bob-hijack", "bob-hijack@example.com")

	teamID := "cccc1111-1111-1111-1111-111111111111"
	push := do(t, http.MethodPost, "/api/v1/sync", alice, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"id": teamID, "name": "Alice FC"}},
		},
	})
	if push.status != http.StatusOK {
		t.Fatalf("alice push: status %d body %s", push.status, push.raw)
	}

	hijack := do(t, http.MethodPost, "/api/v1/sync", bob, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"id": teamID, "name": "Bob FC"}},
		},
	})
	if hijack.status != http.StatusForbidden {
		t.Fatalf("bob's hijacking push: status %d, want 403; body %s", hijack.status, hijack.raw)
	}

	// The row must be untouched and still owned by Alice.
	var name string
	var owner string
	if err := testPool.QueryRow(context.Background(),
		`SELECT name, sync_account_id::text FROM teams WHERE id = $1`, teamID).Scan(&name, &owner); err != nil {
		t.Fatalf("team row: %v", err)
	}
	if name != "Alice FC" {
		t.Fatalf("teams.name = %q, want Alice FC — bob overwrote alice's row", name)
	}

	// And Bob must not have acquired it.
	if got := pullSync(t, bob, ""); len(got.Records) != 0 {
		t.Fatalf("bob should own no records, got %s", mustJSON(got))
	}
	if got := pullSync(t, alice, ""); len(got.Records) != 1 {
		t.Fatalf("alice should still own her team, got %s", mustJSON(got))
	}
}

// TestSyncPushClaimsOwnPersonRow guards the flow migration 0003 exists to enable:
// the coach's account Person is created by /auth/apple with no sync_account_id,
// so the owner guard must still let the app's first push adopt that very row
// (the id is derived from the Apple sub, so it is the same row).
func TestSyncPushClaimsOwnPersonRow(t *testing.T) {
	resetDB(t)
	r := appleSignIn(t, "sub-self-claim", "self@example.com", "Sam Coach")
	if r.status != http.StatusOK {
		t.Fatalf("apple sign-in: status %d body %s", r.status, r.raw)
	}
	token, _ := r.body["token"].(string)
	personID, _ := r.body["personID"].(string)
	if personID == "" {
		t.Fatalf("apple sign-in returned no personID: %s", r.raw)
	}

	push := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{
			{"type": "Person", "id": personID, "payload": map[string]any{"id": personID, "name": "Sam Coach"}},
		},
	})
	if push.status != http.StatusOK {
		t.Fatalf("coach pushing their own Person: status %d body %s", push.status, push.raw)
	}

	var owner string
	if err := testPool.QueryRow(context.Background(),
		`SELECT sync_account_id::text FROM persons WHERE id = $1`, personID).Scan(&owner); err != nil {
		t.Fatalf("person row: %v", err)
	}
	if owner != personID {
		t.Fatalf("persons.sync_account_id = %q, want %q — the coach did not adopt their own row", owner, personID)
	}
}

func TestSyncRequiresAuth(t *testing.T) {
	resetDB(t)
	if r := do(t, http.MethodGet, "/api/v1/sync", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pull: status %d, want 401", r.status)
	}
}

// --- helpers --------------------------------------------------------------

func payloadField(t *testing.T, p syncPull, typ, field string) string {
	t.Helper()
	for _, rec := range p.Records {
		if rec.Type != typ {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Payload, &m); err != nil {
			t.Fatalf("payload for %s not an object: %v", typ, err)
		}
		s, _ := m[field].(string)
		return s
	}
	return ""
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
