package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
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

	// Isolation is not only a read property: Bob must not be able to write over the
	// record either. Asserting only the pull direction is what let a cross-tenant write
	// live in a green suite.
	push := do(t, http.MethodPost, "/api/v1/sync", bob, map[string]any{
		"upserts": []map[string]any{
			{"type": "Diagram", "id": "aaaa1111-1111-1111-1111-111111111111", "payload": map[string]any{"x": 99}},
		},
	})
	if conflicts, _ := push.body["conflicts"].([]any); len(conflicts) != 1 {
		t.Fatalf("bob's write to alice's record should be refused, conflicts=%v", push.body["conflicts"])
	}
	got := pullSync(t, alice, "")
	if len(got.Records) != 1 {
		t.Fatalf("alice should still have exactly her own record, got %d", len(got.Records))
	}
	var payload struct {
		X int `json:"x"`
	}
	if err := json.Unmarshal(got.Records[0].Payload, &payload); err != nil {
		t.Fatalf("decode alice's payload: %v", err)
	}
	if payload.X != 1 {
		t.Errorf("alice's record was altered: x = %d, want 1", payload.X)
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
	victim, victimPerson := signInCoach(t, "sync-victim@e.com")
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
	victim, _ := signInCoach(t, "tomb-victim@e.com")
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

// TestRESTCreatedTeamIsOwnedByItsCreator replaces an earlier test that asserted a
// REST-created row could not be adopted by anyone, its creator included.
//
// That assertion was right while the two write paths were separate: a REST insert left
// sync_account_id NULL, the row was invisible to sync, and any push naming its id
// conflicted. Creating over REST now puts the row in the creator's own stream so their
// app can go on editing it, which is the point of the change -- so the old assertion is
// wrong now rather than broken.
//
// The half that still has to hold, and that now carries the weight, is ownership: an id
// belonging to one account is not writable by another. That was only tested for
// sync-created rows (TestSyncPushCannotWriteAnotherAccountsRow), and a REST-created row
// is a second way to reach the same guard -- one that did not need guarding while those
// rows had no owner at all.
func TestRESTCreatedTeamIsOwnedByItsCreator(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	coach, coachPerson := signInCoach(t, "rest-owner@e.com")
	stranger := appleToken(t, "sub-rest-stranger", "rest-stranger@example.com")

	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{
		"name": "REST Team", "ageGroup": "U12", "season": "2026",
	})
	if team.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", team.status, team.raw)
	}
	teamID, _ := team.body["id"].(string)

	var owner *string
	if err := testPool.QueryRow(ctx,
		`SELECT sync_account_id::text FROM teams WHERE id = $1`, teamID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner == nil || *owner != coachPerson {
		t.Fatalf("a REST-created team belongs to its creator's stream; sync_account_id is %v", owner)
	}

	// A stranger naming the id is refused and told, exactly as for a synced row.
	push := do(t, http.MethodPost, "/api/v1/sync", stranger, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"name": "PWNED"}},
		},
	})
	if push.status != http.StatusOK {
		t.Fatalf("stranger push: %d %s", push.status, push.raw)
	}
	if conflicts, _ := push.body["conflicts"].([]any); len(conflicts) != 1 {
		t.Fatalf("a stranger must not claim a REST-created row, conflicts=%v", push.body["conflicts"])
	}
	var name string
	if err := testPool.QueryRow(ctx,
		`SELECT name FROM teams WHERE id = $1`, teamID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "REST Team" {
		t.Errorf("the row was changed by a stranger: name=%q", name)
	}

	// The creator's own device may go on editing it -- the convergence this enables.
	own := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"name": "Renamed On Phone"}},
		},
	})
	if conflicts, _ := own.body["conflicts"].([]any); len(conflicts) != 0 {
		t.Fatalf("the creator's own push should apply, conflicts=%v", own.body["conflicts"])
	}
	if err := testPool.QueryRow(ctx,
		`SELECT name FROM teams WHERE id = $1`, teamID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Renamed On Phone" {
		t.Errorf("the creator's push did not apply: name=%q", name)
	}
}

// TestTombstonesHideRowsFromREST — the sync spine's `deleted` flag was written by the
// sync path and read by nobody else, so a row deleted in the app went on being served
// by every REST list and get.
func TestTombstonesHideRowsFromREST(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "tombstone-rest@e.com")

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
	coach, _ := signInCoach(t, "rest-delete-sync@e.com")

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

