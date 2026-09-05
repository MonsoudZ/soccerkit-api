package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// aDevice registers a distinct APNs token for a caller, which is what makes them
// reachable — ListReachablePeopleForTeam deliberately skips people with no device.
func aDevice(t *testing.T, token string, n int) {
	t.Helper()
	r := do(t, http.MethodPost, "/api/v1/me/devices", token,
		map[string]any{"token": fmt.Sprintf("%064x", n)})
	if r.status != http.StatusOK {
		t.Fatalf("register device: %d %s", r.status, r.raw)
	}
}

// notified returns the person ids a batch of notifications was addressed to.
func notified(notes []recordedNote) map[string]bool {
	out := map[string]bool{}
	for _, n := range notes {
		out[n.personID.String()] = true
	}
	return out
}

// counts pulls the squad tally out of a sheet response.
func counts(t *testing.T, r resp) map[string]float64 {
	t.Helper()
	raw, ok := r.body["counts"].(map[string]any)
	if !ok {
		t.Fatalf("no counts in %s", r.raw)
	}
	out := map[string]float64{}
	for k, v := range raw {
		n, ok := v.(float64)
		if !ok {
			t.Fatalf("count %s is %T in %s", k, v, r.raw)
		}
		out[k] = n
	}
	return out
}

// entriesOf pulls the lines out of a sheet response.
func entriesOf(t *testing.T, r resp) []map[string]any {
	t.Helper()
	raw, ok := r.body["entries"].([]any)
	if !ok {
		t.Fatalf("no entries in %s", r.raw)
	}
	out := make([]map[string]any, len(raw))
	for i, e := range raw {
		out[i] = e.(map[string]any)
	}
	return out
}

// squad builds a club with a game, a training session and two rostered athletes, one of
// whom has an account and can speak for themselves.
type squad struct {
	coach, orgID     string
	player, playerID string
	mateID           string
	teamID, gameID   string
	sessionID        string
}

func newSquad(t *testing.T, prefix string) squad {
	t.Helper()
	coach, _ := signInCoach(t, prefix+"coach@e.com")
	orgID := orgOf(t, coach)
	player, playerID := signInCoach(t, prefix+"player@e.com")
	joinOrg(t, coach, orgID, player, prefix+"player@e.com", "player")

	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U12"})
	if team.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", team.status, team.raw)
	}
	teamID := team.body["id"].(string)

	mateID := createAthlete(t, coach, prefix+" Teammate")
	for _, id := range []string{playerID, mateID} {
		if r := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coach,
			map[string]any{"personId": id}); r.status != http.StatusCreated {
			t.Fatalf("roster %s: %d %s", id, r.status, r.raw)
		}
	}

	game := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/games", coach, map[string]any{
		"opponent": "Rivals FC", "kickoffAt": "2026-04-11T10:00:00Z", "homeAway": "home",
	})
	if game.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", game.status, game.raw)
	}
	session := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Tuesday", "teamId": teamID, "blocks": []map[string]any{},
	})
	if session.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", session.status, session.raw)
	}
	return squad{
		coach: coach, orgID: orgID, player: player, playerID: playerID, mateID: mateID,
		teamID: teamID, gameID: game.body["id"].(string),
		sessionID: session.body["id"].(string),
	}
}

// TestTheRegisterRoundTrips is the whole loop: a player says they are coming, the coach
// records what happened, and the sheet answers both questions at once.
func TestTheRegisterRoundTrips(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "reg")

	// Nobody has replied yet, and the empty state is a count rather than an absence.
	before := do(t, http.MethodGet, "/api/v1/games/"+s.gameID+"/attendance", s.coach, nil)
	if before.status != http.StatusOK {
		t.Fatalf("sheet: %d %s", before.status, before.raw)
	}
	if c := counts(t, before); c["noReply"] != 2 || c["notRecorded"] != 2 || c["going"] != 0 {
		t.Errorf("an unanswered squad of two should be 2 noReply / 2 notRecorded, got %v", c)
	}
	if len(entriesOf(t, before)) != 2 {
		t.Errorf("the sheet lists the squad whether or not they replied: %s", before.raw)
	}

	// The player answers for themselves.
	reply := doIn(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", s.player, s.orgID,
		map[string]any{"status": "going", "note": "bringing boots"})
	if reply.status != http.StatusOK {
		t.Fatalf("rsvp: %d %s", reply.status, reply.raw)
	}
	if reply.body["rsvp"] != "going" || reply.body["rsvpNote"] != "bringing boots" {
		t.Errorf("the reply is echoed back: %s", reply.raw)
	}
	if reply.body["rsvpBy"] != s.playerID {
		t.Errorf("rsvpBy records who answered, got %s", reply.raw)
	}
	if reply.body["rsvpAt"] == nil {
		t.Errorf("an answered line carries when it was answered: %s", reply.raw)
	}

	// Changing your mind replaces the answer rather than adding one.
	again := doIn(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", s.player, s.orgID,
		map[string]any{"status": "maybe"})
	if again.status != http.StatusOK {
		t.Fatalf("second rsvp: %d %s", again.status, again.raw)
	}
	mid := do(t, http.MethodGet, "/api/v1/games/"+s.gameID+"/attendance", s.coach, nil)
	if c := counts(t, mid); c["maybe"] != 1 || c["going"] != 0 || c["noReply"] != 1 {
		t.Errorf("a replaced reply should leave one maybe and one noReply, got %v", c)
	}

	// The coach records what actually happened.
	rec := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID+"/attendance/"+s.playerID, s.coach,
		map[string]any{"status": "late", "note": "traffic"})
	if rec.status != http.StatusOK {
		t.Fatalf("record: %d %s", rec.status, rec.raw)
	}
	if rec.body["status"] != "late" || rec.body["recordedAt"] == nil {
		t.Errorf("the recorded status and its timestamp come back: %s", rec.raw)
	}
	// The RSVP is untouched by it. They are two facts about one line, and recording the
	// second must not overwrite the first.
	if rec.body["rsvp"] != "maybe" {
		t.Errorf("recording attendance must not disturb the reply: %s", rec.raw)
	}

	after := do(t, http.MethodGet, "/api/v1/games/"+s.gameID+"/attendance", s.coach, nil)
	if c := counts(t, after); c["late"] != 1 || c["notRecorded"] != 1 {
		t.Errorf("expected one late and one not recorded, got %v", c)
	}

	// A line ticked by mistake can be untick_ed. Nothing in the vocabulary means "not
	// recorded", so explicit null has to.
	clear := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID+"/attendance/"+s.playerID, s.coach,
		map[string]any{"status": nil})
	if clear.status != http.StatusOK {
		t.Fatalf("clear: %d %s", clear.status, clear.raw)
	}
	if clear.body["status"] != nil || clear.body["recordedAt"] != nil {
		t.Errorf("clearing a status takes its provenance with it: %s", clear.raw)
	}
	if c := counts(t, do(t, http.MethodGet, "/api/v1/games/"+s.gameID+"/attendance", s.coach, nil)); c["notRecorded"] != 2 {
		t.Errorf("expected the squad back to 2 not recorded, got %v", c)
	}
}

