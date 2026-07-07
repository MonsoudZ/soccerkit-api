package api_test

import (
	"net/http"
	"testing"
)

const futureKickoff = "2027-01-01T18:00:00Z"

func hostMatch(t *testing.T, token string, maxPlayers int) string {
	t.Helper()
	r := do(t, http.MethodPost, "/api/v1/matches", token, map[string]any{
		"title": "Game", "format": "5v5", "maxPlayers": maxPlayers, "kickoffAt": futureKickoff,
	})
	if r.status != http.StatusCreated {
		t.Fatalf("create match failed: %d %s", r.status, r.raw)
	}
	return r.body["id"].(string)
}

func TestMatchHostAutoEnrolled(t *testing.T) {
	resetDB(t)
	host, hostID := registerUser(t, "h@e.com")
	r := do(t, http.MethodPost, "/api/v1/matches", host, map[string]any{
		"title": "Game", "format": "5v5", "maxPlayers": 10, "kickoffAt": futureKickoff,
	})
	if r.body["goingCount"].(float64) != 1 {
		t.Errorf("host should be auto-enrolled, goingCount=%v", r.body["goingCount"])
	}
	if r.body["hostId"] != hostID {
		t.Errorf("hostId mismatch")
	}
}

func TestMatchHostOnlyMutations(t *testing.T) {
	resetDB(t)
	host, _ := registerUser(t, "h2@e.com")
	other, _ := registerUser(t, "o2@e.com")
	id := hostMatch(t, host, 10)

	if r := do(t, http.MethodPatch, "/api/v1/matches/"+id, other, map[string]any{"title": "Hijack"}); r.status != http.StatusForbidden {
		t.Errorf("non-host PATCH should 403, got %d", r.status)
	}
	if r := do(t, http.MethodPatch, "/api/v1/matches/"+id, host, map[string]any{"title": "Renamed"}); r.status != http.StatusOK || r.body["title"] != "Renamed" {
		t.Errorf("host PATCH failed: %d %s", r.status, r.raw)
	}
}

func TestWaitlistPromotion(t *testing.T) {
	resetDB(t)
	host, _ := registerUser(t, "wh@e.com")
	g1, _ := registerUser(t, "g1@e.com")
	g2, g2ID := registerUser(t, "g2@e.com")
	id := hostMatch(t, host, 2) // host + 1 = full

	rsvp := func(token string) resp {
		return do(t, http.MethodPut, "/api/v1/matches/"+id+"/rsvp", token, map[string]any{"status": "GOING"})
	}
	if r := rsvp(g1); r.body["status"] != "GOING" {
		t.Fatalf("g1 should be GOING, got %v", r.body["status"])
	}
	if r := rsvp(g2); r.body["status"] != "WAITLIST" {
		t.Fatalf("g2 should be WAITLIST when full, got %v", r.body["status"])
	}

	// Host leaves -> g2 promoted.
	if r := do(t, http.MethodDelete, "/api/v1/matches/"+id+"/rsvp", host, nil); r.status != http.StatusOK {
		t.Fatalf("host leave failed: %d", r.status)
	}

	detail := do(t, http.MethodGet, "/api/v1/matches/"+id, "", nil)
	rsvps := detail.body["rsvps"].([]any)
	var g2status string
	for _, item := range rsvps {
		m := item.(map[string]any)
		if m["user"].(map[string]any)["id"] == g2ID {
			g2status = m["status"].(string)
		}
	}
	if g2status != "GOING" {
		t.Errorf("g2 should be promoted to GOING, got %q", g2status)
	}
	if detail.body["goingCount"].(float64) != 2 {
		t.Errorf("expected goingCount 2, got %v", detail.body["goingCount"])
	}
}

func TestRsvpToCancelledMatchRejected(t *testing.T) {
	resetDB(t)
	host, _ := registerUser(t, "ch@e.com")
	guest, _ := registerUser(t, "cg@e.com")
	id := hostMatch(t, host, 10)

	do(t, http.MethodDelete, "/api/v1/matches/"+id, host, nil) // cancel

	r := do(t, http.MethodPut, "/api/v1/matches/"+id+"/rsvp", guest, map[string]any{"status": "GOING"})
	if r.status != http.StatusBadRequest {
		t.Errorf("RSVP to cancelled match should 400, got %d", r.status)
	}
}