// TestSyncPushRejectsOversizedBatch — every record in a push is a statement inside one
// transaction, so an unbounded batch held it open for as long as it took.
func TestSyncPushRejectsOversizedBatch(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "batch@e.com")

	upserts := make([]map[string]any, 1001)
	for i := range upserts {
		upserts[i] = map[string]any{
			"type": "Prefs", "id": fmt.Sprintf("k%04d", i), "payload": map[string]any{"i": i},
		}
	}
	r := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{"upserts": upserts})
	if r.status != http.StatusBadRequest {
		t.Fatalf("oversized batch: got %d %s, want 400", r.status, r.raw)
	}
	// Rejected before the transaction opens, so nothing landed.
	if got := pullSync(t, coach, ""); len(got.Records) != 0 {
		t.Errorf("a rejected batch must write nothing, got %d records", len(got.Records))
	}

	// One under the cap still works.
	if r := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{"upserts": upserts[:1000]}); r.status != http.StatusOK {
		t.Fatalf("batch at the cap: got %d %s, want 200", r.status, r.raw)
	}
}

// TestRequestBodyIsCapped — decodeJSON would previously read a body of any size.
func TestRequestBodyIsCapped(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "bodycap@e.com")

	huge := strings.Repeat("a", 5<<20) // 5 MiB, over the 4 MiB cap
	r := do(t, http.MethodPost, "/api/v1/drills", coach, map[string]any{"name": huge})
	if r.status != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d, want 400", r.status)
	}
}

// TestSyncRejectsAnUnstorableCharacter — a NUL escape is valid JSON and illegal in
// jsonb, and the failure used to abort the push as a bare 500. An offline-first client
// retries the batch it failed to push, so that device stopped syncing until the record
// was changed on the phone; a 400 naming the record is something it can act on.
func TestSyncRejectsAnUnstorableCharacter(t *testing.T) {
	resetDB(t)
	token, _ := signInCoach(t, "unstorable@example.com")

	body := `{"upserts":[{"type":"Note","id":"note-1","payload":{"text":"hello` +
		"\\u0000" + `world"}}]}`
	req, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/v1/sync", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a payload with a NUL escape: got %d %s, want 400", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "note-1") {
		t.Errorf("the error should name the offending record: %s", raw)
	}

	// Nothing was written, and the account can still sync.
	if n := countRows(t, `SELECT count(*) FROM sync_documents`); n != 0 {
		t.Errorf("the rejected push wrote %d rows", n)
	}
	if r := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{
			{"type": "Note", "id": "note-2", "payload": map[string]any{"text": "fine"}},
		},
	}); r.status != http.StatusOK {
		t.Errorf("a later clean push: %d %s", r.status, r.raw)
	}
}

// TestSyncPullIsPagedAndLosesNothing — a pull used to return the whole delta, so an
// account that had pushed for a season made every since=0 pull (a reinstall) an
// unbounded allocation. It now returns a page, and the client resumes from the cursor.
//
// The property that matters is not the page size, it is that draining the pages yields
// exactly what one unpaged response would have: every record once, none skipped. That
// holds because seq comes from a single sequence and is unique across every source, so a
// page boundary can never fall inside a group of rows sharing a cursor value.
func TestSyncPullIsPagedAndLosesNothing(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "paged@e.com")

	// More records than one page holds, pushed in batches the push cap allows.
	const total = 1200
	written := map[string]bool{}
	for start := 0; start < total; start += 400 {
		upserts := []map[string]any{}
		for i := start; i < start+400; i++ {
			id := fmt.Sprintf("note-%04d", i)
			written[id] = true
			upserts = append(upserts, map[string]any{
				"type": "Note", "id": id, "payload": map[string]any{"n": i},
			})
		}
		if r := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
			"upserts": upserts,
		}); r.status != http.StatusOK {
			t.Fatalf("push %d: %d %s", start, r.status, r.raw)
		}
	}

	// Drain the way a client does: ask again from the cursor until it stops moving.
	seen := map[string]int{}
	cursor := "0"
	pages := 0
	for pages < 50 {
		pages++
		var pull syncPull
		r := do(t, http.MethodGet, "/api/v1/sync?since="+cursor, coach, nil)
		if r.status != http.StatusOK {
			t.Fatalf("pull page %d: %d %s", pages, r.status, r.raw)
		}
		if err := json.Unmarshal(r.raw, &pull); err != nil {
			t.Fatalf("decode page %d: %v", pages, err)
		}
		if len(pull.Records) > 500 {
			t.Fatalf("page %d returned %d records; the page cap is 500", pages, len(pull.Records))
		}
		for _, rec := range pull.Records {
			seen[rec.ID]++
		}
		next := pull.Cursor
		if next == cursor {
			break
		}
		cursor = next
	}

	if pages < 2 {
		t.Fatalf("expected the delta to need more than one page, drained in %d", pages)
	}
	if len(seen) != total {
		t.Errorf("drained %d distinct records, want %d — a page boundary dropped rows",
			len(seen), total)
	}
	for id := range written {
		switch seen[id] {
		case 1: // delivered exactly once
		case 0:
			t.Fatalf("%s was never delivered", id)
		default:
			t.Fatalf("%s was delivered %d times", id, seen[id])
		}
	}
}