// TestTrainingHasTheSameRegister — the second scheduled thing, through the same handlers.
func TestTrainingHasTheSameRegister(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "train")

	reply := doIn(t, http.MethodPut, "/api/v1/sessions/"+s.sessionID+"/rsvp", s.player, s.orgID,
		map[string]any{"status": "not_going", "note": "exams"})
	if reply.status != http.StatusOK {
		t.Fatalf("session rsvp: %d %s", reply.status, reply.raw)
	}
	sheet := do(t, http.MethodGet, "/api/v1/sessions/"+s.sessionID+"/attendance", s.coach, nil)
	if sheet.status != http.StatusOK {
		t.Fatalf("session sheet: %d %s", sheet.status, sheet.raw)
	}
	if sheet.body["eventType"] != "session" || sheet.body["eventId"] != s.sessionID {
		t.Errorf("the sheet names its own event: %s", sheet.raw)
	}
	if c := counts(t, sheet); c["notGoing"] != 1 || c["noReply"] != 1 {
		t.Errorf("expected one not_going and one noReply, got %v", c)
	}

	// A game's reply and a session's are separate rows: the same player is still
	// unanswered for the fixture.
	game := do(t, http.MethodGet, "/api/v1/games/"+s.gameID+"/attendance", s.coach, nil)
	if c := counts(t, game); c["noReply"] != 2 {
		t.Errorf("a session reply must not answer for the game, got %v", c)
	}
}

// TestATrainingSessionWithNoTeamHasNoRegister — a session's team is optional, and a
// register without a roster is a question with no answer rather than an empty one.
func TestATrainingSessionWithNoTeamHasNoRegister(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "noteam@e.com")
	session := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Solo planning", "blocks": []map[string]any{},
	})
	if session.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", session.status, session.raw)
	}
	id := session.body["id"].(string)
	if r := do(t, http.MethodGet, "/api/v1/sessions/"+id+"/attendance", coach, nil); r.status != http.StatusConflict {
		t.Errorf("expected 409 for a teamless session, got %d %s", r.status, r.raw)
	}
}

// TestTheRegisterIsScopedByRole is the disclosure this feature could have made and does
// not: the sheet names children, so a parent gets their own child's line and a player
// gets their own, while the tally stays the squad's.
func TestTheRegisterIsScopedByRole(t *testing.T) {
	resetDB(t)
	c := newClub(t, "att")
	game := do(t, http.MethodPost, "/api/v1/teams/"+c.teamID+"/games", c.coach,
		map[string]any{"opponent": "Rivals"})
	if game.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", game.status, game.raw)
	}
	gameID := game.body["id"].(string)

	// The parent answers for their own child.
	mine := doIn(t, http.MethodPut, "/api/v1/games/"+gameID+"/rsvp", c.parent, c.orgID,
		map[string]any{"personId": c.childID, "status": "going"})
	if mine.status != http.StatusOK {
		t.Fatalf("a parent must be able to reply for their child: %d %s", mine.status, mine.raw)
	}
	if mine.body["rsvpBy"] != c.parentID {
		t.Errorf("the reply records the parent as its author: %s", mine.raw)
	}

	// And not for anybody else's.
	other := doIn(t, http.MethodPut, "/api/v1/games/"+gameID+"/rsvp", c.parent, c.orgID,
		map[string]any{"personId": c.otherID, "status": "going"})
	if other.status != http.StatusForbidden {
		t.Errorf("replying for another family's child should be 403, got %d %s", other.status, other.raw)
	}

	// The sheet they can read is their own child's line, with the squad's tally over it.
	sheet := doIn(t, http.MethodGet, "/api/v1/games/"+gameID+"/attendance", c.parent, c.orgID, nil)
	if sheet.status != http.StatusOK {
		t.Fatalf("parent sheet: %d %s", sheet.status, sheet.raw)
	}
	entries := entriesOf(t, sheet)
	if len(entries) != 1 || entries[0]["personId"] != c.childID {
		t.Errorf("a parent sees their own child's line and no others: %s", sheet.raw)
	}
	if got := counts(t, sheet); got["going"] != 1 || got["noReply"] != 1 {
		t.Errorf("the tally is the squad's, not the caller's slice of it: %v", got)
	}

	// Recording what happened is the club's statement about a child, not a family's.
	rec := doIn(t, http.MethodPatch, "/api/v1/games/"+gameID+"/attendance/"+c.childID, c.parent,
		c.orgID, map[string]any{"status": "present"})
	if rec.status != http.StatusForbidden {
		t.Errorf("a parent recording attendance should be 403, got %d %s", rec.status, rec.raw)
	}
}

// TestAnUnconnectedMemberSeesNoRegister — the counts are aggregate, but they are still
// the club's, and a member with nobody at the fixture has no business reading them.
func TestAnUnconnectedMemberSeesNoRegister(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "outsider")
	bystander, _ := signInCoach(t, "outsider-bystander@e.com")
	joinOrg(t, s.coach, s.orgID, bystander, "outsider-bystander@e.com", "player")

	r := doIn(t, http.MethodGet, "/api/v1/games/"+s.gameID+"/attendance", bystander, s.orgID, nil)
	if r.status != http.StatusForbidden {
		t.Errorf("a member connected to nobody at this fixture should be 403, got %d %s", r.status, r.raw)
	}
	// And cannot reply themselves into it either: they are on no roster, so there is no
	// line to open.
	reply := doIn(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", bystander, s.orgID,
		map[string]any{"status": "going"})
	if reply.status != http.StatusNotFound {
		t.Errorf("replying for a fixture you are not rostered for should be 404, got %d %s",
			reply.status, reply.raw)
	}
}

