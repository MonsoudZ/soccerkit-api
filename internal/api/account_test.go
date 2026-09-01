package api_test

import (
	"context"
	"net/http"
	"testing"
)

// countRows runs a scalar COUNT(*) against the test pool.
func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestDeleteMeCascadesAndErasesAthletePII is the load-bearing test: after a coach
// deletes their account, nothing they owned survives — and, critically, the
// athlete Person rows (minors' PII) are gone, not just their memberships.
func TestDeleteMeCascadesAndErasesAthletePII(t *testing.T) {
	resetDB(t)
	coach, coachPerson := registerUser(t, "delete-me@e.com")
	athlete := createAthlete(t, coach, "PII Kid")

	// Give the athlete a team, a roster spot, and a submitted evaluation — the
	// full set of rows that reference them.
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U10"})
	if team.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", team.status, team.raw)
	}
	teamID := team.body["id"].(string)
	if add := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coach, map[string]any{"personId": athlete}); add.status != http.StatusCreated {
		t.Fatalf("add roster: %d %s", add.status, add.raw)
	}
	preGame := templateID(t, coach, "pre_game")
	inst := do(t, http.MethodPost, "/api/v1/form-instances", coach, map[string]any{
		"templateId": preGame, "subjectPersonId": athlete,
		// 4, not 8: "sleep" is a scale field declared min 1 / max 5, and answers are
		// now range-checked against their field's config.
		"answers": []map[string]any{{"key": "sleep", "numericValue": 4}},
	})
	if inst.status != http.StatusCreated {
		t.Fatalf("submit instance: %d %s", inst.status, inst.raw)
	}

	// A second, unrelated coach whose data must be untouched by the deletion.
	other, otherPerson := registerUser(t, "survivor@e.com")
	otherAthlete := createAthlete(t, other, "Safe Kid")

	// Delete.
	del := do(t, http.MethodDelete, "/api/v1/me", coach, nil)
	if del.status != http.StatusNoContent {
		t.Fatalf("delete me: expected 204, got %d %s", del.status, del.raw)
	}

	// The caller and everything they owned is gone.
	if n := countRows(t, `SELECT count(*) FROM persons WHERE id = $1`, coachPerson); n != 0 {
		t.Errorf("coach Person should be deleted, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM user_accounts WHERE email = $1`, "delete-me@e.com"); n != 0 {
		t.Errorf("coach user_account should be deleted, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM refresh_tokens rt JOIN user_accounts ua ON ua.id = rt.user_account_id WHERE ua.person_id = $1`, coachPerson); n != 0 {
		t.Errorf("refresh tokens should be deleted, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM organizations`); n != 1 {
		t.Errorf("only the survivor's org should remain, found %d orgs", n)
	}
	if n := countRows(t, `SELECT count(*) FROM teams WHERE id = $1`, teamID); n != 0 {
		t.Errorf("team should be deleted, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM roster_memberships`); n != 0 {
		t.Errorf("roster memberships should be deleted, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM form_instances`); n != 0 {
		t.Errorf("form instances should be deleted, found %d", n)
	}

	// THE regression guard: the athlete's Person row (their PII) is gone.
	if n := countRows(t, `SELECT count(*) FROM persons WHERE id = $1`, athlete); n != 0 {
		t.Errorf("athlete Person (PII) should be erased, found %d", n)
	}

	// The unrelated coach and their athlete are untouched.
	if n := countRows(t, `SELECT count(*) FROM persons WHERE id = $1`, otherPerson); n != 1 {
		t.Errorf("survivor coach Person should remain, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM persons WHERE id = $1`, otherAthlete); n != 1 {
		t.Errorf("survivor athlete Person should remain, found %d", n)
	}
}

// TestDeleteMeIsIdempotent verifies a retry (or a delete of an already-gone
// account) still returns 204 rather than 500 — the flaky-network case.
func TestDeleteMeIsIdempotent(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "idem@e.com")

	if first := do(t, http.MethodDelete, "/api/v1/me", coach, nil); first.status != http.StatusNoContent {
		t.Fatalf("first delete: expected 204, got %d %s", first.status, first.raw)
	}
	// Same (now-stale) access token; the JWT outlives the row it points at.
	if second := do(t, http.MethodDelete, "/api/v1/me", coach, nil); second.status != http.StatusNoContent {
		t.Fatalf("second delete: expected 204, got %d %s", second.status, second.raw)
	}
}

// TestDeleteMeRequiresAuth confirms identity comes from the bearer token — a
// caller can only ever delete their own account, and an anonymous call is 401.
func TestDeleteMeRequiresAuth(t *testing.T) {
	resetDB(t)
	if r := do(t, http.MethodDelete, "/api/v1/me", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("unauthenticated delete should be 401, got %d %s", r.status, r.raw)
	}
}

// TestDeleteMeSparesSharedAthlete guards the multi-org case: an athlete still
// rostered under another coach must survive the first coach's deletion, even
// though that can't happen in today's solo-coach model.
func TestDeleteMeSparesSharedAthlete(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "shareA@e.com")
	coachB, _ := registerUser(t, "shareB@e.com")

	// Athlete belongs to coach A's org (membership) but is also rostered on a
	// team in coach B's org.
	//
	// The roster row is written directly rather than through POST /teams/{id}/roster:
	// that endpoint now refuses a Person outside the caller's organization, because
	// accepting one let any coach attach an arbitrary Person id to their own team and
	// read the athlete's PII back out. Sharing an athlete across clubs is a real future
	// case that needs its own consented flow; until then the state is only reachable
	// here, and what this test is actually about is the deletion cascade's behaviour
	// once it exists.
	shared := createAthlete(t, coachA, "Shared Kid")
	teamB := do(t, http.MethodPost, "/api/v1/teams", coachB, map[string]any{"name": "B Team"})
	teamBID := teamB.body["id"].(string)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO roster_memberships (person_id, team_id) VALUES ($1, $2)`,
		shared, teamBID); err != nil {
		t.Fatalf("link shared athlete to coach B's team: %v", err)
	}

	// Coach A evaluates the athlete before leaving. This is the case the test used to
	// miss: the spared athlete keeps their form_instances, those point at coach A's
	// template, and deleting coach A's org cascades that template away. With
	// form_instances.template_id at its original NO ACTION, Postgres refused the whole
	// transaction and the delete came back 500 with nothing erased.
	inst := do(t, http.MethodPost, "/api/v1/form-instances", coachA, map[string]any{
		"templateId": templateID(t, coachA, "pre_game"), "subjectPersonId": shared,
		"answers": []map[string]any{{"key": "sleep", "numericValue": 4}},
	})
	if inst.status != http.StatusCreated {
		t.Fatalf("submit instance: %d %s", inst.status, inst.raw)
	}

	if del := do(t, http.MethodDelete, "/api/v1/me", coachA, nil); del.status != http.StatusNoContent {
		t.Fatalf("coach A delete: expected 204, got %d %s", del.status, del.raw)
	}

	// The shared athlete survives because they're still linked to coach B's org.
	if n := countRows(t, `SELECT count(*) FROM persons WHERE id = $1`, shared); n != 1 {
		t.Errorf("shared athlete rostered under coach B must survive, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM roster_memberships WHERE person_id = $1`, shared); n != 1 {
		t.Errorf("shared athlete's roster spot under coach B must survive, found %d", n)
	}
}

// TestDeleteMeAfterSelfEvaluation covers the same failure from the direction a single
// account reaches on its own, with no second party and nothing written directly to the
// database: submit an evaluation about yourself, then delete your account.
//
// handleDeleteMe removes the caller's organizations before the caller's own Person, so
// an instance whose subject is the caller is still present when the org's templates
// cascade away — and form_instances.template_id used to refuse that. A player filing
// their own pre-game check-in is the product's core loop, so this was the ordinary path,
// and because the handler is one transaction the account could never be deleted at all.
func TestDeleteMeAfterSelfEvaluation(t *testing.T) {
	resetDB(t)
	coach, coachPerson := registerUser(t, "self-eval@e.com")

	inst := do(t, http.MethodPost, "/api/v1/form-instances", coach, map[string]any{
		"templateId": templateID(t, coach, "pre_game"), "subjectPersonId": coachPerson,
		"answers": []map[string]any{{"key": "sleep", "numericValue": 4}},
	})
	if inst.status != http.StatusCreated {
		t.Fatalf("submit self evaluation: %d %s", inst.status, inst.raw)
	}

	if del := do(t, http.MethodDelete, "/api/v1/me", coach, nil); del.status != http.StatusNoContent {
		t.Fatalf("delete me: expected 204, got %d %s", del.status, del.raw)
	}

	// The erasure actually happened — a 204 with a rolled-back transaction would be
	// worse than the 500 it replaced.
	if n := countRows(t, `SELECT count(*) FROM persons WHERE id = $1`, coachPerson); n != 0 {
		t.Errorf("caller Person should be deleted, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM form_instances`); n != 0 {
		t.Errorf("the evaluation should be gone with its template, found %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM form_templates`); n != 0 {
		t.Errorf("the org's templates should be gone, found %d", n)
	}
}
