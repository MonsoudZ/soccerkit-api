package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// payloadOf finds one record in a pull and returns its decoded payload.
func payloadOf(t *testing.T, pull syncPull, recType, id string) map[string]any {
	t.Helper()
	for _, rec := range pull.Records {
		if rec.Type == recType && rec.ID == id {
			var out map[string]any
			if err := json.Unmarshal(rec.Payload, &out); err != nil {
				t.Fatalf("decode %s payload: %v (%s)", recType, err, rec.Payload)
			}
			return out
		}
	}
	t.Fatalf("%s %s was not in the pull (%d records)", recType, id, len(pull.Records))
	return nil
}

// TestTeamEditReachesTheApp is the point of the whole feature: an edit made over REST
// has to show up on the phone.
//
// A pull returns `payload`, not the projected columns, so a PATCH that wrote only the
// columns would leave the app showing the old name -- and the app's next push, built
// from its own unchanged copy, would put the old name back in the database. The test
// asserts the payload moved, that untouched keys survived, and that a field this server
// does not project survived too.
func TestTeamEditReachesTheApp(t *testing.T) {
	resetDB(t)
	token := appleToken(t, "sub-teamedit", "teamedit@e.com")
	teamID := uuid.NewString()

	push := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{{
			"type": "Team", "id": teamID,
			"payload": map[string]any{
				"name": "U11", "ageGroup": "U11", "season": "2026",
				// A key this server does not project. The app writes fields the backend
				// has never heard of, and an edit must not drop them.
				"colors": map[string]any{"home": "red"},
			},
		}},
	})
	if push.status != http.StatusOK {
		t.Fatalf("push: %d %s", push.status, push.raw)
	}

	patch := do(t, http.MethodPatch, "/api/v1/teams/"+teamID, token, map[string]any{"name": "U12"})
	if patch.status != http.StatusOK {
		t.Fatalf("patch team: %d %s", patch.status, patch.raw)
	}
	if patch.body["name"] != "U12" {
		t.Errorf("response should carry the new name, got %s", patch.raw)
	}

	payload := payloadOf(t, pullSync(t, token, ""), "Team", teamID)
	if payload["name"] != "U12" {
		t.Errorf("the edit did not reach the sync payload: name is %v, want U12 "+
			"(a column-only write is invisible to the app)", payload["name"])
	}
	if payload["ageGroup"] != "U11" || payload["season"] != "2026" {
		t.Errorf("untouched fields were lost from the payload: %v", payload)
	}
	if colors, ok := payload["colors"].(map[string]any); !ok || colors["home"] != "red" {
		t.Errorf("an unprojected field was dropped by the edit: %v", payload["colors"])
	}
}

// TestTeamEditClearsAnOptionalField pins how a cleared field is spelled in the payload.
// The projected upserts run payload strings through nilIfEmpty, so the app writes "" for
// an empty field; writing JSON null back would hand a non-optional Swift String a value
// it cannot decode. See syncString.
func TestTeamEditClearsAnOptionalField(t *testing.T) {
	resetDB(t)
	token := appleToken(t, "sub-teamclear", "teamclear@e.com")
	teamID := uuid.NewString()

	do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{{
			"type": "Team", "id": teamID,
			"payload": map[string]any{"name": "U11", "ageGroup": "U11", "season": "2026"},
		}},
	})
	patch := do(t, http.MethodPatch, "/api/v1/teams/"+teamID, token, map[string]any{"season": nil})
	if patch.status != http.StatusOK {
		t.Fatalf("clear season: %d %s", patch.status, patch.raw)
	}
	if patch.body["season"] != nil {
		t.Errorf("season should read back null over REST, got %v", patch.body["season"])
	}
	payload := payloadOf(t, pullSync(t, token, ""), "Team", teamID)
	if payload["season"] != "" {
		t.Errorf("a cleared field should be \"\" in the payload, not %#v", payload["season"])
	}
}