// TestRepliesAreRefusedForACancelledFixture — "going" to a match that will not be played
// is a coaching signal that is simply wrong, and it would sit in the sheet looking true.
func TestRepliesAreRefusedForACancelledFixture(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "cancel")
	if r := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID, s.coach,
		map[string]any{"status": "cancelled"}); r.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.status, r.raw)
	}
	reply := doIn(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", s.player, s.orgID,
		map[string]any{"status": "going"})
	if reply.status != http.StatusConflict {
		t.Errorf("expected 409 for a cancelled fixture, got %d %s", reply.status, reply.raw)
	}
	// Recording who turned up is still allowed: a match called off at the ground is one
	// the squad travelled to, and that is exactly the register a coach wants afterwards.
	rec := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID+"/attendance/"+s.playerID, s.coach,
		map[string]any{"status": "present"})
	if rec.status != http.StatusOK {
		t.Errorf("recording attendance at a cancelled fixture should still work, got %d %s",
			rec.status, rec.raw)
	}
}

// TestALineSurvivesLeavingTheRoster — the register is a record of an event, not of the
// current squad. Driving it off active memberships alone would erase a player from the
// match they actually played in.
func TestALineSurvivesLeavingTheRoster(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "left")
	if r := doIn(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", s.player, s.orgID,
		map[string]any{"status": "going"}); r.status != http.StatusOK {
		t.Fatalf("rsvp: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID+"/attendance/"+s.playerID, s.coach,
		map[string]any{"status": "present"}); r.status != http.StatusOK {
		t.Fatalf("record: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodDelete, "/api/v1/teams/"+s.teamID+"/roster/"+s.playerID, s.coach, nil); r.status != http.StatusOK {
		t.Fatalf("end roster: %d %s", r.status, r.raw)
	}

	sheet := do(t, http.MethodGet, "/api/v1/games/"+s.gameID+"/attendance", s.coach, nil)
	var line map[string]any
	for _, e := range entriesOf(t, sheet) {
		if e["personId"] == s.playerID {
			line = e
		}
	}
	if line == nil {
		t.Fatalf("the player who was at the match is gone from its register: %s", sheet.raw)
	}
	if line["status"] != "present" {
		t.Errorf("their recorded attendance should be intact: %v", line)
	}
	if line["onRoster"] != false {
		t.Errorf("onRoster is what tells history from squad, got %v", line["onRoster"])
	}
	if c := counts(t, sheet); c["present"] != 1 {
		t.Errorf("the tally still counts them, got %v", c)
	}
}

// TestTheRegisterIsIsolatedByOrg — the same tenancy check every other team-scoped route
// makes. A register is a list of named children.
func TestTheRegisterIsIsolatedByOrg(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "iso")
	stranger, _ := signInCoach(t, "iso-stranger@e.com")

	for _, tc := range []struct {
		name           string
		method, path   string
		payload        any
		wantStatusCode int
	}{
		{"read the game sheet", http.MethodGet, "/api/v1/games/" + s.gameID + "/attendance", nil, http.StatusForbidden},
		{"reply to the game", http.MethodPut, "/api/v1/games/" + s.gameID + "/rsvp",
			map[string]any{"status": "going"}, http.StatusForbidden},
		{"record at the game", http.MethodPatch, "/api/v1/games/" + s.gameID + "/attendance/" + s.playerID,
			map[string]any{"status": "present"}, http.StatusForbidden},
		{"read the session sheet", http.MethodGet, "/api/v1/sessions/" + s.sessionID + "/attendance", nil, http.StatusForbidden},
	} {
		r := do(t, tc.method, tc.path, stranger, tc.payload)
		if r.status != tc.wantStatusCode {
			t.Errorf("%s from another org: got %d, want %d (%s)", tc.name, r.status, tc.wantStatusCode, r.raw)
		}
	}
}

// TestTheRegisterValidatesItsVocabularies — the two are deliberately different words, and
// neither accepts the other's.
func TestTheRegisterValidatesItsVocabularies(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "vocab")

	if r := doIn(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", s.player, s.orgID,
		map[string]any{"status": "present"}); r.status != http.StatusBadRequest {
		t.Errorf("an attendance status is not an RSVP, expected 400, got %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID+"/attendance/"+s.playerID, s.coach,
		map[string]any{"status": "going"}); r.status != http.StatusBadRequest {
		t.Errorf("an RSVP is not an attendance status, expected 400, got %d %s", r.status, r.raw)
	}
	// A reply with no status at all is a client bug, not an empty reply.
	if r := doIn(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", s.player, s.orgID,
		map[string]any{}); r.status != http.StatusBadRequest {
		t.Errorf("expected 400 for a reply with no status, got %d %s", r.status, r.raw)
	}
	// And a person who has nothing to do with the fixture cannot be given a line.
	outsider := createAthlete(t, s.coach, "Not On This Team")
	if r := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID+"/attendance/"+outsider, s.coach,
		map[string]any{"status": "present"}); r.status != http.StatusNotFound {
		t.Errorf("expected 404 recording for someone off the roster, got %d %s", r.status, r.raw)
	}
}

// TestRecordingIsAPatchAndReplyingIsAPut pins the two halves' different semantics, which
// are the easiest thing here to get wrong in a way nobody notices: a coach adding a note
// must not erase the status it annotates, and a reply sent without a note must not keep
// the note from the last one.
func TestRecordingIsAPatchAndReplyingIsAPut(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "patch")
	line := "/api/v1/games/" + s.gameID + "/attendance/" + s.playerID

	if r := do(t, http.MethodPatch, line, s.coach, map[string]any{"status": "present"}); r.status != http.StatusOK {
		t.Fatalf("record: %d %s", r.status, r.raw)
	}
	// An absent key leaves that half alone.
	noted := do(t, http.MethodPatch, line, s.coach, map[string]any{"note": "arrived early"})
	if noted.status != http.StatusOK {
		t.Fatalf("note: %d %s", noted.status, noted.raw)
	}
	if noted.body["status"] != "present" || noted.body["statusNote"] != "arrived early" {
		t.Errorf("adding a note must not erase the status: %s", noted.raw)
	}
	// An explicit null clears it, and takes only its own half.
	cleared := do(t, http.MethodPatch, line, s.coach, map[string]any{"status": nil})
	if cleared.body["status"] != nil || cleared.body["statusNote"] != "arrived early" {
		t.Errorf("null clears the status and nothing else: %s", cleared.raw)
	}
	// A misspelled field is a client bug, reported rather than silently dropped.
	if r := do(t, http.MethodPatch, line, s.coach, map[string]any{"attended": true}); r.status != http.StatusBadRequest {
		t.Errorf("expected 400 for an unknown field, got %d %s", r.status, r.raw)
	}

	// The reply is a PUT of one whole answer: no note means no note.
	rsvp := "/api/v1/games/" + s.gameID + "/rsvp"
	if r := doIn(t, http.MethodPut, rsvp, s.player, s.orgID,
		map[string]any{"status": "going", "note": "bringing boots"}); r.status != http.StatusOK {
		t.Fatalf("first reply: %d %s", r.status, r.raw)
	}
	second := doIn(t, http.MethodPut, rsvp, s.player, s.orgID, map[string]any{"status": "not_going"})
	if second.status != http.StatusOK {
		t.Fatalf("second reply: %d %s", second.status, second.raw)
	}
	if second.body["rsvpNote"] != nil {
		t.Errorf("a replacement reply carries no note from the one it replaced: %s", second.raw)
	}
}

