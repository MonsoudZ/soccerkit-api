package api_test

import (
	"context"
	"net/http"
	"testing"
)

// TestPersonSyncReconcilesCoachIdentity proves the coach's synced Person lands in
// the same persons row the account was provisioned with (one identity), because
// both sides use the id /auth/apple returned.
func TestPersonSyncReconcilesCoachIdentity(t *testing.T) {
	resetDB(t)
	ctx := context.Background()

	r := appleSignIn(t, "apple-sub-coach", "coach@example.com", "Coach One")
	if r.status != http.StatusOK {
		t.Fatalf("apple auth: %d %s", r.status, r.raw)
	}
	token, _ := r.body["token"].(string)
	personID, _ := r.body["personID"].(string)
	if personID == "" {
		t.Fatalf("no personID: %s", r.raw)
	}

	// The account is linked to that Person (provisioned in the persons table).
	var linked int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM user_accounts WHERE person_id = $1`, personID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("account not linked to personID %s", personID)
	}

	// Push the coach's Person with that SAME id — it must UPDATE the existing row,
	// not create a second identity.
	push := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{
			{"type": "Person", "id": personID, "payload": map[string]any{
				"id": personID, "name": "Coach One Updated", "medicalNotes": "NKA",
			}},
		},
	})
	if push.status != http.StatusOK {
		t.Fatalf("push: %d %s", push.status, push.raw)
	}

	// Exactly one persons row, updated, and now scoped to the account.
	var count int
	var name string
	var syncAcct *string
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) OVER (), display_name, sync_account_id::text
		 FROM persons WHERE id = $1`, personID).Scan(&count, &name, &syncAcct); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 persons row for the coach, got %d", count)
	}
	if name != "Coach One Updated" {
		t.Fatalf("projected display_name = %q, want Coach One Updated", name)
	}
	if syncAcct == nil || *syncAcct != personID {
		t.Fatalf("persons.sync_account_id = %v, want %s", syncAcct, personID)
	}

	// And it round-trips on pull.
	pull := pullSync(t, token, "")
	if got := payloadField(t, pull, "Person", "name"); got != "Coach One Updated" {
		t.Fatalf("pulled Person name = %q, want Coach One Updated", got)
	}
}

// TestCoachPersonIDIsDeterministic proves the same Apple subject always maps to
// the same Person id — so every device and the server agree with no migration.
func TestCoachPersonIDIsDeterministic(t *testing.T) {
	resetDB(t)
	a := appleSignIn(t, "sub-deterministic", "a@example.com", nil)
	b := appleSignIn(t, "sub-deterministic", "a@example.com", nil)
	if a.body["personID"] != b.body["personID"] {
		t.Fatalf("personID not stable: %v vs %v", a.body["personID"], b.body["personID"])
	}
	other := appleSignIn(t, "sub-different", "c@example.com", nil)
	if other.body["personID"] == a.body["personID"] {
		t.Fatal("different subjects should map to different Person ids")
	}
}
