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

	// Nor write to it. This test used to assert only the read direction, which is how a
	// cross-tenant write sat in a passing suite: the REST path checked the org, and the
	// sync path — reaching the same table — did not.
	if r := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/games", coachB, map[string]any{
		"opponent": "Ghost FC",
	}); r.status != http.StatusForbidden {
		t.Errorf("cross-org game create should be 403, got %d %s", r.status, r.raw)
	}
	push := do(t, http.MethodPost, "/api/v1/sync", coachB, map[string]any{
		"upserts": []map[string]any{{"type": "Team", "id": teamID, "payload": map[string]any{"name": "Taken"}}},
	})
	if conflicts, _ := push.body["conflicts"].([]any); len(conflicts) != 1 {
		t.Errorf("cross-org sync write should be refused, conflicts=%v", push.body["conflicts"])
	}
	if after := do(t, http.MethodGet, "/api/v1/teams/"+teamID, coachA, nil); after.status != http.StatusOK {
		t.Fatalf("owner read after the attempts: %d %s", after.status, after.raw)
	} else if team, _ := after.body["team"].(map[string]any); team["name"] != "A Team" {
		t.Errorf("team was modified across the org boundary: %v", team)
	}
}

// TestPersonReadsAreScopedToTheOrg covers the read side of the org boundary. These
// endpoints return birthdate, contact details and medical notes, and none of them used
// to check anything beyond "is the caller authenticated".
func TestPersonReadsAreScopedToTheOrg(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "readA@e.com")
	coachB, _ := registerUser(t, "readB@e.com")

	create := do(t, http.MethodPost, "/api/v1/persons", coachA, map[string]any{
		"displayName": "Kid Athlete", "birthdate": "2015-04-01",
		"medicalNotes": "severe peanut allergy", "emergencyContactPhone": "+15550001111",
	})
	if create.status != http.StatusCreated {
		t.Fatalf("create person: %d %s", create.status, create.raw)
	}
	athlete, _ := create.body["id"].(string)

	for _, path := range []string{
		"/api/v1/persons/" + athlete,
		"/api/v1/persons/" + athlete + "/instances",
		"/api/v1/persons/" + athlete + "/aggregate",
	} {
		if r := do(t, http.MethodGet, path, coachB, nil); r.status != http.StatusNotFound {
			t.Errorf("GET %s as another org: got %d %s, want 404", path, r.status, r.raw)
		}
		// The owning coach still gets through.
		if r := do(t, http.MethodGet, path, coachA, nil); r.status != http.StatusOK {
			t.Errorf("GET %s as the owner: got %d %s, want 200", path, r.status, r.raw)
		}
	}
}

// TestRosterRejectsPersonOutsideTheOrg — existence is not authorization. Attaching an
// arbitrary Person id to your own team used to expose their name, email and birthdate
// through GET /teams/{id}, and left a roster link that made the athlete survive their
// own coach's account deletion.
func TestRosterRejectsPersonOutsideTheOrg(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "rosterA@e.com")
	coachB, _ := registerUser(t, "rosterB@e.com")

	athlete := createAthlete(t, coachA, "Minor Athlete")
	teamB := do(t, http.MethodPost, "/api/v1/teams", coachB, map[string]any{"name": "B Team"})
	teamBID, _ := teamB.body["id"].(string)

	add := do(t, http.MethodPost, "/api/v1/teams/"+teamBID+"/roster", coachB, map[string]any{"personId": athlete})
	if add.status != http.StatusNotFound {
		t.Fatalf("cross-org roster attach: got %d %s, want 404", add.status, add.raw)
	}
	team := do(t, http.MethodGet, "/api/v1/teams/"+teamBID, coachB, nil)
	if roster, _ := team.body["roster"].([]any); len(roster) != 0 {
		t.Errorf("attacker's roster should be empty, got %v", roster)
	}
}

// TestCreatePersonAlwaysLinksToTheOrg — a Person with no org linkage would be visible
// to nobody, including the coach who created them, so every created Person gets a
// membership and the endpoint refuses to mint privileged roles.
func TestCreatePersonAlwaysLinksToTheOrg(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "roles@e.com")

	parent := do(t, http.MethodPost, "/api/v1/persons", coach, map[string]any{
		"displayName": "A Parent", "role": "parent",
	})
	if parent.status != http.StatusCreated {
		t.Fatalf("create parent: %d %s", parent.status, parent.raw)
	}
	parentID, _ := parent.body["id"].(string)
	if r := do(t, http.MethodGet, "/api/v1/persons/"+parentID, coach, nil); r.status != http.StatusOK {
		t.Errorf("a person created with role=parent must stay readable, got %d %s", r.status, r.raw)
	}

	for _, role := range []string{"admin", "director", "coach", "wizard"} {
		r := do(t, http.MethodPost, "/api/v1/persons", coach, map[string]any{
			"displayName": "Nope", "role": role,
		})
		if r.status != http.StatusBadRequest {
			t.Errorf("role=%q should be rejected, got %d %s", role, r.status, r.raw)
		}
	}
}