// TestStaffMayReplyForAPlayer — the common case in youth football: the player is nine,
// has no login, and the reply arrived as a text message to the coach.
func TestStaffMayReplyForAPlayer(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "onbehalf")

	r := do(t, http.MethodPut, "/api/v1/games/"+s.gameID+"/rsvp", s.coach,
		map[string]any{"personId": s.mateID, "status": "going", "note": "dad texted"})
	if r.status != http.StatusOK {
		t.Fatalf("staff reply on behalf: %d %s", r.status, r.raw)
	}
	if r.body["personId"] != s.mateID {
		t.Errorf("the line belongs to the player, got %s", r.raw)
	}
	// Who spoke is recorded rather than assumed: "the coach entered this" and "the family
	// told us" are different facts.
	if r.body["rsvpBy"] == s.mateID {
		t.Errorf("rsvpBy should be the coach who entered it, got %s", r.raw)
	}
}

// --- being asked -------------------------------------------------------------

// askedSquad is newSquad with the people who can actually be reached: the player and a
// parent of the other athlete, each with a device, and the coach with one too so the
// actor exclusion has something to prove.
type askedSquad struct {
	squad
	parent, parentID string
}

func newAskedSquad(t *testing.T, prefix string) askedSquad {
	t.Helper()
	s := newSquad(t, prefix)
	parent, parentID := signInCoach(t, prefix+"parent@e.com")
	joinOrg(t, s.coach, s.orgID, parent, prefix+"parent@e.com", "parent")
	if r := do(t, http.MethodPost, "/api/v1/persons/"+s.mateID+"/guardians", s.coach,
		map[string]any{"personId": parentID}); r.status != http.StatusCreated {
		t.Fatalf("link guardian: %d %s", r.status, r.raw)
	}
	aDevice(t, s.player, 1)
	aDevice(t, parent, 2)
	aDevice(t, s.coach, 3)
	testNotes.drain()
	return askedSquad{squad: s, parent: parent, parentID: parentID}
}

// TestSchedulingAFixtureAsksTheSquad is what makes the register work rather than exist.
// Without it a coach schedules a game, a sheet full of "has not replied" appears, and
// nobody is ever told there is anything to reply to.
func TestSchedulingAFixtureAsksTheSquad(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "ask")

	game := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach, map[string]any{
		"opponent": "Rivals FC", "kickoffAt": "2026-05-02T09:30:00Z",
	})
	if game.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", game.status, game.raw)
	}
	notes := testNotes.drain()
	who := notified(notes)

	// The player, because they are on the roster; the parent, through the child they are
	// a recorded guardian of.
	if !who[s.playerID] {
		t.Errorf("the rostered player should be asked, told: %v", who)
	}
	if !who[s.parentID] {
		t.Errorf("a parent should be asked through their child, told: %v", who)
	}
	// The athlete with no account has no device and nothing to receive on; the coach made
	// the fixture and does not need their own phone to tell them.
	if who[s.mateID] {
		t.Errorf("an athlete with no device should not be queued, told: %v", who)
	}
	if len(who) != 2 {
		t.Errorf("expected exactly the player and the parent, told: %v", who)
	}

	// The payload is what a tap acts on. No time in the text -- the server does not know
	// the club's timezone -- so the instant rides here instead.
	note := notes[0].note
	if note.Data["type"] != "game" || note.Data["eventId"] != game.body["id"].(string) {
		t.Errorf("the payload should name the fixture: %v", note.Data)
	}
	if note.Data["teamId"] != s.teamID || note.Data["action"] != "rsvp" {
		t.Errorf("the payload should deep-link to the reply: %v", note.Data)
	}
	if note.Data["startsAt"] != "2026-05-02T09:30:00Z" {
		t.Errorf("the kickoff instant should ride in the payload, got %q", note.Data["startsAt"])
	}
	if !strings.Contains(note.Body, "Rivals FC") {
		t.Errorf("the body should say who they are playing, got %q", note.Body)
	}
	// A rendered time would be the server's UTC, which is the wrong day for half the
	// world. The app formats startsAt in the device's own zone instead.
	if strings.Contains(note.Body, "09:30") || strings.Contains(note.Title, "09:30") {
		t.Errorf("the text must not render a time: %q / %q", note.Title, note.Body)
	}
}

// TestOnlyChangesWorthActingOnArePushed — a push per scoreline would train a squad to
// swipe these away, which costs the two that matter.
func TestOnlyChangesWorthActingOnArePushed(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "moved")
	game := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach, map[string]any{
		"opponent": "Rivals FC", "kickoffAt": "2026-05-02T09:30:00Z",
	})
	gameID := game.body["id"].(string)
	testNotes.drain()

	quiet := []struct {
		name    string
		payload map[string]any
	}{
		{"a corrected opponent", map[string]any{"opponent": "Rivals AFC"}},
		{"a scoreline at full time", map[string]any{"ourScore": 2, "opponentScore": 1}},
		{"a status that is not a cancellation", map[string]any{"status": "completed"}},
		{"the same kickoff sent again", map[string]any{"kickoffAt": "2026-05-02T09:30:00Z"}},
	}
	for _, q := range quiet {
		r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, s.coach, q.payload)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", q.name, r.status, r.raw)
		}
		if notes := testNotes.drain(); len(notes) != 0 {
			t.Errorf("%s should not push, got %d notifications", q.name, len(notes))
		}
	}

	// A kickoff that actually moves is the squad's business.
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, s.coach,
		map[string]any{"kickoffAt": "2026-05-02T14:00:00Z"}); r.status != http.StatusOK {
		t.Fatalf("move kickoff: %d %s", r.status, r.raw)
	}
	moved := testNotes.drain()
	if len(moved) != 2 {
		t.Fatalf("a moved kickoff should reach the player and the parent, got %d", len(moved))
	}
	if !strings.Contains(moved[0].note.Title, "Kickoff changed") {
		t.Errorf("expected a kickoff notice, got %q", moved[0].note.Title)
	}
	if moved[0].note.Data["startsAt"] != "2026-05-02T14:00:00Z" {
		t.Errorf("the payload should carry the new time, got %q", moved[0].note.Data["startsAt"])
	}

	// And so is a fixture that is off.
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, s.coach,
		map[string]any{"status": "cancelled"}); r.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.status, r.raw)
	}
	off := testNotes.drain()
	if len(off) != 2 {
		t.Fatalf("a cancellation should reach the squad, got %d", len(off))
	}
	if !strings.Contains(off[0].note.Title, "cancelled") || !strings.Contains(off[0].note.Body, "is off") {
		t.Errorf("expected a cancellation notice, got %q / %q", off[0].note.Title, off[0].note.Body)
	}
	// Cancelling twice is not news.
	if r := do(t, http.MethodPatch, "/api/v1/games/"+gameID, s.coach,
		map[string]any{"status": "cancelled"}); r.status != http.StatusOK {
		t.Fatalf("re-cancel: %d %s", r.status, r.raw)
	}
	if notes := testNotes.drain(); len(notes) != 0 {
		t.Errorf("cancelling an already-cancelled fixture should not push again, got %d", len(notes))
	}
}

