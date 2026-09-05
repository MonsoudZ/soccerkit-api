package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// soon is a time inside the sweep's 24-hour window, and far is outside it. Both are
// relative to now, because the sweep asks about the window rather than about a date.
func soon() string { return time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339) }
func far() string  { return time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339) }

func sweep(t *testing.T) int {
	t.Helper()
	n, err := testAPI.SweepReminders(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return n
}

// TestTheSweepChasesOnlyThoseWhoOweAnAnswer is the whole point of the reminder. The push
// a fixture sends when it is scheduled is one shot; a squad that swipes it away leaves
// the coach reading a sheet of silence the night before.
func TestTheSweepChasesOnlyThoseWhoOweAnAnswer(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "chase")

	game := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach, map[string]any{
		"opponent": "Rivals FC", "kickoffAt": soon(),
	})
	if game.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", game.status, game.raw)
	}
	gameID := game.body["id"].(string)

	// The player replies; the other athlete (whose parent holds the device) does not.
	if r := doIn(t, http.MethodPut, "/api/v1/games/"+gameID+"/rsvp", s.player, s.orgID,
		map[string]any{"status": "going"}); r.status != http.StatusOK {
		t.Fatalf("rsvp: %d %s", r.status, r.raw)
	}
	testNotes.drain()

	if n := sweep(t); n != 1 {
		t.Fatalf("expected one fixture chased, got %d", n)
	}
	who := notified(testNotes.drain())
	// The parent, because their child still owes an answer. Not the player, who has
	// already done what was asked — chasing them is how a reminder becomes noise.
	if !who[s.parentID] {
		t.Errorf("the parent of the unanswered athlete should be chased, told: %v", who)
	}
	if who[s.playerID] {
		t.Errorf("someone who already replied must not be chased, told: %v", who)
	}
	if len(who) != 1 {
		t.Errorf("expected exactly the parent, told: %v", who)
	}
}

// TestAFixtureIsChasedOnce — a reminder people learn to expect twice is a reminder they
// turn off, and the claim is what holds that across replicas as well as across ticks.
func TestAFixtureIsChasedOnce(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "once")
	if r := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach,
		map[string]any{"opponent": "Rivals", "kickoffAt": soon()}); r.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", r.status, r.raw)
	}
	testNotes.drain()

	if n := sweep(t); n != 1 {
		t.Fatalf("first sweep should chase the fixture, got %d", n)
	}
	if notes := testNotes.drain(); len(notes) == 0 {
		t.Fatal("the first sweep told nobody")
	}
	if n := sweep(t); n != 0 {
		t.Errorf("a chased fixture must not be chased again, got %d", n)
	}
	if notes := testNotes.drain(); len(notes) != 0 {
		t.Errorf("the second sweep pushed %d times", len(notes))
	}
}

// TestTheSweepMindsItsWindow — the fixtures it must leave alone. Each of these was a way
// to push at the wrong time or about the wrong thing.
func TestTheSweepMindsItsWindow(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "window")
	mk := func(payload map[string]any) string {
		t.Helper()
		r := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach, payload)
		if r.status != http.StatusCreated {
			t.Fatalf("create game: %d %s", r.status, r.raw)
		}
		return r.body["id"].(string)
	}

	mk(map[string]any{"opponent": "Next Month", "kickoffAt": far()})
	mk(map[string]any{"opponent": "No Date"})
	cancelled := mk(map[string]any{"opponent": "Called Off", "kickoffAt": soon()})
	if r := do(t, http.MethodPatch, "/api/v1/games/"+cancelled, s.coach,
		map[string]any{"status": "cancelled"}); r.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.status, r.raw)
	}
	// A match that already kicked off: a sweep that fell behind should stay quiet rather
	// than ask a squad whether they are coming to something an hour old.
	started := mk(map[string]any{"opponent": "Already Started", "kickoffAt": soon()})
	if r := do(t, http.MethodPatch, "/api/v1/games/"+started, s.coach, map[string]any{
		"kickoffAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}); r.status != http.StatusOK {
		t.Fatalf("move into the past: %d %s", r.status, r.raw)
	}
	testNotes.drain()

	if n := sweep(t); n != 0 {
		t.Errorf("none of these should be chased, got %d", n)
	}
	if notes := testNotes.drain(); len(notes) != 0 {
		t.Errorf("the sweep pushed %d times for fixtures outside its window", len(notes))
	}
}

