package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSessionWithBlocks(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "scoach@e.com")

	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U13"})
	teamID := team.body["id"].(string)

	drill := do(t, http.MethodPost, "/api/v1/drills", coach, map[string]any{
		"name": "Rondo", "description": "5v2 keep-away",
	})
	if drill.status != http.StatusCreated {
		t.Fatalf("create drill: %d %s", drill.status, drill.raw)
	}
	drillID := drill.body["id"].(string)

	sess := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title":       "Tuesday Training",
		"teamId":      teamID,
		"scheduledAt": "2027-02-01T18:00:00Z",
		"blocks": []map[string]any{
			{"title": "Warm-up", "durationMin": 15},
			{"title": "Rondo", "drillId": drillID, "durationMin": 20},
		},
	})
	if sess.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", sess.status, sess.raw)
	}
	if len(sess.body["blocks"].([]any)) != 2 {
		t.Fatalf("expected 2 blocks, got %v", sess.body["blocks"])
	}
	sessID := sess.body["id"].(string)

	// Get session resolves the drill name on the block.
	got := do(t, http.MethodGet, "/api/v1/sessions/"+sessID, coach, nil)
	blocks := got.body["blocks"].([]any)
	var foundDrillName bool
	for _, b := range blocks {
		if name, ok := b.(map[string]any)["drillName"].(string); ok && name == "Rondo" {
			foundDrillName = true
		}
	}
	if !foundDrillName {
		t.Errorf("expected a block to resolve drillName 'Rondo', got %v", blocks)
	}

	// Session appears in the list, and is org-isolated from another coach.
	if list := do(t, http.MethodGet, "/api/v1/sessions", coach, nil); len(list.arr()) != 1 {
		t.Errorf("expected 1 session in list")
	}
	other, _ := signInCoach(t, "sother@e.com")
	if r := do(t, http.MethodGet, "/api/v1/sessions/"+sessID, other, nil); r.status != http.StatusForbidden {
		t.Errorf("cross-org session read should be 403, got %d", r.status)
	}
}

func TestGameDayFlow(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "gcoach@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "First XI"})
	teamID := team.body["id"].(string)

	// Schedule a game.
	game := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/games", coach, map[string]any{
		"opponent": "City Rovers", "kickoffAt": "2027-03-01T15:00:00Z", "homeAway": "home",
	})
	if game.status != http.StatusCreated || game.body["status"] != "scheduled" {
		t.Fatalf("create game: %d %s", game.status, game.raw)
	}
	gameID := game.body["id"].(string)

	// Record the result.
	upd := do(t, http.MethodPatch, "/api/v1/games/"+gameID, coach, map[string]any{
		"ourScore": 3, "opponentScore": 1, "status": "completed",
	})
	if upd.status != http.StatusOK {
		t.Fatalf("update game: %d %s", upd.status, upd.raw)
	}
	if upd.body["ourScore"].(float64) != 3 || upd.body["status"] != "completed" {
		t.Errorf("unexpected game after result: %v", upd.body)
	}

	// One-sided score is rejected.
	if bad := do(t, http.MethodPatch, "/api/v1/games/"+gameID, coach, map[string]any{"ourScore": 2}); bad.status != http.StatusBadRequest {
		t.Errorf("one-sided score should 400, got %d", bad.status)
	}

	// Post-game evaluation referencing the game via context_ref.
	athlete := createAthlete(t, coach, "Striker")
	postGame := templateID(t, coach, "post_game")
	inst := do(t, http.MethodPost, "/api/v1/form-instances", coach, map[string]any{
		"templateId":      postGame,
		"subjectPersonId": athlete,
		"contextRefType":  "game",
		"contextRefId":    gameID,
		"answers": []map[string]any{
			{"key": "effort", "numericValue": 5},
			{"key": "goals", "numericValue": 2},
		},
	})
	if inst.status != http.StatusCreated {
		t.Fatalf("post-game instance: %d %s", inst.status, inst.raw)
	}
	if inst.body["contextRefType"] != "game" || inst.body["contextRefId"] != gameID {
		t.Errorf("instance should reference the game, got %v", inst.body)
	}
}