// TestSchedulingTrainingAsksTheSquad — training a squad is expected at is the same ask,
// and a plan with no team is not an event anybody attends.
func TestSchedulingTrainingAsksTheSquad(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "trainask")

	session := do(t, http.MethodPost, "/api/v1/sessions", s.coach, map[string]any{
		"title": "Finishing", "teamId": s.teamID, "scheduledAt": "2026-05-01T17:00:00Z",
		"blocks": []map[string]any{},
	})
	if session.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", session.status, session.raw)
	}
	notes := testNotes.drain()
	if len(notes) != 2 {
		t.Fatalf("training should reach the player and the parent, got %d", len(notes))
	}
	note := notes[0].note
	if note.Data["type"] != "session" || note.Data["eventId"] != session.body["id"].(string) {
		t.Errorf("the payload should name the session: %v", note.Data)
	}
	if !strings.Contains(note.Body, "Finishing") {
		t.Errorf("the body should name the session, got %q", note.Body)
	}

	// A session a coach is drafting for themselves has no roster to tell.
	solo := do(t, http.MethodPost, "/api/v1/sessions", s.coach, map[string]any{
		"title": "Planning", "blocks": []map[string]any{},
	})
	if solo.status != http.StatusCreated {
		t.Fatalf("create teamless session: %d %s", solo.status, solo.raw)
	}
	if n := testNotes.drain(); len(n) != 0 {
		t.Errorf("a teamless session should tell nobody, got %d", len(n))
	}
}

// TestAPlayerWhoLeftIsNoLongerAsked — the notify audience is the active roster, unlike
// the sheet, which keeps a line for whoever was at the event. Asking someone who left the
// club whether they are coming is the wrong question and, to their family, a strange one.
func TestAPlayerWhoLeftIsNoLongerAsked(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "gone")
	if r := do(t, http.MethodDelete, "/api/v1/teams/"+s.teamID+"/roster/"+s.playerID, s.coach, nil); r.status != http.StatusOK {
		t.Fatalf("end roster: %d %s", r.status, r.raw)
	}
	testNotes.drain()

	if r := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach,
		map[string]any{"opponent": "Rivals"}); r.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", r.status, r.raw)
	}
	who := notified(testNotes.drain())
	if who[s.playerID] {
		t.Errorf("a player who left the team should not be asked, told: %v", who)
	}
	if !who[s.parentID] {
		t.Errorf("the remaining squad's parent should still be asked, told: %v", who)
	}
}

// TestMovingTrainingTellsTheSquad — the half that had no endpoint to move through until
// PATCH /sessions/{id} existed. A moved kickoff pushed and moved training did not, which
// is the more common change of the two.
func TestMovingTrainingTellsTheSquad(t *testing.T) {
	resetDB(t)
	s := newAskedSquad(t, "movetrain")
	session := do(t, http.MethodPost, "/api/v1/sessions", s.coach, map[string]any{
		"title": "Finishing", "teamId": s.teamID, "scheduledAt": "2026-05-01T17:00:00Z",
		"blocks": []map[string]any{},
	})
	if session.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", session.status, session.raw)
	}
	id := session.body["id"].(string)
	testNotes.drain()

	// A rename is not something anybody has to act on.
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, s.coach,
		map[string]any{"title": "Finishing & crossing"}); r.status != http.StatusOK {
		t.Fatalf("rename: %d %s", r.status, r.raw)
	}
	if n := testNotes.drain(); len(n) != 0 {
		t.Errorf("a rename should not push, got %d", len(n))
	}

	// A new time is.
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+id, s.coach,
		map[string]any{"scheduledAt": "2026-05-01T19:00:00Z"}); r.status != http.StatusOK {
		t.Fatalf("move: %d %s", r.status, r.raw)
	}
	notes := testNotes.drain()
	if len(notes) != 2 {
		t.Fatalf("a moved session should reach the player and the parent, got %d", len(notes))
	}
	note := notes[0].note
	if !strings.Contains(note.Title, "Training moved") {
		t.Errorf("expected a moved-training notice, got %q", note.Title)
	}
	if note.Data["type"] != "session" || note.Data["eventId"] != id {
		t.Errorf("the payload should name the session: %v", note.Data)
	}
	if note.Data["startsAt"] != "2026-05-01T19:00:00Z" {
		t.Errorf("the payload should carry the new time, got %q", note.Data["startsAt"])
	}
}

// --- the season, not the Saturday --------------------------------------------

// recordFor pulls one player's line out of a team attendance response.
func recordFor(t *testing.T, r resp, personID string) map[string]any {
	t.Helper()
	rows, ok := r.body["records"].([]any)
	if !ok {
		t.Fatalf("no records in %s", r.raw)
	}
	for _, row := range rows {
		m := row.(map[string]any)
		if m["personId"] == personID {
			return m
		}
	}
	t.Fatalf("no record for %s in %s", personID, r.raw)
	return nil
}

