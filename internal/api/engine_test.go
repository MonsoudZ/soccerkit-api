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