// TestSyncPullAlwaysMakesProgress — the byte budget must not be able to stall the
// cursor. A payload larger than the budget is returned on its own rather than skipped;
// skipping it would leave the client asking for the same page forever, because the
// cursor only advances over rows actually delivered.
//
// The heavy record is planted rather than pushed, and that is the point rather than a
// convenience: maxSyncRecordBytes now rejects a payload this size at the door, so no
// push can create one. The rule still has to hold, because rows written before that cap
// existed are still in the table and a page that cannot get past one is a device that
// never syncs again. Planting is the only way left to reach the state.
func TestSyncPullAlwaysMakesProgress(t *testing.T) {
	resetDB(t)
	coach, personID := signInCoach(t, "bigpayload@e.com")

	// One record heavier than the page's byte budget, then an ordinary one after it.
	big := strings.Repeat("x", 3<<20)
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO sync_documents (sync_account_id, type, id, payload, deleted, seq)
		VALUES ($1, 'Note', 'heavy', $2::jsonb, false, nextval('sync_seq'))`,
		personID, fmt.Sprintf(`{"blob":%q}`, big)); err != nil {
		t.Fatalf("plant the heavy record: %v", err)
	}
	if r := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
		"upserts": []map[string]any{
			{"type": "Note", "id": "light", "payload": map[string]any{"n": 1}},
		},
	}); r.status != http.StatusOK {
		t.Fatalf("push the light record: %d %s", r.status, r.raw)
	}

	var first syncPull
	r := do(t, http.MethodGet, "/api/v1/sync?since=0", coach, nil)
	if err := json.Unmarshal(r.raw, &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(first.Records) != 1 || first.Records[0].ID != "heavy" {
		t.Fatalf("the oversized record should come back alone, got %d records", len(first.Records))
	}
	if first.Cursor == "" || first.Cursor == "0" {
		t.Fatal("the cursor must advance past it, or the client can never get past it")
	}

	var second syncPull
	r = do(t, http.MethodGet, "/api/v1/sync?since="+first.Cursor, coach, nil)
	if err := json.Unmarshal(r.raw, &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(second.Records) != 1 || second.Records[0].ID != "light" {
		t.Fatalf("the next page should carry the rest, got %d records", len(second.Records))
	}
}

// TestSyncRejectsACursorItCouldNotHaveIssued — a cursor is a sequence position, so the
// only shapes it takes are "absent" and "a non-negative integer". Anything else is a
// client bug, and answering it with a silent resync from the beginning (which is what
// this did) hides that bug behind the most expensive pull the API offers.
func TestSyncRejectsACursorItCouldNotHaveIssued(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "badcursor@e.com")

	for _, since := range []string{"abc", "-1", "1e3", "3.5", "0x10"} {
		r := do(t, http.MethodGet, "/api/v1/sync?since="+since, coach, nil)
		if r.status != http.StatusBadRequest {
			t.Errorf("since=%q: status %d, want 400 — body %s", since, r.status, r.raw)
		}
	}

	// An absent cursor still means "I have never synced", which is not an error.
	if r := do(t, http.MethodGet, "/api/v1/sync", coach, nil); r.status != http.StatusOK {
		t.Errorf("an absent cursor must still sync from the beginning: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodGet, "/api/v1/sync?since=0", coach, nil); r.status != http.StatusOK {
		t.Errorf("since=0 must still sync from the beginning: %d %s", r.status, r.raw)
	}
}

// TestSyncResyncsADeviceStrandedByARestore — the AUDIT-5 M2 defect, from the device's
// side. A pull used to seed its high-water mark with whatever cursor it was handed and
// raise it only over rows it delivered, so a cursor past the end of the sequence came
// back unchanged with an empty page — which is exactly the client's "you are up to
// date" condition. Every device that had synced past a restore point silently stepped
// over the records written into the seqs it had already passed, then resumed normally,
// so nothing ever surfaced the gap.
//
// The way in is the recovery path 0009 documents in its own header: "the way back is a
// database restore."
func TestSyncResyncsADeviceStrandedByARestore(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "restored@e.com")
	ctx := context.Background()

	push := func(ids ...string) {
		t.Helper()
		ups := []map[string]any{}
		for _, id := range ids {
			ups = append(ups, map[string]any{
				"type": "Note", "id": id, "payload": map[string]any{"v": id}})
		}
		if r := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
			"upserts": ups,
		}); r.status != http.StatusOK {
			t.Fatalf("push: %d %s", r.status, r.raw)
		}
	}
	// drain is the client loop: ask again from the cursor until it stops moving.
	drain := func(from string, seen map[string]bool) string {
		t.Helper()
		cursor := from
		for i := 0; i < 50; i++ {
			r := do(t, http.MethodGet, "/api/v1/sync?since="+cursor, coach, nil)
			if r.status != http.StatusOK {
				t.Fatalf("pull(since=%s): %d %s", cursor, r.status, r.raw)
			}
			var p syncPull
			if err := json.Unmarshal(r.raw, &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, rec := range p.Records {
				seen[rec.ID] = true
			}
			if p.Cursor == cursor {
				return cursor
			}
			cursor = p.Cursor
		}
		t.Fatal("drain did not terminate")
		return ""
	}

	device := map[string]bool{}
	push("a", "b", "c", "d", "e", "f")
	cursor := drain("0", device)

	// The restore: rows above seq 3 are gone and sync_seq resumes there. The device is
	// untouched and still holds a cursor from after that point.
	if _, err := testPool.Exec(ctx, `DELETE FROM sync_documents WHERE seq > 3`); err != nil {
		t.Fatalf("restore (rows): %v", err)
	}
	// setval with is_called=true is the shape pg_dump and PITR restore a sequence in:
	// last_value reads back as 3 and the next nextval returns 4. ALTER SEQUENCE RESTART
	// would leave last_value NULL, which is a state no real restore produces.
	if _, err := testPool.Exec(ctx, `SELECT setval('sync_seq', 3, true)`); err != nil {
		t.Fatalf("restore (sequence): %v", err)
	}

	// The device reconnects. Its cursor is above the sequence, so it is rewound and
	// resynced rather than told it is up to date.
	cursor = drain(cursor, device)
	if cursor != "3" {
		t.Fatalf("a stranded device should have been resynced to the restored high-water "+
			"mark, cursor is %q", cursor)
	}

	// The coach carries on. These take the seqs the device had already passed, and are
	// exactly what it used to skip.
	push("g", "h", "i")
	cursor = drain(cursor, device)
	push("j", "k")
	drain(cursor, device)

	// Everything a device installing today would see.
	fresh := map[string]bool{}
	drain("0", fresh)

	for id := range fresh {
		if !device[id] {
			t.Errorf("%s exists on the server but was never delivered to a device that "+
				"held a cursor from before the restore", id)
		}
	}
}

// TestSyncCannotSeeAStaleCursorOnceTheSequenceCatchesUp pins the edge of what the
// bounds check can do, so the next reader does not mistake it for a closed hole.
//
// The check compares the cursor against sync_seq. That only identifies a stale cursor
// while the sequence is still below it. If enough writes land after the restore to
// carry the sequence back past the cursor *before* the device reconnects, the cursor is
// indistinguishable from a legitimate one and the device silently skips the rows
// written into that window — the original M2 defect, in the narrower window the check
// leaves behind.
//
// Closing it needs something the server cannot derive from restored state alone, since
// a restore rewinds every record of what was issued along with the data. The two real
// answers are operational or a wire change, and both are written up in docs/AUDIT-5.md
// M2: bump sync_seq past the pre-restore high-water mark as part of the restore, or
// carry an epoch alongside the cursor.
//
// This test asserts the limitation rather than the fix. If it ever starts failing,
// something closed the window and this should become a fix test.
func TestSyncCannotSeeAStaleCursorOnceTheSequenceCatchesUp(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "caughtup@e.com")
	ctx := context.Background()

	push := func(ids ...string) {
		t.Helper()
		ups := []map[string]any{}
		for _, id := range ids {
			ups = append(ups, map[string]any{
				"type": "Note", "id": id, "payload": map[string]any{"v": id}})
		}
		if r := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
			"upserts": ups,
		}); r.status != http.StatusOK {
			t.Fatalf("push: %d %s", r.status, r.raw)
		}
	}

	push("a", "b", "c", "d", "e", "f")
	r := do(t, http.MethodGet, "/api/v1/sync?since=0", coach, nil)
	var first syncPull
	if err := json.Unmarshal(r.raw, &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stale := first.Cursor // "6"

	if _, err := testPool.Exec(ctx, `DELETE FROM sync_documents WHERE seq > 3`); err != nil {
		t.Fatalf("restore (rows): %v", err)
	}
	// setval with is_called=true is the shape pg_dump and PITR restore a sequence in:
	// last_value reads back as 3 and the next nextval returns 4. ALTER SEQUENCE RESTART
	// would leave last_value NULL, which is a state no real restore produces.
	if _, err := testPool.Exec(ctx, `SELECT setval('sync_seq', 3, true)`); err != nil {
		t.Fatalf("restore (sequence): %v", err)
	}

	// Writes land before the device reconnects, carrying sync_seq back up to 6.
	push("g", "h", "i")

	r = do(t, http.MethodGet, "/api/v1/sync?since="+stale, coach, nil)
	var after syncPull
	if err := json.Unmarshal(r.raw, &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(after.Records) != 0 {
		t.Fatalf("the window is closed — this test is now obsolete and should become a "+
			"fix test; got %d records", len(after.Records))
	}
	t.Logf("known gap: cursor %q survives because sync_seq caught back up to it; "+
		"g,h,i are skipped silently (docs/AUDIT-5.md M2)", stale)
}

// TestSyncPullDoesNotSpendTheByteBudgetOnTombstones — a delete goes on the wire as
// {type, id}, but the seven projected tables keep a tombstoned row's payload (see
// docs/AUDIT-5.md L1) and the pull query used to select it. Those bytes were read,
// charged against the page's byte budget, and then dropped, so a page of deletes came
// back a fraction of the size it could have been and cost the whole budget to build.
//
// Pinned by count: 200 large tombstones now arrive in one page. Under the old query
// their payloads alone were 50 MiB against a 2 MiB budget, so the page cut off after a
// handful of them and a client needed dozens of round trips to drain a pure-delete
// delta.
func TestSyncPullDoesNotSpendTheByteBudgetOnTombstones(t *testing.T) {
	resetDB(t)
	coach, personID := signInCoach(t, "tombbudget@e.com")
	ctx := context.Background()

	const tombstones = 200
	blob := strings.Repeat("x", 256<<10)
	for i := 0; i < tombstones; i++ {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO sync_documents (sync_account_id, type, id, payload, deleted, seq)
			VALUES ($1, 'Note', $2, $3::jsonb, true, nextval('sync_seq'))`,
			personID, fmt.Sprintf("gone-%04d", i), fmt.Sprintf(`{"b":%q}`, blob)); err != nil {
			t.Fatalf("plant tombstone %d: %v", i, err)
		}
	}

	r := do(t, http.MethodGet, "/api/v1/sync?since=0", coach, nil)
	if r.status != http.StatusOK {
		t.Fatalf("pull: %d %s", r.status, r.raw)
	}
	var p syncPull
	if err := json.Unmarshal(r.raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(p.Deletes) != tombstones {
		t.Errorf("one page carried %d of %d tombstones; a delete weighs {type, id} and "+
			"must not be charged for a payload it does not carry", len(p.Deletes), tombstones)
	}
	if len(p.Records) != 0 {
		t.Errorf("tombstones must not come back as records, got %d", len(p.Records))
	}
	// The response is the wire cost of 200 {type, id} pairs, not of 50 MiB of payload.
	if len(r.raw) > 64<<10 {
		t.Errorf("a page of %d tombstones weighed %d bytes on the wire; payloads are "+
			"leaking back into the response", tombstones, len(r.raw))
	}
}