// TestTheSeasonReadsDownNotAcross is the question a coach actually has. The per-event
// sheet answers "who is coming on Saturday"; nobody asks that twice.
func TestTheSeasonReadsDownNotAcross(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "season")

	// Three training sessions and a match, all on the team.
	var sessions []string
	for _, day := range []string{"2026-03-03", "2026-03-10", "2026-03-17"} {
		r := do(t, http.MethodPost, "/api/v1/sessions", s.coach, map[string]any{
			"title": day, "teamId": s.teamID, "scheduledAt": day + "T18:00:00Z",
			"blocks": []map[string]any{},
		})
		if r.status != http.StatusCreated {
			t.Fatalf("create session: %d %s", r.status, r.raw)
		}
		sessions = append(sessions, r.body["id"].(string))
	}
	game := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach, map[string]any{
		"opponent": "Rivals", "kickoffAt": "2026-03-21T10:00:00Z",
	})
	gameID := game.body["id"].(string)

	// The player turns up twice, is late once, and misses the match having said they
	// were coming — the no-show a per-fixture sheet cannot surface.
	mark := func(path, person, status string) {
		t.Helper()
		if r := do(t, http.MethodPatch, path+"/attendance/"+person, s.coach,
			map[string]any{"status": status}); r.status != http.StatusOK {
			t.Fatalf("mark %s: %d %s", status, r.status, r.raw)
		}
	}
	mark("/api/v1/sessions/"+sessions[0], s.playerID, "present")
	mark("/api/v1/sessions/"+sessions[1], s.playerID, "present")
	mark("/api/v1/sessions/"+sessions[2], s.playerID, "late")
	if r := do(t, http.MethodPut, "/api/v1/games/"+gameID+"/rsvp", s.coach,
		map[string]any{"personId": s.playerID, "status": "going"}); r.status != http.StatusOK {
		t.Fatalf("rsvp: %d %s", r.status, r.raw)
	}
	mark("/api/v1/games/"+gameID, s.playerID, "absent")
	// The teammate is marked at nothing at all.

	all := do(t, http.MethodGet, "/api/v1/teams/"+s.teamID+"/attendance", s.coach, nil)
	if all.status != http.StatusOK {
		t.Fatalf("aggregate: %d %s", all.status, all.raw)
	}
	// newSquad already made a game and a session, so the window is those two plus these
	// four. What matters is that every player is counted against the same number.
	events, _ := all.body["events"].(float64)
	if events != 6 {
		t.Fatalf("expected six events in the window, got %v (%s)", events, all.raw)
	}

	player := recordFor(t, all, s.playerID)
	if player["present"].(float64) != 2 || player["late"].(float64) != 1 || player["absent"].(float64) != 1 {
		t.Errorf("the player's season is wrong: %v", player)
	}
	if player["noShows"].(float64) != 1 {
		t.Errorf("said going and did not turn up should be a no-show: %v", player)
	}
	// present+late over present+late+absent = 3/4.
	if rate := player["rate"].(float64); rate != 0.75 {
		t.Errorf("rate = %v, want 0.75", rate)
	}

	// The teammate was never marked. That must read as "we do not know", not as a clean
	// record — the whole reason notRecorded is reported separately.
	mate := recordFor(t, all, s.mateID)
	if mate["notRecorded"].(float64) != 6 {
		t.Errorf("an unmarked player should show every event as not recorded: %v", mate)
	}
	if mate["rate"] != nil {
		t.Errorf("a rate over no observations is unknown, not zero: %v", mate["rate"])
	}
}

// TestTheSeasonCanBeNarrowed — "who keeps missing training" is about one kind of event,
// and a season is a window rather than all of history.
func TestTheSeasonCanBeNarrowed(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "narrow")
	if r := do(t, http.MethodPost, "/api/v1/sessions", s.coach, map[string]any{
		"title": "March", "teamId": s.teamID, "scheduledAt": "2026-03-03T18:00:00Z",
		"blocks": []map[string]any{},
	}); r.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPost, "/api/v1/teams/"+s.teamID+"/games", s.coach, map[string]any{
		"opponent": "Rivals", "kickoffAt": "2026-03-21T10:00:00Z",
	}); r.status != http.StatusCreated {
		t.Fatalf("create game: %d %s", r.status, r.raw)
	}

	base := "/api/v1/teams/" + s.teamID + "/attendance"
	eventsIn := func(query string) float64 {
		t.Helper()
		r := do(t, http.MethodGet, base+query, s.coach, nil)
		if r.status != http.StatusOK {
			t.Fatalf("aggregate%s: %d %s", query, r.status, r.raw)
		}
		n, _ := r.body["events"].(float64)
		return n
	}

	// The window. newSquad's own fixtures are in April and outside it.
	if got := eventsIn("?from=2026-03-01&to=2026-03-31"); got != 2 {
		t.Errorf("March should hold two events, got %v", got)
	}
	if got := eventsIn("?from=2026-03-01&to=2026-03-10"); got != 1 {
		t.Errorf("the first half of March should hold one, got %v", got)
	}
	// An inclusive single day, not an empty instant.
	if got := eventsIn("?from=2026-03-21&to=2026-03-21"); got != 1 {
		t.Errorf("a one-day window should hold that day's match, got %v", got)
	}
	// The kind.
	if got := eventsIn("?from=2026-03-01&to=2026-03-31&type=session"); got != 1 {
		t.Errorf("March holds one training session, got %v", got)
	}
	if got := eventsIn("?from=2026-03-01&to=2026-03-31&type=game"); got != 1 {
		t.Errorf("March holds one match, got %v", got)
	}

	for _, bad := range []string{"?from=March", "?to=2026-13-01", "?type=practice", "?from=2026-03-31&to=2026-03-01"} {
		if r := do(t, http.MethodGet, base+bad, s.coach, nil); r.status != http.StatusBadRequest {
			t.Errorf("%s should be 400, got %d %s", bad, r.status, r.raw)
		}
	}
}

// TestACancelledMatchIsNotAnAbsence — nobody attends a match that was called off, and
// counting it against a squad would punish them for a coach's decision.
func TestACancelledMatchIsNotAnAbsence(t *testing.T) {
	resetDB(t)
	s := newSquad(t, "calledoff")
	before := do(t, http.MethodGet, "/api/v1/teams/"+s.teamID+"/attendance", s.coach, nil)
	was, _ := before.body["events"].(float64)

	if r := do(t, http.MethodPatch, "/api/v1/games/"+s.gameID, s.coach,
		map[string]any{"status": "cancelled"}); r.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.status, r.raw)
	}
	after := do(t, http.MethodGet, "/api/v1/teams/"+s.teamID+"/attendance", s.coach, nil)
	now, _ := after.body["events"].(float64)
	if now != was-1 {
		t.Errorf("a cancelled match should leave the count, was %v now %v", was, now)
	}
}