// TestUpdateGameRejectsWrongTypes — the previous decode marked a field as "supplied"
// on mere presence and assigned it only if the type matched, so a wrong-typed value
// wrote NULL and skipped its own validation. A malformed client request returned 200
// and destroyed the recorded result of a match, with no undo.
func TestUpdateGameRejectsWrongTypes(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "patchgame@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "T"})
	teamID, _ := team.body["id"].(string)
	created := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/games", coach, map[string]any{
		"opponent": "Rivals FC", "homeAway": "home",
	})
	gameID, _ := created.body["id"].(string)

	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, coach, map[string]any{
		"ourScore": 3, "opponentScore": 1, "status": "completed",
	}); r.status != http.StatusOK {
		t.Fatalf("record result: %d %s", r.status, r.raw)
	}

	for _, bad := range []map[string]any{
		{"opponent": 12345},
		{"homeAway": true},
		{"homeAway": "sideways"},
		{"ourScore": "x", "opponentScore": "y"},
		{"ourScore": 1.5, "opponentScore": 2},
		{"kickoffAt": 99},
		{"status": 7},
		{"totallyUnknownField": 1},
	} {
		if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, coach, bad); r.status != http.StatusBadRequest {
			t.Errorf("PATCH %v: got %d %s, want 400", bad, r.status, r.raw)
		}
	}

	// Nothing above touched the stored game.
	after := do(t, http.MethodGet, "/api/v1/games/"+gameID, coach, nil)
	if after.body["opponent"] != "Rivals FC" || after.body["homeAway"] != "home" {
		t.Errorf("opponent/homeAway changed: %s", after.raw)
	}
	if after.body["ourScore"] != float64(3) || after.body["opponentScore"] != float64(1) {
		t.Errorf("score changed: %s", after.raw)
	}

	// An explicit null still clears a nullable column — absence and null differ.
	clear := do(t, http.MethodPatch, "/api/v1/games/"+gameID, coach, map[string]any{"opponent": nil})
	if clear.status != http.StatusOK {
		t.Fatalf("explicit null: %d %s", clear.status, clear.raw)
	}
	if clear.body["opponent"] != nil {
		t.Errorf("opponent should be cleared, got %s", clear.raw)
	}
	if clear.body["ourScore"] != float64(3) {
		t.Errorf("clearing opponent must not disturb the score: %s", clear.raw)
	}
}

// TestGameKickoffCanBeCleared — a cancelled fixture's kickoff time could not be unset.
// UpdateGame read kickoff_at as COALESCE(narg, kickoff_at), which treats NULL as "leave
// it alone", so there was no value that meant "clear it"; the handler made it moot
// anyway by unmarshalling JSON null into a string, which is a silent no-op that leaves
// "" behind and then fails RFC3339 parsing. See docs/AUDIT-2.md L3.
func TestGameKickoffCanBeCleared(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "kickoff@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "Clearables"})
	teamID := team.body["id"].(string)

	game := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/games", coach, map[string]any{
		"opponent": "Postponed FC", "kickoffAt": "2027-03-01T15:00:00Z",
	})
	if game.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", game.status, game.raw)
	}
	gameID := game.body["id"].(string)

	// An unrelated PATCH must leave it alone — the set-flag's other half.
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, coach, map[string]any{
		"opponent": "Postponed United",
	}); r.status != http.StatusOK {
		t.Fatalf("patch opponent: %d %s", r.status, r.raw)
	}
	if got := do(t, http.MethodGet, "/api/v1/games/"+gameID, coach, nil); got.body["kickoffAt"] == nil {
		t.Fatalf("an unrelated PATCH cleared kickoffAt: %s", got.raw)
	}

	// Explicit null clears it.
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, coach, map[string]any{
		"kickoffAt": nil,
	}); r.status != http.StatusOK {
		t.Fatalf("clear kickoffAt: %d %s", r.status, r.raw)
	}
	got := do(t, http.MethodGet, "/api/v1/games/"+gameID, coach, nil)
	if got.body["kickoffAt"] != nil {
		t.Errorf("kickoffAt should be null after clearing, got %v", got.body["kickoffAt"])
	}
}

// TestSessionRepeatsOneDrillAcrossBlocks covers a session whose blocks point at the
// same drill twice. The org check asks about each *distinct* drill once and compares
// the count it gets back with the number it asked about, so counting the references
// instead of the distinct ids would reject this perfectly ordinary session.
func TestSessionRepeatsOneDrillAcrossBlocks(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "repeatdrill@e.com")

	drill := do(t, http.MethodPost, "/api/v1/drills", coach, map[string]any{"name": "Rondo"})
	if drill.status != http.StatusCreated {
		t.Fatalf("create drill: %d %s", drill.status, drill.raw)
	}
	drillID := drill.body["id"].(string)

	sess := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Rondo Twice",
		"blocks": []map[string]any{
			{"title": "First rondo", "drillId": drillID, "durationMin": 10},
			{"title": "Water", "durationMin": 5},
			{"title": "Second rondo", "drillId": drillID, "durationMin": 10},
		},
	})
	if sess.status != http.StatusCreated {
		t.Fatalf("a session may use one drill in two blocks: %d %s", sess.status, sess.raw)
	}

	// Each block must carry the drill it was given — the ids are parsed once now and
	// carried to the insert by position, so an off-by-one would show up here.
	got := do(t, http.MethodGet, "/api/v1/sessions/"+sess.body["id"].(string), coach, nil)
	if got.status != http.StatusOK {
		t.Fatalf("get session: %d %s", got.status, got.raw)
	}
	blocks := got.body["blocks"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d: %s", len(blocks), got.raw)
	}
	want := []string{drillID, "", drillID}
	for i, b := range blocks {
		block := b.(map[string]any)
		id, _ := block["drillId"].(string)
		if id != want[i] {
			t.Errorf("block %d (%v) has drillId %q, want %q", i, block["title"], id, want[i])
		}
	}
}