// TestSyncTombstoneClearsWhatItHeld — a tombstoned row used to keep everything it had,
// for as long as it existed, with nothing to ever revisit it. For persons that meant a
// deleted athlete's medical notes and emergency contact stayed in the database
// indefinitely: a coach who deleted an athlete had not deleted the athlete's data.
// sync_documents already cleared its payload on write; the seven projected tables did
// not. See docs/AUDIT-5.md L1.
func TestSyncTombstoneClearsWhatItHeld(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "tombclear@e.com")
	ctx := context.Background()

	personID := "7e5a0000-0000-4000-8000-0000000000a1"
	push := func(body map[string]any) {
		t.Helper()
		if r := do(t, http.MethodPost, "/api/v1/sync", coach, body); r.status != http.StatusOK {
			t.Fatalf("push: %d %s", r.status, r.raw)
		}
	}
	push(map[string]any{"upserts": []map[string]any{{
		"type": "Person", "id": personID,
		"payload": map[string]any{
			"name":                  "Sam Athlete",
			"emergencyContactName":  "Pat Guardian",
			"emergencyContactPhone": "+1-555-0100",
			"medicalNotes":          "carries an epinephrine auto-injector",
		},
	}}})

	// Everything is stored while the row is live.
	var name string
	var contact, phone, notes, payload *string
	row := func() {
		t.Helper()
		if err := testPool.QueryRow(ctx, `
			SELECT display_name, emergency_contact_name, emergency_contact_phone,
			       medical_notes, payload::text
			FROM persons WHERE id = $1`, personID,
		).Scan(&name, &contact, &phone, &notes, &payload); err != nil {
			t.Fatalf("read persons row: %v", err)
		}
	}
	row()
	if name != "Sam Athlete" || contact == nil || notes == nil || payload == nil {
		t.Fatalf("precondition: the live row should hold everything, got %q %v %v %v",
			name, contact, notes, payload)
	}

	push(map[string]any{"deletes": []map[string]any{{"type": "Person", "id": personID}}})

	row()
	if name != "" {
		t.Errorf("display_name survived the tombstone: %q", name)
	}
	for label, got := range map[string]*string{
		"emergency_contact_name":  contact,
		"emergency_contact_phone": phone,
		"medical_notes":           notes,
		"payload":                 payload,
	} {
		if got != nil {
			t.Errorf("%s survived the tombstone: %q", label, *got)
		}
	}

	// The tombstone still does its job: the delete reaches a syncing client.
	var p syncPull
	r := do(t, http.MethodGet, "/api/v1/sync?since=0", coach, nil)
	if err := json.Unmarshal(r.raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawDelete bool
	for _, d := range p.Deletes {
		if d.ID == personID && d.Type == "Person" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Errorf("the tombstone must still be delivered as a delete: %s", r.raw)
	}

	// And clearing is reversible, because a tombstone clears exactly the columns its
	// upsert sets: pushing the record again restores every one of them.
	push(map[string]any{"upserts": []map[string]any{{
		"type": "Person", "id": personID,
		"payload": map[string]any{
			"name": "Sam Athlete", "medicalNotes": "carries an epinephrine auto-injector",
		},
	}}})
	row()
	if name != "Sam Athlete" || notes == nil || *notes != "carries an epinephrine auto-injector" {
		t.Errorf("re-pushing the record should restore what the tombstone cleared, got %q %v",
			name, notes)
	}
}