// TestPersonEditReachesTheApp covers the same propagation for a person, and the field
// split that makes persons different: only the four columns SyncUpsertPerson projects
// belong in the payload. givenName and birthdate are REST's alone, and inventing keys
// for them would put fields in the app's record that the app never wrote.
func TestPersonEditReachesTheApp(t *testing.T) {
	resetDB(t)
	token := appleToken(t, "sub-personedit", "personedit@e.com")
	personID := uuid.NewString()

	push := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{{
			"type": "Person", "id": personID,
			"payload": map[string]any{
				"name": "Alex Kim", "medicalNotes": "none", "shirtSize": "YM",
			},
		}},
	})
	if push.status != http.StatusOK {
		t.Fatalf("push: %d %s", push.status, push.raw)
	}

	patch := do(t, http.MethodPatch, "/api/v1/persons/"+personID, token, map[string]any{
		"displayName":  "Alex Kim-Rivera",
		"medicalNotes": "peanut allergy",
		"givenName":    "Alex",
		"birthdate":    "2013-04-02",
	})
	if patch.status != http.StatusOK {
		t.Fatalf("patch person: %d %s", patch.status, patch.raw)
	}
	if patch.body["givenName"] != "Alex" || patch.body["birthdate"] != "2013-04-02" {
		t.Errorf("REST-only fields should read back over REST: %s", patch.raw)
	}

	payload := payloadOf(t, pullSync(t, token, ""), "Person", personID)
	if payload["name"] != "Alex Kim-Rivera" {
		t.Errorf("displayName did not reach the payload as \"name\": %v", payload["name"])
	}
	if payload["medicalNotes"] != "peanut allergy" {
		t.Errorf("medicalNotes did not reach the payload: %v", payload["medicalNotes"])
	}
	if payload["shirtSize"] != "YM" {
		t.Errorf("an unprojected field was dropped by the edit: %v", payload["shirtSize"])
	}
	for _, key := range []string{"givenName", "birthdate", "familyName", "email", "phone"} {
		if _, present := payload[key]; present {
			t.Errorf("%q is REST-only and must not be written into the app's record: %v",
				key, payload)
		}
	}
}

// TestPersonEditRejectsEditingAnotherAccountHolder pins the authorization line: a coach
// manages loginless athletes, and anyone with an account edits their own row.
func TestPersonEditRejectsEditingAnotherAccountHolder(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "clubcoach@e.com")
	_, otherID := signInCoach(t, "othercoach@e.com")

	// An athlete the coach created over REST: no login, so theirs to edit.
	athlete := do(t, http.MethodPost, "/api/v1/persons", coach, map[string]any{
		"displayName": "Sam Ortiz", "role": "player",
	})
	if athlete.status != http.StatusCreated {
		t.Fatalf("create athlete: %d %s", athlete.status, athlete.raw)
	}
	ok := do(t, http.MethodPatch, "/api/v1/persons/"+athlete.body["id"].(string), coach,
		map[string]any{"phone": "555-0100"})
	if ok.status != http.StatusOK {
		t.Fatalf("a coach may edit a loginless athlete: %d %s", ok.status, ok.raw)
	}

	// Another coach's own Person is not theirs to rewrite. They are in a different org
	// here, so the visibility gate answers first -- either way it must not be 200.
	bad := do(t, http.MethodPatch, "/api/v1/persons/"+otherID, coach,
		map[string]any{"medicalNotes": "invented"})
	if bad.status == http.StatusOK {
		t.Fatalf("editing another account holder must be refused, got 200 %s", bad.raw)
	}
}

// TestPatchRejectsUnknownAndMistypedFields covers the shared decodePatch contract across
// the endpoints that use it. A PATCH that ignored a misspelled field would answer 200
// for a change it never made.
func TestPatchRejectsUnknownAndMistypedFields(t *testing.T) {
	resetDB(t)
	coach, selfID := signInCoach(t, "patchval@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U13"})
	teamID := team.body["id"].(string)

	cases := []struct {
		name, path string
		body       map[string]any
	}{
		{"unknown team field", "/api/v1/teams/" + teamID, map[string]any{"nmae": "typo"}},
		{"team name wrong type", "/api/v1/teams/" + teamID, map[string]any{"name": true}},
		{"team name empty", "/api/v1/teams/" + teamID, map[string]any{"name": ""}},
		{"team name null", "/api/v1/teams/" + teamID, map[string]any{"name": nil}},
		{"unknown person field", "/api/v1/persons/" + selfID, map[string]any{"medicalNote": "typo"}},
		{"person birthdate bad", "/api/v1/persons/" + selfID, map[string]any{"birthdate": "02-04-2013"}},
		{"person phone wrong type", "/api/v1/persons/" + selfID, map[string]any{"phone": 5550100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, http.MethodPatch, tc.path, coach, tc.body)
			if r.status != http.StatusBadRequest {
				t.Errorf("expected 400, got %d %s", r.status, r.raw)
			}
		})
	}
}