// TestSessionRejectsDrillFromAnotherOrg is the negative half of the same check: the
// single count query replaced a per-block GetDrill, and it has to keep rejecting a
// drill id that resolves to a row in someone else's organization.
func TestSessionRejectsDrillFromAnotherOrg(t *testing.T) {
	resetDB(t)
	mine, _ := signInCoach(t, "mine@e.com")
	theirs, _ := signInCoach(t, "theirs@e.com")

	foreign := do(t, http.MethodPost, "/api/v1/drills", theirs, map[string]any{"name": "Their Drill"})
	if foreign.status != http.StatusCreated {
		t.Fatalf("create foreign drill: %d %s", foreign.status, foreign.raw)
	}
	foreignID := foreign.body["id"].(string)

	sess := do(t, http.MethodPost, "/api/v1/sessions", mine, map[string]any{
		"title":  "Borrowed",
		"blocks": []map[string]any{{"title": "Nope", "drillId": foreignID}},
	})
	if sess.status != http.StatusBadRequest {
		t.Fatalf("another org's drill should be rejected, got %d %s", sess.status, sess.raw)
	}

	// A mix of one valid and one foreign drill is rejected as a whole, so a session is
	// never half-created around a reference it was not allowed to make.
	ownDrill := do(t, http.MethodPost, "/api/v1/drills", mine, map[string]any{"name": "My Drill"})
	mixed := do(t, http.MethodPost, "/api/v1/sessions", mine, map[string]any{
		"title": "Mixed",
		"blocks": []map[string]any{
			{"title": "Fine", "drillId": ownDrill.body["id"].(string)},
			{"title": "Nope", "drillId": foreignID},
		},
	})
	if mixed.status != http.StatusBadRequest {
		t.Fatalf("a batch containing a foreign drill should be rejected, got %d %s", mixed.status, mixed.raw)
	}
	list := do(t, http.MethodGet, "/api/v1/sessions", mine, nil)
	if n := len(list.arr()); n != 0 {
		t.Errorf("no session should have been created, found %d", n)
	}
}

// --- editing a session -------------------------------------------------------

