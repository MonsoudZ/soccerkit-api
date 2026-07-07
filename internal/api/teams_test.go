package api_test

import (
	"net/http"
	"testing"
)

func TestTeamAndTimeBoundedRoster(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "tcoach@e.com")
	athlete := createAthlete(t, coach, "Roster Kid")

	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{
		"name": "U11 Blue", "ageGroup": "U11", "season": "2026",
	})
	if team.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", team.status, team.raw)
	}
	teamID := team.body["id"].(string)

	// Add to roster.
	add := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coach, map[string]any{
		"personId": athlete, "jerseyNumber": 7, "position": "FWD",
	})
	if add.status != http.StatusCreated {
		t.Fatalf("add roster: %d %s", add.status, add.raw)
	}

	// Duplicate active membership rejected.
	if dup := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coach, map[string]any{"personId": athlete}); dup.status != http.StatusConflict {
		t.Errorf("expected 409 on duplicate active roster spot, got %d", dup.status)
	}

	// Team shows one active roster entry.
	detail := do(t, http.MethodGet, "/api/v1/teams/"+teamID, coach, nil)
	roster := detail.body["roster"].([]any)
	if len(roster) != 1 {
		t.Fatalf("expected 1 roster entry, got %d", len(roster))
	}

	// End the membership (player leaves / is moved).
	if end := do(t, http.MethodDelete, "/api/v1/teams/"+teamID+"/roster/"+athlete, coach, nil); end.status != http.StatusOK {
		t.Fatalf("end roster: %d %s", end.status, end.raw)
	}
	after := do(t, http.MethodGet, "/api/v1/teams/"+teamID, coach, nil)
	if len(after.body["roster"].([]any)) != 0 {
		t.Errorf("roster should be empty after ending membership")
	}

	// Re-adding after ending is allowed (history preserved, new spot opened).
	if re := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coach, map[string]any{"personId": athlete}); re.status != http.StatusCreated {
		t.Errorf("re-adding after end should succeed, got %d %s", re.status, re.raw)
	}
}

func TestTeamIsolatedByOrg(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "orgA@e.com")
	coachB, _ := registerUser(t, "orgB@e.com")

	team := do(t, http.MethodPost, "/api/v1/teams", coachA, map[string]any{"name": "A Team"})
	teamID := team.body["id"].(string)

	// Coach B (different personal org) cannot see or delete coach A's team.
	if r := do(t, http.MethodGet, "/api/v1/teams/"+teamID, coachB, nil); r.status != http.StatusForbidden {
		t.Errorf("cross-org team read should be 403, got %d", r.status)
	}
	if r := do(t, http.MethodDelete, "/api/v1/teams/"+teamID, coachB, nil); r.status != http.StatusForbidden {
		t.Errorf("cross-org team delete should be 403, got %d", r.status)
	}
}
