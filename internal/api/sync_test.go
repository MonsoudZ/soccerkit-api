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

// TestSyncPushCannotWriteAnotherAccountsRow is the write-direction counterpart to
// TestSyncIsolatedPerAccount, which only ever proved Bob cannot *read* Alice's rows.
// Without an ownership guard on the upsert's conflict clause, a push naming an
// existing row's id rewrote that row and reassigned it to the pusher.
func TestSyncPushCannotWriteAnotherAccountsRow(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	victim, victimPerson := registerUser(t, "sync-victim@e.com")
	attacker := appleToken(t, "sub-sync-attacker", "sync-attacker@example.com")

	teamID := "5e1f0a11-0000-4000-8000-00000000ab01"
	if push := do(t, http.MethodPost, "/api/v1/sync", victim, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"name": "Victim U12"}},
		},
	}); push.status != http.StatusOK {
		t.Fatalf("victim push: %d %s", push.status, push.raw)
	}

	// The attacker names the victim's team id.
	push := do(t, http.MethodPost, "/api/v1/sync", attacker, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"name": "PWNED"}},
		},
	})
	if push.status != http.StatusOK {
		t.Fatalf("attacker push: %d %s", push.status, push.raw)
	}

	// Rejected, and reported back rather than silently dropped.
	conflicts, _ := push.body["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %v", push.body["conflicts"])
	}
	if c, _ := conflicts[0].(map[string]any); c["id"] != teamID || c["type"] != "Team" {
		t.Errorf("conflict should name the rejected record, got %v", conflicts[0])
	}

	// The row is untouched and still the victim's.
	var name string
	var owner string
	if err := testPool.QueryRow(ctx,
		`SELECT name, sync_account_id::text FROM teams WHERE id = $1`, teamID).Scan(&name, &owner); err != nil {
		t.Fatal(err)
	}
	if name != "Victim U12" {
		t.Errorf("team name = %q, want it unchanged", name)
	}
	if owner != victimPerson {
		t.Errorf("team owner = %s, want the victim %s", owner, victimPerson)
	}

	// And it never reaches the attacker's delta.
	if got := pullSync(t, attacker, ""); len(got.Records) != 0 {
		t.Errorf("attacker pulled %d records, want 0", len(got.Records))
	}
}

// TestSyncTombstoneCannotDeleteAnotherAccountsRow covers the delete direction.
func TestSyncTombstoneCannotDeleteAnotherAccountsRow(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	victim, _ := registerUser(t, "tomb-victim@e.com")
	attacker := appleToken(t, "sub-tomb-attacker", "tomb-attacker@example.com")

	drillID := "5e1f0a11-0000-4000-8000-00000000ab02"
	do(t, http.MethodPost, "/api/v1/sync", victim, map[string]any{
		"upserts": []map[string]any{
			{"type": "Drill", "id": drillID, "payload": map[string]any{"title": "Rondo"}},
		},
	})

	push := do(t, http.MethodPost, "/api/v1/sync", attacker, map[string]any{
		"deletes": []map[string]any{{"type": "Drill", "id": drillID}},
	})
	if push.status != http.StatusOK {
		t.Fatalf("attacker delete: %d %s", push.status, push.raw)
	}
	if conflicts, _ := push.body["conflicts"].([]any); len(conflicts) != 1 {
		t.Fatalf("expected the rejected delete in conflicts, got %v", push.body["conflicts"])
	}

	var deleted bool
	if err := testPool.QueryRow(ctx, `SELECT deleted FROM drills WHERE id = $1`, drillID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("attacker tombstoned a drill they do not own")
	}
}

// TestSyncCannotAdoptRESTCreatedRow guards the separation 0002_sync.sql describes:
// rows written through the REST API have a NULL sync_account_id and sync may not
// claim them.
func TestSyncCannotAdoptRESTCreatedRow(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	coach, _ := registerUser(t, "rest-owner@e.com")

	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "REST Team"})
	teamID, _ := team.body["id"].(string)

	push := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"name": "Claimed"}},
		},
	})
	if conflicts, _ := push.body["conflicts"].([]any); len(conflicts) != 1 {
		t.Fatalf("REST-created row should not be adoptable, conflicts=%v", push.body["conflicts"])
	}

	var name string
	var owner *string
	if err := testPool.QueryRow(ctx,
		`SELECT name, sync_account_id::text FROM teams WHERE id = $1`, teamID).Scan(&name, &owner); err != nil {
		t.Fatal(err)
	}
	if name != "REST Team" || owner != nil {
		t.Errorf("REST row changed: name=%q owner=%v", name, owner)
	}
}

// TestTombstonesHideRowsFromREST — the sync spine's `deleted` flag was written by the
// sync path and read by nobody else, so a row deleted in the app went on being served
// by every REST list and get.
func TestTombstonesHideRowsFromREST(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "tombstone-rest@e.com")

	teamID := "7d000000-0000-4000-8000-00000000dd01"
	drillID := "7d000000-0000-4000-8000-00000000dd02"
	do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"name": "Synced Team"}},
			{"type": "Drill", "id": drillID, "payload": map[string]any{"title": "Synced Drill"}},
		},
	})
	if teams := do(t, http.MethodGet, "/api/v1/teams", coach, nil).arr(); len(teams) != 1 {
		t.Fatalf("expected the synced team to be listed, got %v", teams)
	}

	del := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
		"deletes": []map[string]any{
			{"type": "Team", "id": teamID},
			{"type": "Drill", "id": drillID},
		},
	})
	if conflicts, _ := del.body["conflicts"].([]any); len(conflicts) != 0 {
		t.Fatalf("owner's own deletes should apply, got conflicts %v", conflicts)
	}

	if teams := do(t, http.MethodGet, "/api/v1/teams", coach, nil).arr(); len(teams) != 0 {
		t.Errorf("tombstoned team still listed: %v", teams)
	}
	if r := do(t, http.MethodGet, "/api/v1/teams/"+teamID, coach, nil); r.status != http.StatusNotFound {
		t.Errorf("GET tombstoned team: got %d, want 404", r.status)
	}
	if drills := do(t, http.MethodGet, "/api/v1/drills", coach, nil).arr(); len(drills) != 0 {
		t.Errorf("tombstoned drill still listed: %v", drills)
	}
}

// TestRESTDeleteReachesSyncClients — the other direction. DELETE /teams/{id} used to
// drop the row outright, which produced no delta, so a device holding the team never
// learned it was gone and re-created it on its next push.
func TestRESTDeleteReachesSyncClients(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "rest-delete-sync@e.com")

	teamID := "7d000000-0000-4000-8000-00000000dd03"
	do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
		"upserts": []map[string]any{{"type": "Team", "id": teamID, "payload": map[string]any{"name": "Doomed"}}},
	})
	cursor := pullSync(t, coach, "").Cursor

	if r := do(t, http.MethodDelete, "/api/v1/teams/"+teamID, coach, nil); r.status != http.StatusOK {
		t.Fatalf("delete team: %d %s", r.status, r.raw)
	}

	delta := pullSync(t, coach, cursor)
	if len(delta.Deletes) != 1 || delta.Deletes[0].ID != teamID {
		t.Fatalf("REST delete should reach the client as a tombstone, got %+v", delta)
	}
	if len(delta.Records) != 0 {
		t.Errorf("expected no live records in the delta, got %+v", delta.Records)
	}
}
