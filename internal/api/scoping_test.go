package api_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

// teamNames pulls the names out of a GET /teams response.
func teamNames(r resp) []string {
	var out []string
	for _, t := range r.arr() {
		out = append(out, t.(map[string]any)["name"].(string))
	}
	return out
}

// TestTeamListIsScopedByRole is the permission matrix's seeEveryTeam, which the schema
// could not express until team_staff existed: a person's only link to a team was the
// athlete roster, so "the teams this coach coaches" had no answer and every member got
// the whole club.
func TestTeamListIsScopedByRole(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "scope-admin@e.com")
	orgID := orgOf(t, admin)
	coach, coachID := signInCoach(t, "scope-coach@e.com")
	joinOrg(t, admin, orgID, coach, "scope-coach@e.com", "coach")

	// Two teams; the coach staffs one of them.
	mine := do(t, http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": "U12 Mine"})
	theirs := do(t, http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": "U14 Theirs"})
	if theirs.status != http.StatusCreated {
		t.Fatalf("create teams: %d %s", theirs.status, theirs.raw)
	}
	mineID := mine.body["id"].(string)
	if r := do(t, http.MethodPost, "/api/v1/teams/"+mineID+"/staff", admin,
		map[string]any{"personId": coachID}); r.status != http.StatusCreated {
		t.Fatalf("assign staff: %d %s", r.status, r.raw)
	}

	// The admin created both, so they staff both — and would see both regardless.
	if got := teamNames(do(t, http.MethodGet, "/api/v1/teams", admin, nil)); len(got) != 2 {
		t.Errorf("an admin sees the organization, got %v", got)
	}

	got := teamNames(doIn(t, http.MethodGet, "/api/v1/teams", coach, orgID, nil))
	if len(got) != 1 || got[0] != "U12 Mine" {
		t.Errorf("a coach sees the teams they staff, got %v", got)
	}
}

// TestAPlayerSeesOnlyTheirOwnTeams — the same scoping from the other end. A player used
// to get the club's whole team list, which is the shape of an organization they have one
// team in.
func TestAPlayerSeesOnlyTheirOwnTeams(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "pt-admin@e.com")
	orgID := orgOf(t, admin)
	player, playerID := signInCoach(t, "pt-player@e.com")
	joinOrg(t, admin, orgID, player, "pt-player@e.com", "player")

	mine := do(t, http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": "U12 Mine"})
	do(t, http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": "U14 Other"})
	mineID := mine.body["id"].(string)
	if r := do(t, http.MethodPost, "/api/v1/teams/"+mineID+"/roster", admin,
		map[string]any{"personId": playerID, "jerseyNumber": 9, "position": "ST"}); r.status != http.StatusCreated {
		t.Fatalf("roster the player: %d %s", r.status, r.raw)
	}

	got := teamNames(doIn(t, http.MethodGet, "/api/v1/teams", player, orgID, nil))
	if len(got) != 1 || got[0] != "U12 Mine" {
		t.Errorf("a player sees the teams they play for, got %v", got)
	}

	// And the membership detail they could not get at all before.
	mineTeams := doIn(t, http.MethodGet, "/api/v1/me/teams", player, orgID, nil)
	if mineTeams.status != http.StatusOK {
		t.Fatalf("me/teams: %d %s", mineTeams.status, mineTeams.raw)
	}
	rows := mineTeams.arr()
	if len(rows) != 1 {
		t.Fatalf("expected one membership, got %s", mineTeams.raw)
	}
	row := rows[0].(map[string]any)
	if row["teamName"] != "U12 Mine" || row["jerseyNumber"].(float64) != 9 || row["position"] != "ST" {
		t.Errorf("the membership detail is the point of this endpoint: %v", row)
	}
}

// TestAParentSeesTheirChildsTeam — a parent is connected to a team through a child, not
// through a roster row of their own.
func TestAParentSeesTheirChildsTeam(t *testing.T) {
	resetDB(t)
	c := newClub(t, "pts")

	got := teamNames(doIn(t, http.MethodGet, "/api/v1/teams", c.parent, c.orgID, nil))
	if len(got) != 1 || got[0] != "U12" {
		t.Errorf("a parent sees the teams their children are on, got %v", got)
	}
	// Their own roster is empty — they do not play.
	if rows := doIn(t, http.MethodGet, "/api/v1/me/teams", c.parent, c.orgID, nil).arr(); len(rows) != 0 {
		t.Errorf("a parent is on no roster themselves, got %v", rows)
	}
}

// TestTheCoachingLibraryIsStaffOnly is the matrix's seeSharedLibrary. Drills, session
// plans and evaluation templates were readable by anyone holding any membership, so a
// player could read every drill and every session plan in the club.
func TestTheCoachingLibraryIsStaffOnly(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "lib-admin@e.com")
	orgID := orgOf(t, admin)
	player, _ := signInCoach(t, "lib-player@e.com")
	joinOrg(t, admin, orgID, player, "lib-player@e.com", "player")

	drill := do(t, http.MethodPost, "/api/v1/drills", admin, map[string]any{"name": "Rondo"})
	session := do(t, http.MethodPost, "/api/v1/sessions", admin, map[string]any{
		"title": "Tuesday", "blocks": []map[string]any{},
	})
	if session.status != http.StatusCreated || drill.status != http.StatusCreated {
		t.Fatalf("seed library: %d %d", drill.status, session.status)
	}

	for _, path := range []string{
		"/api/v1/drills",
		"/api/v1/sessions",
		"/api/v1/sessions/" + session.body["id"].(string),
		"/api/v1/templates",
	} {
		r := doIn(t, http.MethodGet, path, player, orgID, nil)
		if r.status != http.StatusForbidden {
			t.Errorf("%s should be staff-only, got %d %s", path, r.status, r.raw)
		}
	}
	// Staff still read it.
	if r := do(t, http.MethodGet, "/api/v1/drills", admin, nil); r.status != http.StatusOK {
		t.Errorf("staff must still see the library, got %d %s", r.status, r.raw)
	}
}

// TestOnlyAnAdminOrDirectorStandardizesTemplates — a template is the club's measuring
// stick, so a coach who could add one could change what every other coach's athletes are
// scored against.
func TestOnlyAnAdminOrDirectorStandardizesTemplates(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "tpl-admin@e.com")
	orgID := orgOf(t, admin)
	coach, _ := signInCoach(t, "tpl-coach@e.com")
	joinOrg(t, admin, orgID, coach, "tpl-coach@e.com", "coach")

	body := map[string]any{
		"context": "pre_game", "name": "Check-In",
		"fields": []map[string]any{{"key": "sleep", "label": "Sleep", "kind": "scale"}},
	}
	if r := doIn(t, http.MethodPost, "/api/v1/templates", coach, orgID, body); r.status != http.StatusForbidden {
		t.Errorf("a coach must not standardize templates, got %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPost, "/api/v1/templates", admin, body); r.status != http.StatusCreated {
		t.Errorf("an admin may, got %d %s", r.status, r.raw)
	}
}

// TestTeamStaffAssignmentIsGatedAndChecked covers the two rules on assigning staff: it
// takes the member-management gate, and the person must already hold a staff role — this
// says which teams someone coaches, not that they coach.
func TestTeamStaffAssignmentIsGatedAndChecked(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "ts-admin@e.com")
	orgID := orgOf(t, admin)
	coach, coachID := signInCoach(t, "ts-coach@e.com")
	joinOrg(t, admin, orgID, coach, "ts-coach@e.com", "coach")
	player, playerID := signInCoach(t, "ts-player@e.com")
	joinOrg(t, admin, orgID, player, "ts-player@e.com", "player")

	team := do(t, http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": "U12"})
	teamID := team.body["id"].(string)

	t.Run("a coach cannot assign staff", func(t *testing.T) {
		r := doIn(t, http.MethodPost, "/api/v1/teams/"+teamID+"/staff", coach, orgID,
			map[string]any{"personId": coachID})
		if r.status != http.StatusForbidden {
			t.Errorf("expected 403, got %d %s", r.status, r.raw)
		}
	})

	t.Run("a player cannot be made team staff", func(t *testing.T) {
		r := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/staff", admin,
			map[string]any{"personId": playerID})
		if r.status != http.StatusBadRequest {
			t.Errorf("expected 400, got %d %s", r.status, r.raw)
		}
	})

	t.Run("an admin assigns a coach, and it shows and reverses", func(t *testing.T) {
		if r := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/staff", admin,
			map[string]any{"personId": coachID}); r.status != http.StatusCreated {
			t.Fatalf("assign: %d %s", r.status, r.raw)
		}
		listed := do(t, http.MethodGet, "/api/v1/teams/"+teamID+"/staff", admin, nil)
		if len(listed.arr()) != 2 {
			t.Errorf("expected the creator and the new coach, got %s", listed.raw)
		}
		if got := teamNames(doIn(t, http.MethodGet, "/api/v1/teams", coach, orgID, nil)); len(got) != 1 {
			t.Errorf("the coach should now see the team, got %v", got)
		}
		if r := do(t, http.MethodDelete, "/api/v1/teams/"+teamID+"/staff/"+coachID, admin, nil); r.status != http.StatusOK {
			t.Fatalf("remove: %d %s", r.status, r.raw)
		}
		if got := teamNames(doIn(t, http.MethodGet, "/api/v1/teams", coach, orgID, nil)); len(got) != 0 {
			t.Errorf("and stop seeing it, got %v", got)
		}
	})
}