// TestOrganizationRename covers the club endpoint, which has no sync spine at all and so
// needs no propagation.
func TestOrganizationRename(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "orgrename@e.com")

	me := do(t, http.MethodGet, "/api/v1/me", coach, nil)
	memberships := me.body["memberships"].([]any)
	orgID := memberships[0].(map[string]any)["organizationId"].(string)

	r := do(t, http.MethodPatch, "/api/v1/organizations/"+orgID, coach,
		map[string]any{"name": "Riverside FC"})
	if r.status != http.StatusOK {
		t.Fatalf("rename org: %d %s", r.status, r.raw)
	}
	if r.body["name"] != "Riverside FC" {
		t.Errorf("expected the new name, got %s", r.raw)
	}

	// kind is deliberately not editable -- it changes what account deletion destroys.
	k := do(t, http.MethodPatch, "/api/v1/organizations/"+orgID, coach,
		map[string]any{"kind": "club"})
	if k.status != http.StatusBadRequest {
		t.Errorf("kind must not be editable, got %d %s", k.status, k.raw)
	}

	// Another coach's org is not visible to this one.
	stranger, _ := signInCoach(t, "stranger@e.com")
	s := do(t, http.MethodPatch, "/api/v1/organizations/"+orgID, stranger,
		map[string]any{"name": "Hijacked"})
	if s.status == http.StatusOK {
		t.Fatalf("renaming someone else's org must be refused, got 200 %s", s.raw)
	}
}

// TestTeamCreatedOverRESTReachesTheApp is the create-side counterpart to
// TestTeamEditReachesTheApp. A REST insert used to leave sync_account_id NULL, and
// ListSyncChangesSince scopes every branch to an account, so a team made this way never
// reached a phone at all.
//
// The payload assertions are the load-bearing part. Swift's Codable throws on a missing
// required key and loses the whole record, so a team that syncs but cannot be decoded is
// no better than one that never arrives -- and worse, because the failure surfaces on the
// device as "Unexpected server response". The keys below are what Models/Team.swift
// decodes with `decode` rather than `decodeIfPresent`.
func TestTeamCreatedOverRESTReachesTheApp(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "restcreate@e.com")

	created := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{
		"name": "U11 Rovers", "ageGroup": "U11", "season": "2026",
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", created.status, created.raw)
	}
	teamID := created.body["id"].(string)

	payload := payloadOf(t, pullSync(t, coach, ""), "Team", teamID)

	// Every key Team's decoder requires, or the app loses the record.
	for _, key := range []string{"id", "name", "ageGroup", "season", "accentName"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("Team requires %q; the payload has %v", key, payload)
		}
	}
	if payload["id"] != teamID {
		t.Errorf("the payload id must match the record id: %v vs %s", payload["id"], teamID)
	}
	if payload["name"] != "U11 Rovers" || payload["ageGroup"] != "U11" || payload["season"] != "2026" {
		t.Errorf("payload does not carry what was created: %v", payload)
	}
	if payload["accentName"] == "" || payload["accentName"] == nil {
		t.Errorf("accentName has no default in the app's decoder, so it must be set: %v", payload)
	}
}

// TestTeamCreatedWithoutOptionalsIsStillDecodable covers the nullable columns. ageGroup
// and season are optional over REST and required by the app, so they are written as ""
// rather than omitted -- a missing key throws on the far side, and null fails to decode
// into a non-optional String.
func TestTeamCreatedWithoutOptionalsIsStillDecodable(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "restcreatebare@e.com")

	created := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "Bare"})
	if created.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", created.status, created.raw)
	}
	payload := payloadOf(t, pullSync(t, coach, ""), "Team", created.body["id"].(string))

	for _, key := range []string{"ageGroup", "season"} {
		v, ok := payload[key]
		if !ok {
			t.Errorf("%q must be present even when unset, or the app throws: %v", key, payload)
			continue
		}
		if _, isString := v.(string); !isString {
			t.Errorf("%q must be a string, not %#v — null fails a non-optional String", key, v)
		}
	}
}