// TestRESTDeleteAlsoClearsWhatItHeld — DeleteTeam tombstones rather than dropping, so
// the deletion reaches sync clients. It has to clear the row for the same reason the
// sync path does, or which endpoint the coach happened to delete through decides whether
// the data stays behind.
func TestRESTDeleteAlsoClearsWhatItHeld(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "restclear@e.com")
	ctx := context.Background()

	create := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{
		"name": "U12 Rovers", "ageGroup": "U12", "season": "2026",
	})
	if create.status != http.StatusCreated && create.status != http.StatusOK {
		t.Fatalf("create team: %d %s", create.status, create.raw)
	}
	teamID, _ := create.body["id"].(string)
	if teamID == "" {
		t.Fatalf("no team id: %s", create.raw)
	}

	if r := do(t, http.MethodDelete, "/api/v1/teams/"+teamID, coach, nil); r.status/100 != 2 {
		t.Fatalf("delete team: %d %s", r.status, r.raw)
	}

	var name string
	var ageGroup, season, payload *string
	if err := testPool.QueryRow(ctx, `
		SELECT name, age_group, season, payload::text FROM teams WHERE id = $1`, teamID,
	).Scan(&name, &ageGroup, &season, &payload); err != nil {
		t.Fatalf("read teams row: %v", err)
	}
	if name != "" || ageGroup != nil || season != nil || payload != nil {
		t.Errorf("a REST delete left the row's content behind: name=%q ageGroup=%v season=%v payload=%v",
			name, ageGroup, season, payload)
	}
}

