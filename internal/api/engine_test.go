package api_test

import (
	"net/http"
	"testing"
)

func templateID(t *testing.T, token, context string) string {
	t.Helper()
	list := do(t, http.MethodGet, "/api/v1/templates?context="+context, token, nil)
	arr := list.arr()
	if len(arr) == 0 {
		t.Fatalf("no template for context %s", context)
	}
	return arr[0].(map[string]any)["id"].(string)
}

func createAthlete(t *testing.T, token, name string) string {
	t.Helper()
	r := do(t, http.MethodPost, "/api/v1/persons", token, map[string]any{"displayName": name})
	if r.status != http.StatusCreated {
		t.Fatalf("create athlete: %d %s", r.status, r.raw)
	}
	return r.body["id"].(string)
}

func TestEvaluationEngineAggregates(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "coach@e.com")
	athlete := createAthlete(t, coach, "Sam Player")
	preGame := templateID(t, coach, "pre_game")

	submit := func(sleep, energy float64) {
		r := do(t, http.MethodPost, "/api/v1/form-instances", coach, map[string]any{
			"templateId":      preGame,
			"subjectPersonId": athlete,
			"answers": []map[string]any{
				{"key": "sleep", "numericValue": sleep},
				{"key": "energy", "numericValue": energy},
				{"key": "warmed_up", "boolValue": true},
			},
		})
		if r.status != http.StatusCreated {
			t.Fatalf("submit instance: %d %s", r.status, r.raw)
		}
	}
	submit(4, 5)
	submit(2, 3)

	// Two instances recorded.
	instances := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/instances", coach, nil).arr()
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(instances))
	}

	// Aggregate: sleep avg (4+2)/2 = 3, energy avg (5+3)/2 = 4.
	agg := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/aggregate?context=pre_game", coach, nil).arr()
	byKey := map[string]map[string]any{}
	for _, row := range agg {
		m := row.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if byKey["sleep"] == nil || byKey["sleep"]["average"].(float64) != 3 {
		t.Errorf("sleep average should be 3, got %v", byKey["sleep"])
	}
	if byKey["energy"] == nil || byKey["energy"]["average"].(float64) != 4 {
		t.Errorf("energy average should be 4, got %v", byKey["energy"])
	}
	if byKey["sleep"]["samples"].(float64) != 2 {
		t.Errorf("expected 2 samples for sleep, got %v", byKey["sleep"]["samples"])
	}
}

func TestSubmitRejectsUnknownFieldKey(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "coach2@e.com")
	athlete := createAthlete(t, coach, "Pat Player")
	preGame := templateID(t, coach, "pre_game")

	r := do(t, http.MethodPost, "/api/v1/form-instances", coach, map[string]any{
		"templateId":      preGame,
		"subjectPersonId": athlete,
		"answers":         []map[string]any{{"key": "not_a_field", "numericValue": 3}},
	})
	if r.status != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown field key, got %d %s", r.status, r.raw)
	}
}

func TestCreateCustomTemplate(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "coach3@e.com")

	r := do(t, http.MethodPost, "/api/v1/templates", coach, map[string]any{
		"context": "tryout",
		"name":    "Striker Tryout",
		"fields": []map[string]any{
			{"key": "finishing", "label": "Finishing", "kind": "scale"},
			{"key": "movement", "label": "Off-ball movement", "kind": "scale"},
		},
	})
	if r.status != http.StatusCreated {
		t.Fatalf("create template: %d %s", r.status, r.raw)
	}
	if r.body["context"] != "tryout" {
		t.Errorf("unexpected context: %v", r.body["context"])
	}
	fields := r.body["fields"].([]any)
	if len(fields) != 2 {
		t.Errorf("expected 2 fields, got %d", len(fields))
	}
}