// TestSessionCanBeMovedAndRenamed is the gap this closes. Sessions could be created and
// deleted but never edited, so a coach moving Tuesday training to Thursday had to delete
// it and build it again — and since a session carries a register now, that threw away
// everyone's reply with it.
func TestSessionCanBeMovedAndRenamed(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "sess-edit@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U13"})
	teamID := team.body["id"].(string)

	created := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Tuesdya", "teamId": teamID, "scheduledAt": "2026-06-02T18:00:00Z",
		"notes": "shape work", "blocks": []map[string]any{{"title": "Warm-up"}},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	id := created.body["id"].(string)

	moved := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach, map[string]any{
		"title": "Thursday", "scheduledAt": "2026-06-04T19:30:00Z",
	})
	if moved.status != http.StatusOK {
		t.Fatalf("patch: %d %s", moved.status, moved.raw)
	}
	if moved.body["title"] != "Thursday" || moved.body["scheduledAt"] != "2026-06-04T19:30:00Z" {
		t.Errorf("the edit should be reflected back: %s", moved.raw)
	}
	// Untouched fields stay put, and the plan is not rewritten by a rename.
	if moved.body["notes"] != "shape work" {
		t.Errorf("an absent key must leave its field alone: %s", moved.raw)
	}
	blocks, _ := moved.body["blocks"].([]any)
	if len(blocks) != 1 {
		t.Errorf("editing a session must not touch its blocks: %s", moved.raw)
	}

	// Explicit null clears; the session keeps its identity with no time on it.
	cleared := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach, map[string]any{
		"scheduledAt": nil, "notes": nil,
	})
	if cleared.status != http.StatusOK {
		t.Fatalf("clear: %d %s", cleared.status, cleared.raw)
	}
	if cleared.body["scheduledAt"] != nil || cleared.body["notes"] != nil {
		t.Errorf("null should clear these: %s", cleared.raw)
	}
	// And the usual PATCH hygiene.
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"blocks": []any{}}); r.status != http.StatusBadRequest {
		t.Errorf("blocks are not editable here, expected 400, got %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"title": ""}); r.status != http.StatusBadRequest {
		t.Errorf("a session cannot be renamed to nothing, expected 400, got %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"scheduledAt": "next tuesday"}); r.status != http.StatusBadRequest {
		t.Errorf("expected 400 for an unparseable time, got %d %s", r.status, r.raw)
	}
}

// TestSessionEditReachesTheApp — a REST edit that only touched the columns would be
// invisible on the phone, and the app's next push would write the old values back over
// it. Both halves are written, so a pull carries the change.
func TestSessionEditReachesTheApp(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "sess-sync@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U13"})
	teamID := team.body["id"].(string)
	created := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Old title", "teamId": teamID, "scheduledAt": "2026-06-02T18:00:00Z",
		"notes": "old objective", "blocks": []map[string]any{},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	id := created.body["id"].(string)

	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach, map[string]any{
		"title": "New title", "notes": "new objective", "scheduledAt": "2026-06-04T19:30:00Z",
	}); r.status != http.StatusOK {
		t.Fatalf("patch: %d %s", r.status, r.raw)
	}

	pull := pullSync(t, coach, "")
	if got := payloadField(t, pull, "Session", "title"); got != "New title" {
		t.Errorf("the app would still see %q", got)
	}
	// The app calls it the objective; this API calls it notes. Same field.
	if got := payloadField(t, pull, "Session", "objective"); got != "new objective" {
		t.Errorf("the objective did not reach the payload, got %q", got)
	}
	// The date is a Swift Date — seconds since 2001 — so it arrives as a number, not a
	// string, and it has to move with the column.
	var record map[string]any
	for _, rec := range pull.Records {
		if rec.Type == "Session" {
			if err := json.Unmarshal(rec.Payload, &record); err != nil {
				t.Fatalf("decode session payload: %v", err)
			}
		}
	}
	date, ok := record["date"].(float64)
	if !ok {
		t.Fatalf("the session's date should be a number, got %T in %v", record["date"], record)
	}
	want := time.Date(2026, time.June, 4, 19, 30, 0, 0, time.UTC).
		Sub(time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)).Seconds()
	if date != want {
		t.Errorf("the payload date is %v, want %v", date, want)
	}
	// A cleared time still leaves a date the app can decode — its record requires one,
	// and a missing key loses the whole session.
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"scheduledAt": nil}); r.status != http.StatusOK {
		t.Fatalf("clear: %d %s", r.status, r.raw)
	}
	after := pullSync(t, coach, "")
	for _, rec := range after.Records {
		if rec.Type != "Session" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Payload, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := m["date"].(float64); !ok {
			t.Errorf("a session with no time still needs a decodable date, got %v", m["date"])
		}
	}
}

// TestSessionCannotChangeTeamOnceItHasARegister — those replies are about a specific
// squad's training. Carrying them to another team would attribute them to people who
// were never asked.
func TestSessionCannotChangeTeamOnceItHasARegister(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "sess-team@e.com")
	one := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U13"})
	two := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U15"})
	oneID, twoID := one.body["id"].(string), two.body["id"].(string)
	athlete := createAthlete(t, coach, "Register Kid")
	if r := do(t, http.MethodPost, "/api/v1/teams/"+oneID+"/roster", coach,
		map[string]any{"personId": athlete}); r.status != http.StatusCreated {
		t.Fatalf("roster: %d %s", r.status, r.raw)
	}
	created := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Tuesday", "teamId": oneID, "blocks": []map[string]any{},
	})
	id := created.body["id"].(string)

	// Before anyone has answered, a mistyped team is worth being able to fix.
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"teamId": twoID}); r.status != http.StatusOK {
		t.Fatalf("moving an unanswered session should work: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"teamId": oneID}); r.status != http.StatusOK {
		t.Fatalf("move back: %d %s", r.status, r.raw)
	}

	// Once the register has an answer, it is somebody's statement about this squad.
	if r := do(t, http.MethodPut, "/api/v1/sessions/"+id+"/rsvp", coach,
		map[string]any{"personId": athlete, "status": "going"}); r.status != http.StatusOK {
		t.Fatalf("rsvp: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"teamId": twoID}); r.status != http.StatusConflict {
		t.Errorf("expected 409 moving an answered session, got %d %s", r.status, r.raw)
	}
	// Everything else about it is still editable.
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, coach,
		map[string]any{"title": "Tuesday (moved indoors)"}); r.status != http.StatusOK {
		t.Errorf("a rename should still work: %d %s", r.status, r.raw)
	}
}

// TestSessionEditIsScopedToTheOrg — the same tenancy check every other route makes.
func TestSessionEditIsScopedToTheOrg(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "sess-mine@e.com")
	stranger, _ := signInCoach(t, "sess-theirs@e.com")
	created := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Tuesday", "blocks": []map[string]any{},
	})
	id := created.body["id"].(string)
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, stranger,
		map[string]any{"title": "PWNED"}); r.status != http.StatusForbidden {
		t.Errorf("cross-org session edit should be 403, got %d %s", r.status, r.raw)
	}
}