// TestSyncBoundsTypeAndIDAndPayload — the three client-chosen strings a push writes
// verbatim. Any type outside the seven projected ones becomes a sync_documents row
// carrying that type, and nothing bounded the type, the id, or a single payload: the
// only limit was the 4 MiB body. See docs/AUDIT-2.md L6 and docs/AUDIT-5.md M1 (3).
func TestSyncBoundsTypeAndIDAndPayload(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "bounds@e.com")

	push := func(body map[string]any) int {
		return do(t, http.MethodPost, "/api/v1/sync", coach, body).status
	}
	upsert := func(rec map[string]any) int {
		return push(map[string]any{"upserts": []map[string]any{rec}})
	}
	ok := map[string]any{"n": 1}

	// The types the app actually sends must all still be accepted — this is the
	// forward-compatibility property TestContractUnprojectedTypesRoundTrip pins, and the
	// reason this is a shape check rather than an allowlist of known types.
	for _, typ := range []string{
		"Team", "Prefs", "RosterMembership", "OrgMembership", "ShareGrant", "FormInstance",
		"Some_New_Type", "v2.Thing", "kebab-type",
	} {
		if st := upsert(map[string]any{"type": typ, "id": uuid.NewString(), "payload": ok}); st != http.StatusOK {
			t.Errorf("type %q must be accepted, got %d", typ, st)
		}
	}

	// Shapes that are not record type names.
	for _, typ := range []string{
		strings.Repeat("T", 65), "9Leading", "_leading", "has space", "has/slash", "emoji😀",
	} {
		if st := upsert(map[string]any{"type": typ, "id": uuid.NewString(), "payload": ok}); st != http.StatusBadRequest {
			t.Errorf("type %q should be rejected, got %d", typ, st)
		}
	}

	// An unbounded id is the other half of the same key.
	if st := upsert(map[string]any{
		"type": "Note", "id": strings.Repeat("i", 129), "payload": ok,
	}); st != http.StatusBadRequest {
		t.Errorf("an over-long id should be rejected, got %d", st)
	}
	if st := upsert(map[string]any{"type": "Note", "id": "prefs", "payload": ok}); st != http.StatusOK {
		t.Errorf("the fixed non-UUID id 'prefs' must still be accepted, got %d", st)
	}

	// A single payload is capped well under the body limit, because a pull's row limit
	// and byte budget only balance at ~4.2 KiB a record and everything above that makes
	// a page scan more rows than it delivers.
	under := strings.Repeat("x", 200<<10)
	if st := upsert(map[string]any{
		"type": "Note", "id": uuid.NewString(), "payload": map[string]any{"b": under},
	}); st != http.StatusOK {
		t.Errorf("a 200 KiB payload should be accepted, got %d", st)
	}
	over := strings.Repeat("x", 300<<10)
	r := do(t, http.MethodPost, "/api/v1/sync", coach, map[string]any{
		"upserts": []map[string]any{
			{"type": "Note", "id": "toobig", "payload": map[string]any{"b": over}},
		},
	})
	if r.status != http.StatusBadRequest {
		t.Errorf("a 300 KiB payload should be rejected, got %d", r.status)
	}
	// The error has to name the record: an offline-first client retries the batch it
	// failed to push, so a failure it cannot attribute stops that device syncing.
	if !strings.Contains(string(r.raw), "toobig") {
		t.Errorf("the rejection must name the record, got %s", r.raw)
	}

	// Deletes carry the same key and get the same bounds.
	if st := push(map[string]any{
		"deletes": []map[string]any{{"type": "has space", "id": "x"}},
	}); st != http.StatusBadRequest {
		t.Errorf("a delete's type should be bounded too, got %d", st)
	}
}

