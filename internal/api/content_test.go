package api_test

import (
	"net/http"
	"testing"
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