// TestTheSeasonIsScopedLikeTheSheet — a column of absences beside a child's name is a
// more pointed disclosure than one fixture's line, not a lesser one.
func TestTheSeasonIsScopedLikeTheSheet(t *testing.T) {
	resetDB(t)
	c := newClub(t, "seasonscope")
	if r := do(t, http.MethodPost, "/api/v1/sessions", c.coach, map[string]any{
		"title": "Tuesday", "teamId": c.teamID, "scheduledAt": "2026-03-03T18:00:00Z",
		"blocks": []map[string]any{},
	}); r.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", r.status, r.raw)
	}

	// The coach sees the squad.
	staff := do(t, http.MethodGet, "/api/v1/teams/"+c.teamID+"/attendance", c.coach, nil)
	if staff.status != http.StatusOK {
		t.Fatalf("staff aggregate: %d %s", staff.status, staff.raw)
	}
	if rows := staff.body["records"].([]any); len(rows) != 2 {
		t.Fatalf("a coach sees the squad, got %d", len(rows))
	}

	// The parent sees their own child and nobody else's — with the squad's event count
	// over it, the same way the per-fixture sheet keeps its counts whole.
	parent := doIn(t, http.MethodGet, "/api/v1/teams/"+c.teamID+"/attendance", c.parent, c.orgID, nil)
	if parent.status != http.StatusOK {
		t.Fatalf("parent aggregate: %d %s", parent.status, parent.raw)
	}
	rows := parent.body["records"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["personId"] != c.childID {
		t.Errorf("a parent sees their own child's record and no others: %s", parent.raw)
	}
	if events, _ := parent.body["events"].(float64); events != 1 {
		t.Errorf("the event count is the squad's, got %v", events)
	}

	// And a stranger gets nothing at all.
	stranger, _ := signInCoach(t, "seasonscope-stranger@e.com")
	if r := do(t, http.MethodGet, "/api/v1/teams/"+c.teamID+"/attendance", stranger, nil); r.status != http.StatusForbidden {
		t.Errorf("cross-org aggregate should be 403, got %d %s", r.status, r.raw)
	}
}

// TestAnAthletesRecordFollowsThemBetweenTeams is the reason this is not just the team
// aggregate filtered by person: an athlete who moved up in January did not stop having
// attended the autumn, and their current roster is the wrong universe for their record.
func TestAnAthletesRecordFollowsThemBetweenTeams(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "career-coach@e.com")
	young := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U12"})
	older := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U14"})
	youngID, olderID := young.body["id"].(string), older.body["id"].(string)
	athlete := createAthlete(t, coach, "Moved Up")

	// Autumn on U12, then a transfer, then spring on U14.
	if r := do(t, http.MethodPost, "/api/v1/teams/"+youngID+"/roster", coach,
		map[string]any{"personId": athlete, "joinedOn": "2025-09-01"}); r.status != http.StatusCreated {
		t.Fatalf("roster U12: %d %s", r.status, r.raw)
	}
	autumn := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Autumn", "teamId": youngID, "scheduledAt": "2025-10-07T18:00:00Z",
		"blocks": []map[string]any{},
	})
	if autumn.status != http.StatusCreated {
		t.Fatalf("autumn session: %d %s", autumn.status, autumn.raw)
	}
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+autumn.body["id"].(string)+
		"/attendance/"+athlete, coach, map[string]any{"status": "present"}); r.status != http.StatusOK {
		t.Fatalf("mark autumn: %d %s", r.status, r.raw)
	}
	// They leave U12 at the end of December and join U14.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE roster_memberships SET left_on = '2025-12-31', status = 'inactive'
		 WHERE person_id = $1 AND team_id = $2`, athlete, youngID); err != nil {
		t.Fatalf("end membership: %v", err)
	}
	if r := do(t, http.MethodPost, "/api/v1/teams/"+olderID+"/roster", coach,
		map[string]any{"personId": athlete, "joinedOn": "2026-01-01"}); r.status != http.StatusCreated {
		t.Fatalf("roster U14: %d %s", r.status, r.raw)
	}
	spring := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Spring", "teamId": olderID, "scheduledAt": "2026-02-03T18:00:00Z",
		"blocks": []map[string]any{},
	})
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+spring.body["id"].(string)+
		"/attendance/"+athlete, coach, map[string]any{"status": "absent"}); r.status != http.StatusOK {
		t.Fatalf("mark spring: %d %s", r.status, r.raw)
	}

	// A U12 session held after they left is not theirs to have missed.
	after := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "After they left", "teamId": youngID, "scheduledAt": "2026-02-10T18:00:00Z",
		"blocks": []map[string]any{},
	})
	if after.status != http.StatusCreated {
		t.Fatalf("later U12 session: %d %s", after.status, after.raw)
	}

	rec := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/attendance", coach, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("person attendance: %d %s", rec.status, rec.raw)
	}
	if events, _ := rec.body["events"].(float64); events != 2 {
		t.Errorf("their record is the two events they were rostered for, got %v (%s)", events, rec.raw)
	}
	teams := rec.body["teams"].([]any)
	if len(teams) != 2 {
		t.Fatalf("expected a line per team, got %s", rec.raw)
	}
	byName := map[string]map[string]any{}
	for _, row := range teams {
		m := row.(map[string]any)
		byName[m["teamName"].(string)] = m
	}
	if got := byName["U12"]; got["present"].(float64) != 1 || got["events"].(float64) != 1 {
		t.Errorf("the autumn on U12 should survive the move: %v", got)
	}
	if got := byName["U14"]; got["absent"].(float64) != 1 {
		t.Errorf("the spring on U14 is theirs too: %v", got)
	}
	// One turned up of two known, across both teams — and the overall rate is computed
	// from the totals, not averaged from the two lines (which would also be 0.5 here only
	// by coincidence of equal sizes, so the sizes are checked above).
	overall := rec.body["overall"].(map[string]any)
	if overall["present"].(float64) != 1 || overall["absent"].(float64) != 1 {
		t.Errorf("overall should sum the teams: %v", overall)
	}
	if overall["rate"].(float64) != 0.5 {
		t.Errorf("overall rate = %v, want 0.5", overall["rate"])
	}

	// Narrowed to one team, it is that season alone.
	one := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/attendance?teamId="+youngID, coach, nil)
	if one.status != http.StatusOK {
		t.Fatalf("narrowed: %d %s", one.status, one.raw)
	}
	if len(one.body["teams"].([]any)) != 1 || one.body["events"].(float64) != 1 {
		t.Errorf("teamId should narrow to that team's events: %s", one.raw)
	}
}

// TestAnAthletesRecordIsScopedLikeThePerson — the same gate the rest of /persons takes,
// which is what keeps one family out of another's.
func TestAnAthletesRecordIsScopedLikeThePerson(t *testing.T) {
	resetDB(t)
	c := newClub(t, "recscope")
	if r := do(t, http.MethodPost, "/api/v1/sessions", c.coach, map[string]any{
		"title": "Tuesday", "teamId": c.teamID, "scheduledAt": soon(),
		"blocks": []map[string]any{},
	}); r.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", r.status, r.raw)
	}

	mine := doIn(t, http.MethodGet, "/api/v1/persons/"+c.childID+"/attendance", c.parent, c.orgID, nil)
	if mine.status != http.StatusOK {
		t.Fatalf("a parent must see their own child's record: %d %s", mine.status, mine.raw)
	}
	if mine.body["events"].(float64) != 1 {
		t.Errorf("expected the one session, got %s", mine.raw)
	}
	// Another family's child is 404, not 403: these ids are not enumerable and answering
	// "forbidden" would confirm one exists.
	if r := doIn(t, http.MethodGet, "/api/v1/persons/"+c.otherID+"/attendance", c.parent,
		c.orgID, nil); r.status != http.StatusNotFound {
		t.Errorf("another family's child should be 404, got %d %s", r.status, r.raw)
	}
	stranger, _ := signInCoach(t, "recscope-stranger@e.com")
	if r := do(t, http.MethodGet, "/api/v1/persons/"+c.childID+"/attendance", stranger, nil); r.status != http.StatusNotFound {
		t.Errorf("a stranger should be 404, got %d %s", r.status, r.raw)
	}
}

// TestAnAthletesRecordStopsAtTheOrganization — a person may be rostered in two clubs, and
// the caller was cleared to see them in exactly one.
func TestAnAthletesRecordStopsAtTheOrganization(t *testing.T) {
	resetDB(t)
	// One club, with a coach who can see the athlete.
	here, _ := signInCoach(t, "twoclub-here@e.com")
	hereOrg := orgOf(t, here)
	athlete := createAthlete(t, here, "Two Clubs")
	hereTeam := do(t, http.MethodPost, "/api/v1/teams", here, map[string]any{"name": "Here U12"})
	hereTeamID := hereTeam.body["id"].(string)
	if r := do(t, http.MethodPost, "/api/v1/teams/"+hereTeamID+"/roster", here,
		map[string]any{"personId": athlete}); r.status != http.StatusCreated {
		t.Fatalf("roster here: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPost, "/api/v1/sessions", here, map[string]any{
		"title": "Here", "teamId": hereTeamID, "scheduledAt": soon(),
		"blocks": []map[string]any{},
	}); r.status != http.StatusCreated {
		t.Fatalf("session here: %d %s", r.status, r.raw)
	}

	// A second club rosters the same athlete and trains twice.
	there, _ := signInCoach(t, "twoclub-there@e.com")
	thereTeam := do(t, http.MethodPost, "/api/v1/teams", there, map[string]any{"name": "There U12"})
	thereTeamID := thereTeam.body["id"].(string)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO roster_memberships (person_id, team_id) VALUES ($1, $2)`,
		athlete, thereTeamID); err != nil {
		t.Fatalf("roster there: %v", err)
	}
	if r := do(t, http.MethodPost, "/api/v1/sessions", there, map[string]any{
		"title": "There", "teamId": thereTeamID, "scheduledAt": soon(),
		"blocks": []map[string]any{},
	}); r.status != http.StatusCreated {
		t.Fatalf("session there: %d %s", r.status, r.raw)
	}

	rec := doIn(t, http.MethodGet, "/api/v1/persons/"+athlete+"/attendance", here, hereOrg, nil)
	if rec.status != http.StatusOK {
		t.Fatalf("person attendance: %d %s", rec.status, rec.raw)
	}
	teams := rec.body["teams"].([]any)
	if len(teams) != 1 || teams[0].(map[string]any)["teamName"] != "Here U12" {
		t.Errorf("the other club's team must not appear: %s", rec.raw)
	}
	if rec.body["events"].(float64) != 1 {
		t.Errorf("only this organization's events count: %s", rec.raw)
	}
}

