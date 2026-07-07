package api_test

import (
	"net/http"
	"testing"
)

func TestSessionWithBlocks(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "scoach@e.com")

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
	other, _ := registerUser(t, "sother@e.com")
	if r := do(t, http.MethodGet, "/api/v1/sessions/"+sessID, other, nil); r.status != http.StatusForbidden {
		t.Errorf("cross-org session read should be 403, got %d", r.status)
	}
}

func TestGameDayFlow(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "gcoach@e.com")
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
