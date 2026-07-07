package api_test

import (
	"net/http"
	"testing"
)

func makeTeam(t *testing.T, token, name string) string {
	t.Helper()
	r := do(t, http.MethodPost, "/api/v1/teams", token, map[string]any{"name": name})
	if r.status != http.StatusCreated {
		t.Fatalf("create team %s failed: %d %s", name, r.status, r.raw)
	}
	return r.body["id"].(string)
}

func TestStandingsComputation(t *testing.T) {
	resetDB(t)
	owner, _ := registerUser(t, "owner@e.com")

	lions := makeTeam(t, owner, "Lions")
	tigers := makeTeam(t, owner, "Tigers")
	bears := makeTeam(t, owner, "Bears")

	league := do(t, http.MethodPost, "/api/v1/leagues", owner, map[string]any{"name": "Cup", "season": "2026"})
	lid := league.body["id"].(string)
	for _, id := range []string{lions, tigers, bears} {
		do(t, http.MethodPost, "/api/v1/leagues/"+lid+"/teams", owner, map[string]any{"teamId": id})
	}

	fixture := func(home, away string) string {
		r := do(t, http.MethodPost, "/api/v1/leagues/"+lid+"/fixtures", owner, map[string]any{
			"homeTeamId": home, "awayTeamId": away, "kickoffAt": "2027-01-01T15:00:00Z",
		})
		return r.body["id"].(string)
	}
	result := func(fid string, hs, as int) {
		do(t, http.MethodPut, "/api/v1/leagues/fixtures/"+fid+"/result", owner, map[string]any{
			"homeScore": hs, "awayScore": as,
		})
	}
	result(fixture(lions, tigers), 3, 1) // Lions win
	result(fixture(tigers, bears), 2, 2) // draw

	standings := do(t, http.MethodGet, "/api/v1/leagues/"+lid+"/standings", "", nil).arr()
	first := standings[0].(map[string]any)
	if first["teamName"] != "Lions" || first["points"].(float64) != 3 || first["goalDifference"].(float64) != 2 {
		t.Errorf("Lions should top the table with 3 pts, GD 2; got %v", first)
	}

	// Bears (GD 0, 1pt) must rank above Tigers (GD -1, 1pt).
	pos := map[string]int{}
	for i, row := range standings {
		pos[row.(map[string]any)["teamName"].(string)] = i
	}
	if pos["Bears"] >= pos["Tigers"] {
		t.Errorf("Bears should rank above Tigers on goal difference")
	}
}

func TestFixtureRejectsTeamNotInLeague(t *testing.T) {
	resetDB(t)
	owner, _ := registerUser(t, "owner2@e.com")
	inLeague := makeTeam(t, owner, "In")
	outsider := makeTeam(t, owner, "Out")

	league := do(t, http.MethodPost, "/api/v1/leagues", owner, map[string]any{"name": "L", "season": "2026"})
	lid := league.body["id"].(string)
	do(t, http.MethodPost, "/api/v1/leagues/"+lid+"/teams", owner, map[string]any{"teamId": inLeague})

	r := do(t, http.MethodPost, "/api/v1/leagues/"+lid+"/fixtures", owner, map[string]any{
		"homeTeamId": inLeague, "awayTeamId": outsider, "kickoffAt": "2027-01-01T15:00:00Z",
	})
	if r.status != http.StatusBadRequest {
		t.Errorf("fixture with non-league team should 400, got %d", r.status)
	}
}

func TestTeamOwnerAuthorization(t *testing.T) {
	resetDB(t)
	owner, _ := registerUser(t, "towner@e.com")
	other, _ := registerUser(t, "tother@e.com")
	tid := makeTeam(t, owner, "Reds")

	if r := do(t, http.MethodDelete, "/api/v1/teams/"+tid, other, nil); r.status != http.StatusForbidden {
		t.Errorf("non-owner delete should 403, got %d", r.status)
	}
	if r := do(t, http.MethodDelete, "/api/v1/teams/"+tid, owner, nil); r.status != http.StatusOK {
		t.Errorf("owner delete should 200, got %d", r.status)
	}
}
