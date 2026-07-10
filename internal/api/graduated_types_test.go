package api_test

import (
	"context"
	"net/http"
	"testing"
)

// TestGraduatedTypesProjectIntoTables verifies Player/Event/Diagram now land in
// their own tables (queryable), not the opaque sync_documents fallback, and
// still round-trip on pull.
func TestGraduatedTypesProjectIntoTables(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	token := appleToken(t, "grad-sub", "grad@example.com")

	playerID := "11111111-0000-4000-8000-000000000001"
	eventID := "22222222-0000-4000-8000-000000000002"
	diagramID := "33333333-0000-4000-8000-000000000003"
	teamID := "44444444-0000-4000-8000-000000000004"

	push := do(t, http.MethodPost, "/api/v1/sync", token, map[string]any{
		"upserts": []map[string]any{
			{"type": "Player", "id": playerID, "payload": map[string]any{
				"id": playerID, "name": "Kid Striker", "number": 9, "position": "FWD",
			}},
			{"type": "Event", "id": eventID, "payload": map[string]any{
				"id": eventID, "teamID": teamID, "title": "City Cup", "kind": "tournament",
			}},
			{"type": "Diagram", "id": diagramID, "payload": map[string]any{
				"id": diagramID, "teamID": teamID, "title": "4-3-3 High Press",
			}},
		},
	})
	if push.status != http.StatusOK {
		t.Fatalf("push: %d %s", push.status, push.raw)
	}

	// Each landed in its real table with projected columns.
	var pName string
	var pNum int
	if err := testPool.QueryRow(ctx, `SELECT name, number FROM players WHERE id=$1`, playerID).Scan(&pName, &pNum); err != nil {
		t.Fatalf("player row: %v", err)
	}
	if pName != "Kid Striker" || pNum != 9 {
		t.Fatalf("player projected wrong: %q #%d", pName, pNum)
	}
	var eTitle, eKind string
	if err := testPool.QueryRow(ctx, `SELECT title, kind FROM events WHERE id=$1`, eventID).Scan(&eTitle, &eKind); err != nil {
		t.Fatalf("event row: %v", err)
	}
	if eTitle != "City Cup" || eKind != "tournament" {
		t.Fatalf("event projected wrong: %q/%q", eTitle, eKind)
	}
	var dTitle string
	if err := testPool.QueryRow(ctx, `SELECT title FROM diagrams WHERE id=$1`, diagramID).Scan(&dTitle); err != nil {
		t.Fatalf("diagram row: %v", err)
	}
	if dTitle != "4-3-3 High Press" {
		t.Fatalf("diagram projected wrong: %q", dTitle)
	}

	// None of them leaked into the generic fallback.
	var docs int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM sync_documents WHERE type IN ('Player','Event','Diagram')`).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != 0 {
		t.Fatalf("expected 0 graduated-type rows in sync_documents, got %d", docs)
	}

	// All three round-trip on pull.
	pull := pullSync(t, token, "")
	if len(pull.Records) != 3 {
		t.Fatalf("expected 3 records on pull, got %d: %s", len(pull.Records), mustJSON(pull))
	}
	for _, typ := range []string{"Player", "Event", "Diagram"} {
		if payloadField(t, pull, typ, "id") == "" {
			t.Fatalf("%s did not round-trip", typ)
		}
	}
}