// TestSyncEveryProjectedTypeRoutes pushes and pulls one record of each of the seven
// projected types.
//
// It exists to pin the agreement applyUpsert and applyDelete now depend on: both read
// projectedSyncTypes to decide whether a record needs a UUID before routing it, so a
// type listed there but missing from a switch would answer "no upsert route", and a type
// in a switch but missing from the map would silently be filed as a document instead —
// splitting that type's records across two tables. Neither is reachable through the
// wire format, so only a test that walks the whole list can see it.
func TestSyncEveryProjectedTypeRoutes(t *testing.T) {
	resetDB(t)
	token := appleToken(t, "sub-routing", "routing@e.com")

	// One payload per projected type, carrying the fields its table projects out, and
	// the table it must land in. The table is the half that matters: a record routed to
	// sync_documents by mistake still pulls back under its own type name, because
	// sync_documents stores the client's type verbatim — so the wire cannot tell the two
	// apart and only the database can.
	types := []struct {
		name    string
		table   string
		payload map[string]any
	}{
		{"Team", "teams", map[string]any{"name": "U11", "ageGroup": "U11", "season": "2027"}},
		{"Drill", "drills", map[string]any{"title": "Rondo", "fieldSetup": "5v2"}},
		{"Session", "sessions", map[string]any{"title": "Tuesday", "objective": "Pressing"}},
		{"Person", "persons", map[string]any{"name": "Alex Kim", "medicalNotes": "none"}},
		{"Player", "players", map[string]any{"name": "Alex Kim", "number": 9, "position": "ST"}},
		{"Event", "events", map[string]any{"title": "Match", "kind": "game"}},
		{"Diagram", "diagrams", map[string]any{"title": "Shape"}},
	}

	ids := make(map[string]string, len(types))
	upserts := make([]map[string]any, 0, len(types))
	for _, ty := range types {
		id := uuid.NewString()
		ids[ty.name] = id
		upserts = append(upserts, map[string]any{"type": ty.name, "id": id, "payload": ty.payload})
	}

	push := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{"upserts": upserts})
	if push.status != http.StatusOK {
		t.Fatalf("push: status %d body %s", push.status, push.raw)
	}
	if conflicts, _ := push.body["conflicts"].([]any); len(conflicts) != 0 {
		t.Fatalf("push reported conflicts on fresh ids: %s", push.raw)
	}

	pulled := pullSync(t, token, "")
	got := make(map[string]string, len(pulled.Records))
	for _, rec := range pulled.Records {
		got[rec.Type] = rec.ID
	}
	for _, ty := range types {
		if got[ty.name] != ids[ty.name] {
			t.Errorf("%s did not round-trip: wanted id %s, pull returned %q",
				ty.name, ids[ty.name], got[ty.name])
		}
		var inTable, asDocument int
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM "+ty.table+" WHERE id = $1", ids[ty.name],
		).Scan(&inTable); err != nil {
			t.Fatalf("count %s rows: %v", ty.table, err)
		}
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM sync_documents WHERE type = $1 AND id = $2",
			ty.name, ids[ty.name],
		).Scan(&asDocument); err != nil {
			t.Fatalf("count sync_documents rows: %v", err)
		}
		if inTable != 1 || asDocument != 0 {
			t.Errorf("%s landed in the wrong place: %d row(s) in %s, %d in sync_documents "+
				"(want 1 and 0) — check that projectedSyncTypes and the applyUpsert switch "+
				"still list the same types", ty.name, inTable, ty.table, asDocument)
		}
	}

	// And the same list through the delete route, which shares the routing.
	deletes := make([]map[string]any, 0, len(types))
	for _, ty := range types {
		deletes = append(deletes, map[string]any{"type": ty.name, "id": ids[ty.name]})
	}
	del := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{"deletes": deletes})
	if del.status != http.StatusOK {
		t.Fatalf("delete push: status %d body %s", del.status, del.raw)
	}
	if conflicts, _ := del.body["conflicts"].([]any); len(conflicts) != 0 {
		t.Fatalf("tombstoning rows this account owns reported conflicts: %s", del.raw)
	}

	after := pullSync(t, token, "")
	tombstoned := make(map[string]bool, len(after.Deletes))
	for _, d := range after.Deletes {
		tombstoned[d.Type] = true
	}
	for _, ty := range types {
		if !tombstoned[ty.name] {
			t.Errorf("%s was not tombstoned by its delete route", ty.name)
		}
	}
}