// TestEvaluationIsScopedToTheOrg — templates and instances were readable, and
// instances writable, by any authenticated account that knew an id.
func TestEvaluationIsScopedToTheOrg(t *testing.T) {
	resetDB(t)
	coachA, _ := registerUser(t, "evalA@e.com")
	coachB, _ := registerUser(t, "evalB@e.com")
	athleteA := createAthlete(t, coachA, "A's Athlete")
	templateA := templateID(t, coachA, "pre_game")

	// Coach B cannot read coach A's template.
	if r := do(t, http.MethodGet, "/api/v1/templates/"+templateA, coachB, nil); r.status != http.StatusNotFound {
		t.Errorf("cross-org template read: got %d %s, want 404", r.status, r.raw)
	}

	// Nor submit against it, nor against A's athlete.
	if r := do(t, http.MethodPost, "/api/v1/form-instances", coachB, map[string]any{
		"templateId": templateA, "subjectPersonId": athleteA,
		"answers": []map[string]any{{"key": "sleep", "numericValue": 999999}},
	}); r.status != http.StatusNotFound {
		t.Errorf("cross-org submit: got %d %s, want 404", r.status, r.raw)
	}

	// A's aggregate is untouched by the attempt.
	if agg := do(t, http.MethodGet, "/api/v1/persons/"+athleteA+"/aggregate", coachA, nil).arr(); len(agg) != 0 {
		t.Errorf("aggregate should be empty, got %v", agg)
	}

	// And an instance A legitimately records is not readable by B.
	inst := do(t, http.MethodPost, "/api/v1/form-instances", coachA, map[string]any{
		"templateId": templateA, "subjectPersonId": athleteA,
		"answers": []map[string]any{{"key": "sleep", "numericValue": 4}},
	})
	if inst.status != http.StatusCreated {
		t.Fatalf("owner submit: %d %s", inst.status, inst.raw)
	}
	instID, _ := inst.body["id"].(string)
	if r := do(t, http.MethodGet, "/api/v1/form-instances/"+instID, coachB, nil); r.status != http.StatusNotFound {
		t.Errorf("cross-org instance read: got %d %s, want 404", r.status, r.raw)
	}
	if r := do(t, http.MethodGet, "/api/v1/form-instances/"+instID, coachA, nil); r.status != http.StatusOK {
		t.Errorf("owner instance read: got %d %s, want 200", r.status, r.raw)
	}
}

// TestAnswersAreValidatedAgainstTheirField — answers used to be written straight
// through without consulting the field they claimed to answer, so the aggregate the
// product is built on averaged unvalidated client input.
func TestAnswersAreValidatedAgainstTheirField(t *testing.T) {
	resetDB(t)
	coach, _ := registerUser(t, "answers@e.com")
	athlete := createAthlete(t, coach, "Val Idation")
	preGame := templateID(t, coach, "pre_game")

	submit := func(answers []map[string]any) resp {
		return do(t, http.MethodPost, "/api/v1/form-instances", coach, map[string]any{
			"templateId": preGame, "subjectPersonId": athlete, "answers": answers,
		})
	}

	cases := []struct {
		name    string
		answers []map[string]any
	}{
		// "warmed_up" is a bool field; a number here used to land in the score aggregate.
		{"number into a bool field", []map[string]any{{"key": "warmed_up", "numericValue": 77}}},
		{"text into a scale field", []map[string]any{{"key": "sleep", "textValue": "great"}}},
		{"bool into a scale field", []map[string]any{{"key": "sleep", "boolValue": true}}},
		{"scale below its configured min", []map[string]any{{"key": "sleep", "numericValue": -4200.5}}},
		{"scale above its configured max", []map[string]any{{"key": "sleep", "numericValue": 999999}}},
		{"two values in one answer", []map[string]any{{"key": "sleep", "numericValue": 3, "textValue": "x"}}},
		{"no value at all", []map[string]any{{"key": "sleep"}}},
		// The upsert on (instance_id, field_id) used to let the second silently
		// overwrite the first, usually with NULL, while the response echoed both.
		{"duplicate key", []map[string]any{
			{"key": "sleep", "numericValue": 4},
			{"key": "sleep", "textValue": "second"},
		}},
	}
	for _, tc := range cases {
		if r := submit(tc.answers); r.status != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", tc.name, r.status, r.raw)
		}
	}

	// Nothing above was recorded.
	if agg := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/aggregate", coach, nil).arr(); len(agg) != 0 {
		t.Errorf("no rejected answer should reach the aggregate, got %v", agg)
	}

	// A well-formed submission still works, at both ends of the declared range.
	if r := submit([]map[string]any{
		{"key": "sleep", "numericValue": 1},
		{"key": "energy", "numericValue": 5},
		{"key": "warmed_up", "boolValue": true},
	}); r.status != http.StatusCreated {
		t.Fatalf("valid submission: %d %s", r.status, r.raw)
	}
	if agg := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/aggregate", coach, nil).arr(); len(agg) != 2 {
		t.Errorf("expected sleep and energy in the aggregate, got %v", agg)
	}
}