// TestATeamCreatedOnThePhoneIsStaffedByItsCoach — teams pushed through sync have to
// register their staff too, or GET /teams would answer "you have none" for the very
// teams a coach made on their own device.
func TestATeamCreatedOnThePhoneIsStaffedByItsCoach(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "sync-staff-admin@e.com")
	orgID := orgOf(t, admin)
	coach, _ := signInCoach(t, "sync-staff-coach@e.com")
	joinOrg(t, admin, orgID, coach, "sync-staff-coach@e.com", "coach")

	teamID := "6d000000-0000-4000-8000-00000000ee01"
	if r := doIn(t, http.MethodPost, "/api/v1/sync", coach, orgID, map[string]any{
		"upserts": []map[string]any{
			{"type": "Team", "id": teamID, "payload": map[string]any{"name": "Phone Team"}},
		},
	}); r.status != http.StatusOK {
		t.Fatalf("push: %d %s", r.status, r.raw)
	}

	got := teamNames(doIn(t, http.MethodGet, "/api/v1/teams", coach, orgID, nil))
	if len(got) != 1 || got[0] != "Phone Team" {
		t.Errorf("a team pushed from the app must be visible to its coach over REST, got %v", got)
	}
}

// TestTheTeamStaffBackfillRestoresExistingTeams exercises the one part of 0013 that a
// fresh database can never reach.
//
// Scoping GET /teams to team_staff on an empty table answers "no teams" to every coach in
// existence — the app's main screen going blank on deploy. The backfill is what prevents
// that, and it runs exactly once against data this suite does not have: every team here
// is created through an endpoint that registers its staff on the way in. So the test
// makes the pre-migration state by hand and runs the migration's own statement against
// it, read out of the file rather than restated, so the two cannot drift apart.
func TestTheTeamStaffBackfillRestoresExistingTeams(t *testing.T) {
	resetDB(t)
	coach, coachID := signInCoach(t, "backfill@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "Legacy"})
	if team.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", team.status, team.raw)
	}
	teamID := team.body["id"].(string)

	// Pre-migration: the team exists and nothing records who runs it.
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM team_staff WHERE team_id = $1`, teamID); err != nil {
		t.Fatal(err)
	}
	// Asserted on the table rather than through GET /teams, because this coach owns a
	// personal org and so holds admin — they would see the team either way. It is the
	// coach in someone else's club, who holds only `coach`, that the backfill saves, and
	// what saves them is a row existing here at all.
	var staffCount int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM team_staff WHERE team_id = $1`, teamID).Scan(&staffCount); err != nil {
		t.Fatal(err)
	}
	if staffCount != 0 {
		t.Fatalf("precondition: the team should have no staff row, got %d", staffCount)
	}

	if _, err := testPool.Exec(context.Background(), teamStaffBackfillSQL(t)); err != nil {
		t.Fatalf("run the backfill: %v", err)
	}

	var owner string
	if err := testPool.QueryRow(context.Background(),
		`SELECT person_id::text FROM team_staff WHERE team_id = $1`, teamID).Scan(&owner); err != nil {
		t.Fatalf("the backfill left the team unstaffed: %v", err)
	}
	if owner != coachID {
		t.Errorf("backfilled to %s, want the coach the team belongs to (%s)", owner, coachID)
	}

	// And it is idempotent, which matters because a migration that is re-run against a
	// partly-migrated database must not fail on the rows already there.
	if _, err := testPool.Exec(context.Background(), teamStaffBackfillSQL(t)); err != nil {
		t.Errorf("the backfill must be safe to run twice: %v", err)
	}
}

// teamStaffBackfillSQL returns the INSERT from 0013, so the test cannot pass against a
// statement the migration no longer contains.
func teamStaffBackfillSQL(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("../database/migrations/0013_team_staff.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	const marker = "INSERT INTO team_staff (team_id, person_id)\nSELECT"
	i := strings.Index(string(source), marker)
	if i < 0 {
		t.Fatal("0013 no longer contains the backfill this test exists to cover")
	}
	return string(source)[i:]
}