// TestMovingAFixtureReArmsItsReminder — the fixture was chased at a time that is no
// longer when it happens, so the claim is released with the change.
func TestMovingAFixtureReArmsItsReminder(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "rearm")
	game := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach,
		map[string]any{"opponent": "Rivals", "kickoffAt": soon()})
	gameID := game.body["id"].(string)
	testNotes.drain()

	if n := sweep(t); n != 1 {
		t.Fatalf("first sweep: %d", n)
	}
	testNotes.drain()

	// Re-sending the same kickoff is not a move, and must not re-arm anything.
	same := game.body["kickoffAt"].(string)
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, s.coach,
		map[string]any{"kickoffAt": same}); r.status != http.StatusOK {
		t.Fatalf("re-send same kickoff: %d %s", r.status, r.raw)
	}
	if n := sweep(t); n != 0 {
		t.Errorf("re-sending the same time must not re-arm the reminder, got %d", n)
	}

	// An actual move does.
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, s.coach, map[string]any{
		"kickoffAt": time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339),
	}); r.status != http.StatusOK {
		t.Fatalf("move: %d %s", r.status, r.raw)
	}
	testNotes.drain()
	if n := sweep(t); n != 1 {
		t.Errorf("a moved fixture should be chased at its new time, got %d", n)
	}
	if notes := testNotes.drain(); len(notes) == 0 {
		t.Error("the re-armed sweep told nobody")
	}
}

// TestTrainingIsChasedToo — the other scheduled thing, and the one a coach schedules
// most often.
func TestTrainingIsChasedToo(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "chasetrain")
	session := do(t, http.MethodPost, "/api/v1/sessions", s.coach, map[string]any{
		"title": "Finishing", "teamId": s.teamID, "scheduledAt": soon(),
		"blocks": []map[string]any{},
	})
	if session.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", session.status, session.raw)
	}
	sessionID := session.body["id"].(string)
	// A session with no team has no roster to chase.
	if r := do(t, http.MethodPost, "/api/v1/sessions", s.coach, map[string]any{
		"title": "Planning", "scheduledAt": soon(), "blocks": []map[string]any{},
	}); r.status != http.StatusCreated {
		t.Fatalf("create teamless session: %d %s", r.status, r.raw)
	}
	testNotes.drain()

	if n := sweep(t); n != 1 {
		t.Fatalf("expected the team's session and only it, got %d", n)
	}
	notes := testNotes.drain()
	if len(notes) != 2 {
		t.Fatalf("expected the player and the parent, got %d", len(notes))
	}
	note := notes[0].note
	if !strings.Contains(note.Body, "Finishing") || !strings.Contains(note.Body, "not replied") {
		t.Errorf("the body should say what is unanswered, got %q", note.Body)
	}
	if note.Data["type"] != "session" || note.Data["eventId"] != sessionID {
		t.Errorf("the payload should deep-link to the session: %v", note.Data)
	}
}

// TestASquadThatHasAllRepliedIsLeftAlone — the good case, and it must cost nothing: the
// fixture is claimed whether or not anybody needed telling, so a sweep never returns to
// it.
func TestASquadThatHasAllRepliedIsLeftAlone(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "quiet")
	game := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach,
		map[string]any{"opponent": "Rivals", "kickoffAt": soon()})
	gameID := game.body["id"].(string)
	for _, person := range []string{s.playerID, s.mateID} {
		if r := do(t, http.MethodPut, "/api/v1/games/"+gameID+"/rsvp", s.coach,
			map[string]any{"personId": person, "status": "going"}); r.status != http.StatusOK {
			t.Fatalf("rsvp for %s: %d %s", person, r.status, r.raw)
		}
	}
	testNotes.drain()

	if n := sweep(t); n != 0 {
		t.Errorf("a squad that has all replied needs no chasing, got %d", n)
	}
	if notes := testNotes.drain(); len(notes) != 0 {
		t.Errorf("the sweep pushed %d times to a squad that had replied", len(notes))
	}
	// And it is still claimed, so a later sweep does not find it either.
	if n := sweep(t); n != 0 {
		t.Errorf("second sweep should also find nothing, got %d", n)
	}
}

// TestARecordedAttendanceIsNotAReply — the coach marking someone present is the club
// speaking, not the player. They still owe an answer.
func TestARecordedAttendanceIsNotAReply(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "notareply")
	game := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach,
		map[string]any{"opponent": "Rivals", "kickoffAt": soon()})
	gameID := game.body["id"].(string)
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID+"/attendance/"+s.playerID, s.coach,
		map[string]any{"status": "present"}); r.status != http.StatusOK {
		t.Fatalf("record: %d %s", r.status, r.raw)
	}
	testNotes.drain()

	if n := sweep(t); n != 1 {
		t.Fatalf("expected the fixture chased, got %d", n)
	}
	if who := notified(testNotes.drain()); !who[s.playerID] {
		t.Errorf("a recorded status is not the player's reply; they should still be chased: %v", who)
	}
}