// TestARecordedFactOutranksTheRosterWindow — the roster window says which events an
// athlete was expected at, but a coach who rosters a player today and back-fills last
// month's register has written a fact about them, and a record that hides it is wrong.
func TestARecordedFactOutranksTheRosterWindow(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "backfill@e.com")
	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U12"})
	teamID := team.body["id"].(string)
	athlete := createAthlete(t, coach, "Late Signing")

	// A session held well before anyone put them on the roster.
	past := do(t, http.MethodPost, "/api/v1/sessions", coach, map[string]any{
		"title": "Before they joined", "teamId": teamID, "scheduledAt": "2025-01-06T18:00:00Z",
		"blocks": []map[string]any{},
	})
	if past.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", past.status, past.raw)
	}
	// Rostered today, so the window starts today and the session is behind it.
	if r := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coach,
		map[string]any{"personId": athlete}); r.status != http.StatusCreated {
		t.Fatalf("roster: %d %s", r.status, r.raw)
	}

	// Nothing recorded: the session is outside their window and not theirs.
	before := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/attendance", coach, nil)
	if before.status != http.StatusOK {
		t.Fatalf("person attendance: %d %s", before.status, before.raw)
	}
	if events, _ := before.body["events"].(float64); events != 0 {
		t.Errorf("an event before they joined is not theirs to have missed, got %v", events)
	}

	// The coach back-fills it. Now there is evidence, and evidence outranks the window.
	if r := do(t, http.MethodPatch, "/api/v1/sessions/"+past.body["id"].(string)+
		"/attendance/"+athlete, coach, map[string]any{"status": "present"}); r.status != http.StatusOK {
		t.Fatalf("back-fill: %d %s", r.status, r.raw)
	}
	after := do(t, http.MethodGet, "/api/v1/persons/"+athlete+"/attendance", coach, nil)
	if events, _ := after.body["events"].(float64); events != 1 {
		t.Fatalf("a recorded fact must appear in their record, got %v (%s)", events, after.raw)
	}
	overall := after.body["overall"].(map[string]any)
	if overall["present"].(float64) != 1 || overall["rate"].(float64) != 1 {
		t.Errorf("the back-filled attendance should count: %v", overall)
	}
}
