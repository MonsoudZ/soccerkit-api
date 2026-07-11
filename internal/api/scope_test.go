package api_test

import (
	"net/http"
	"testing"
)

// The by-id reads on persons, templates and form instances had no org check at
// all: the handlers went from the path UUID straight to the query. Any
// authenticated caller who could name an id could read any athlete's medical
// notes and any submitted evaluation. The suite had cross-org tests for teams
// and sessions — the two resources that already had the check — and none for
// these, so it was shaped like the implementation and could not catch its gaps.
//
// A caller who may not see a resource gets 404, not 403: a coach's Person id is
// a derived UUIDv5 of their Apple sub, so it is guessable, and 403 would confirm
// it exists.

// TestPersonReadsIsolatedByOrg covers the PII/PHI leak: displayName, birthdate,
// email, emergency contacts and medicalNotes all ride on personDTO.
func TestPersonReadsIsolatedByOrg(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "person-orgA@e.com")
	coachB, _ := registerUser(t, "person-orgB@e.com")
	athlete := createAthlete(t, coachA, "Private Kid")

	for _, path := range []string{
		"/api/v1/persons/" + athlete,
		"/api/v1/persons/" + athlete + "/instances",
		"/api/v1/persons/" + athlete + "/aggregate",
	} {
		if r := do(t, http.MethodGet, path, coachB, nil); r.status != http.StatusNotFound {
			t.Errorf("cross-org GET %s = %d, want 404; body %s", path, r.status, r.raw)
		}
		// The owning coach must still be able to read it.
		if r := do(t, http.MethodGet, path, coachA, nil); r.status != http.StatusOK {
			t.Errorf("owner GET %s = %d, want 200; body %s", path, r.status, r.raw)
		}
	}
}

// TestRosterRejectsForeignPerson covers the exfil path that works even without a
// direct person read: roster a foreign athlete onto your own team, then GET the
// team, which returns their displayName, email and birthdate.
func TestRosterRejectsForeignPerson(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "roster-orgA@e.com")
	coachB, _ := registerUser(t, "roster-orgB@e.com")
	athlete := createAthlete(t, coachA, "Poached Kid")

	team := do(t, http.MethodPost, "/api/v1/teams", coachB, map[string]any{"name": "B Team"})
	teamID := team.body["id"].(string)

	r := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coachB, map[string]any{
		"personId": athlete,
	})
	if r.status != http.StatusBadRequest {
		t.Fatalf("rostering a foreign person = %d, want 400; body %s", r.status, r.raw)
	}

	// Coach B's own athlete still rosters fine.
	own := createAthlete(t, coachB, "Own Kid")
	if ok := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coachB, map[string]any{
		"personId": own,
	}); ok.status != http.StatusCreated {
		t.Fatalf("rostering own person = %d, want 201; body %s", ok.status, ok.raw)
	}
}

// TestTemplateReadIsolatedByOrg: handleListTemplates already scoped by org, and
// the by-id read simply forgot to.
func TestTemplateReadIsolatedByOrg(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "tpl-orgA@e.com")
	coachB, _ := registerUser(t, "tpl-orgB@e.com")
	tpl := templateID(t, coachA, "pre_game")

	if r := do(t, http.MethodGet, "/api/v1/templates/"+tpl, coachB, nil); r.status != http.StatusNotFound {
		t.Errorf("cross-org template read = %d, want 404; body %s", r.status, r.raw)
	}
	if r := do(t, http.MethodGet, "/api/v1/templates/"+tpl, coachA, nil); r.status != http.StatusOK {
		t.Errorf("owner template read = %d, want 200; body %s", r.status, r.raw)
	}
}

// TestInstanceReadIsolatedByOrg: a submitted evaluation carries the athlete's
// mood, soreness and pain answers. An instance has no organization_id, so it
// inherits scope from its subject.
func TestInstanceReadIsolatedByOrg(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "inst-orgA@e.com")
	coachB, _ := registerUser(t, "inst-orgB@e.com")
	athlete := createAthlete(t, coachA, "Evaluated Kid")
	tpl := templateID(t, coachA, "pre_game")

	sub := do(t, http.MethodPost, "/api/v1/form-instances", coachA, map[string]any{
		"templateId":      tpl,
		"subjectPersonId": athlete,
		"answers":         []map[string]any{{"key": "sleep", "numericValue": 4}},
	})
	if sub.status != http.StatusCreated {
		t.Fatalf("submit: %d %s", sub.status, sub.raw)
	}
	instanceID := sub.body["id"].(string)

	if r := do(t, http.MethodGet, "/api/v1/form-instances/"+instanceID, coachB, nil); r.status != http.StatusNotFound {
		t.Errorf("cross-org instance read = %d, want 404; body %s", r.status, r.raw)
	}
	if r := do(t, http.MethodGet, "/api/v1/form-instances/"+instanceID, coachA, nil); r.status != http.StatusOK {
		t.Errorf("owner instance read = %d, want 200; body %s", r.status, r.raw)
	}
}

// TestSubmitInstanceRejectsForeignSubject covers the cross-org *write*: the
// handler resolved the org for its role check and then never used it again, so a
// coach could file an evaluation against another org's athlete, using another
// org's template.
func TestSubmitInstanceRejectsForeignSubject(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "write-orgA@e.com")
	coachB, _ := registerUser(t, "write-orgB@e.com")
	athleteA := createAthlete(t, coachA, "A's Kid")
	tplA := templateID(t, coachA, "pre_game")
	tplB := templateID(t, coachB, "pre_game")

	// B writes against A's athlete, using B's own template.
	foreignSubject := do(t, http.MethodPost, "/api/v1/form-instances", coachB, map[string]any{
		"templateId":      tplB,
		"subjectPersonId": athleteA,
		"answers":         []map[string]any{{"key": "sleep", "numericValue": 1}},
	})
	if foreignSubject.status != http.StatusBadRequest {
		t.Errorf("writing against a foreign subject = %d, want 400; body %s",
			foreignSubject.status, foreignSubject.raw)
	}

	// B writes against B's own athlete, but using A's template.
	athleteB := createAthlete(t, coachB, "B's Kid")
	foreignTemplate := do(t, http.MethodPost, "/api/v1/form-instances", coachB, map[string]any{
		"templateId":      tplA,
		"subjectPersonId": athleteB,
		"answers":         []map[string]any{{"key": "sleep", "numericValue": 1}},
	})
	if foreignTemplate.status != http.StatusBadRequest {
		t.Errorf("writing with a foreign template = %d, want 400; body %s",
			foreignTemplate.status, foreignTemplate.raw)
	}
}