// TestSyncIDShapeFollowsTheRoute pins the asymmetry the routing rests on: a projected
// type is keyed by a UUID column and must reject anything else, while sync_documents
// keys on the client's string exactly as it arrives and must accept it. Collapsing the
// per-branch UUID parse into one call before the switch is only safe while documents
// stay outside it.
func TestSyncIDShapeFollowsTheRoute(t *testing.T) {
	resetDB(t)
	token := appleToken(t, "sub-idshape", "idshape@e.com")

	bad := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": "not-a-uuid", "payload": map[string]any{"name": "U11"}},
		},
	})
	if bad.status != http.StatusBadRequest {
		t.Fatalf("projected type with a non-UUID id: status %d body %s", bad.status, bad.raw)
	}
	if !strings.Contains(string(bad.raw), "Team id must be a UUID") {
		t.Errorf("the error should name the offending type so the client can act on it, got %s", bad.raw)
	}

	// The same id on an unprojected type is a legitimate document key.
	ok := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{
			{"type": "Prefs", "id": "prefs", "payload": map[string]any{"theme": "dark"}},
		},
	})
	if ok.status != http.StatusOK {
		t.Fatalf("document type with a non-UUID id should be accepted: status %d body %s", ok.status, ok.raw)
	}
	pulled := pullSync(t, token, "")
	found := false
	for _, rec := range pulled.Records {
		if rec.Type == "Prefs" && rec.ID == "prefs" {
			found = true
		}
	}
	if !found {
		t.Errorf("the document did not round-trip: %+v", pulled.Records)
	}
}
