package api

import (
	"context"
	"log"
	"net/http"

	"github.com/google/uuid"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// The two vocabularies. They are deliberately different words: an RSVP is a prediction a
// family makes, a status is a fact a coach records, and a single set of values would
// leave "going" meaning both "said they would come" and "came".
var validRSVP = map[string]bool{"going": true, "maybe": true, "not_going": true}
var validAttendanceStatus = map[string]bool{
	"present": true, "absent": true, "late": true, "excused": true,
}

// eventRef is a thing that can be attended, resolved from the path and checked against
// the caller's organization.
//
// Games and training sessions are the two scheduled things in the schema, and attendance
// means the same for both, so the handlers below are written once against this and
// mounted twice. Exactly one of gameID and sessionID is set, matching the column pair the
// row is stored under.
type eventRef struct {
	gameID    *uuid.UUID
	sessionID *uuid.UUID
	teamID    uuid.UUID
	kind      string
	// open reports whether an answer still means anything. A cancelled fixture is the
	// one case where it does not: "going" to a match that will not be played is a
	// coaching signal that is simply wrong, and it would sit in the sheet looking true.
	open bool
}

// eventResolver loads the event named in the path. Router passes the game or the session
// one to each shared handler.
type eventResolver func(*http.Request, orgContext) (eventRef, error)

func (s *Server) gameEvent(r *http.Request, oc orgContext) (eventRef, error) {
	game, err := s.gameInOrg(r, oc)
	if err != nil {
		return eventRef{}, err
	}
	return eventRef{
		gameID: &game.ID, teamID: game.TeamID, kind: "game",
		open: game.Status != "cancelled",
	}, nil
}

// sessionEvent is the session half, and it does not apply the staff-only gate the rest of
// /sessions carries. That gate is about the coaching library -- the drills and the plan --
// which is not what this is: a player has to be able to say whether they are coming to
// training, and being told the club has a training session they are rostered for is not a
// disclosure of how it will be run.
func (s *Server) sessionEvent(r *http.Request, oc orgContext) (eventRef, error) {
	session, err := s.sessionInOrg(r, oc)
	if err != nil {
		return eventRef{}, err
	}
	// teamId is optional on a session, and a session with no team has no roster to ask.
	// 409 rather than 404: the session is real and the caller found it, but the thing
	// they asked for cannot exist until the session is attached to a team.
	if session.TeamID == nil {
		return eventRef{}, errConflict(
			"that session is not attached to a team, so it has no roster to take attendance for")
	}
	return eventRef{
		sessionID: &session.ID, teamID: *session.TeamID, kind: "session", open: true,
	}, nil
}

// --- reading the sheet ------------------------------------------------------

// attendanceSheet answers "who is coming, and who came" for one event.
//
// Staff get the squad. Everyone else gets the lines they are entitled to -- themselves,
// and the children they are a recorded guardian of -- for the reason personVisibleTo
// exists: the sheet names people, and handing a parent the whole list is the disclosure
// that check refuses one id at a time.
//
// The counts are the squad's either way, not the caller's slice of them. That follows
// GET /teams/{id}, where the roster count is whole while the entries are narrowed: how
// many of a squad replied is not a disclosure about any one of them, and a parent shown
// "1 going" for a team of fifteen would be reading something false rather than something
// private.
func (s *Server) attendanceSheet(resolve eventResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		oc, err := s.resolveOrg(r)
		if err != nil {
			writeError(w, err)
			return
		}
		ev, err := resolve(r, oc)
		if err != nil {
			writeError(w, err)
			return
		}
		rows, err := s.store.ListAttendanceForEvent(r.Context(), store.ListAttendanceForEventParams{
			TeamID: ev.teamID, GameID: ev.gameID, SessionID: ev.sessionID, AllPeople: true,
		})
		if err != nil {
			writeError(w, err)
			return
		}

		entries := make([]AttendanceEntry, 0, len(rows))
		if oc.isStaff() {
			for _, row := range rows {
				entries = append(entries, attendanceEntryDTO(row))
			}
		} else {
			mine, merr := s.ownPeople(r.Context(), oc)
			if merr != nil {
				writeError(w, merr)
				return
			}
			for _, row := range rows {
				if mine[row.PersonID] {
					entries = append(entries, attendanceEntryDTO(row))
				}
			}
			// Nobody the caller is entitled to see is at this event, which means they are
			// not connected to the team. An empty sheet plus a live set of counts would
			// otherwise tell an unconnected member how big somebody else's squad is and
			// how it is replying.
			if len(entries) == 0 {
				writeError(w, errForbidden("you are not connected to that team"))
				return
			}
		}
		writeJSON(w, http.StatusOK, AttendanceSheet{
			EventType: ev.kind, EventID: ev.eventID(), TeamID: ev.teamID,
			Counts: countAttendance(rows), Entries: entries,
		})
	}
}

// --- answering (the player's half) ------------------------------------------

type rsvpRequest struct {
	// PersonID names who is replying. Absent means the caller, which is the ordinary
	// case; a parent sends their child's id, and a coach sends the id of a player who
	// phoned in.
	PersonID *string `json:"personId"`
	Status   string  `json:"status"`
	Note     *string `json:"note"`
}

// rsvp records whether someone is coming.
//
// PUT rather than POST: an RSVP is one answer per person per event that replaces
// whatever was there, and a player changing their mind twice on the bus should not
// create three rows. There is no way to un-answer -- "not_going" is what a retraction
// means, and a fourth value meaning "I have unsaid it" would be indistinguishable from
// never having replied.
func (s *Server) rsvp(resolve eventResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		oc, err := s.resolveOrg(r)
		if err != nil {
			writeError(w, err)
			return
		}
		ev, err := resolve(r, oc)
		if err != nil {
			writeError(w, err)
			return
		}
		var req rsvpRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, err)
			return
		}
		if !validRSVP[req.Status] {
			writeError(w, errValidation("status must be going, maybe or not_going"))
			return
		}
		if !ev.open {
			writeError(w, errConflict("that game was cancelled, so there is nothing to reply to"))
			return
		}
		subject, err := s.rsvpSubject(r, oc, req.PersonID)
		if err != nil {
			writeError(w, err)
			return
		}
		// The gate and the response in one read: a person with no line on this sheet is
		// neither on the team's roster nor already recorded at the event, and opening a
		// row for them would be the "any Person id in the database" hazard the roster
		// endpoint guards against, one table over.
		if _, err := s.attendanceLine(r.Context(), ev, subject); err != nil {
			writeError(w, err)
			return
		}

		if err := s.store.EnsureAttendance(r.Context(), store.EnsureAttendanceParams{
			GameID: ev.gameID, SessionID: ev.sessionID, PersonID: subject,
		}); err != nil {
			writeError(w, err)
			return
		}
		caller := oc.callerID
		if _, err := s.store.SetAttendanceRSVP(r.Context(), store.SetAttendanceRSVPParams{
			GameID: ev.gameID, SessionID: ev.sessionID, PersonID: subject,
			Rsvp: req.Status, RsvpNote: req.Note, RsvpByPersonID: &caller,
		}); err != nil {
			writeError(w, err)
			return
		}
		line, err := s.attendanceLine(r.Context(), ev, subject)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, attendanceEntryDTO(line))
	}
}

// rsvpSubject decides who this reply is about, and whether the caller may make it.
//
// Yourself, always. A parent, for a child they are a recorded guardian of. Staff, for
// anyone on the team -- which is not a loophole but the common case in youth football,
// where the player is nine and the reply arrived by text message to the coach.
func (s *Server) rsvpSubject(r *http.Request, oc orgContext, raw *string) (uuid.UUID, error) {
	caller := oc.callerID
	if raw == nil || *raw == "" {
		return caller, nil
	}
	subject, err := parseUUIDParam(*raw, "personId")
	if err != nil {
		return uuid.Nil, err
	}
	if subject == caller || oc.isStaff() {
		return subject, nil
	}
	if oc.roles[roleParent] {
		isGuardian, gerr := s.store.IsGuardianOf(r.Context(), store.IsGuardianOfParams{
			GuardianPersonID: caller, ChildPersonID: subject,
		})
		if gerr != nil {
			return uuid.Nil, gerr
		}
		if isGuardian {
			return subject, nil
		}
	}
	return uuid.Nil, errForbidden("you can only reply for yourself or for your own children")
}

// --- recording (the coach's half) -------------------------------------------

// recordableFields is the set PATCH .../attendance/{personId} accepts.
var recordableFields = map[string]bool{"status": true, "note": true}

// recordAttendance writes what actually happened, which is staff's alone. An RSVP is a
// family's statement about itself; this is the club's statement about a child, and it is
// what a coach will later read back as a record.
//
// Decoded a key at a time, because absence and null are different requests here. Explicit
// null clears a line ticked by mistake -- nothing in the vocabulary means "not recorded",
// so a status that could not be removed would leave the register saying something untrue
// -- while an absent key leaves that half alone, so adding a note does not erase the
// status it annotates. A typed struct cannot tell those apart; see patch.go.
func (s *Server) recordAttendance(resolve eventResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		oc, err := s.requireCoach(r)
		if err != nil {
			writeError(w, err)
			return
		}
		ev, err := resolve(r, oc)
		if err != nil {
			writeError(w, err)
			return
		}
		subject, err := pathUUID(r, "personId")
		if err != nil {
			writeError(w, err)
			return
		}
		raw, err := decodePatch(r, recordableFields)
		if err != nil {
			writeError(w, err)
			return
		}
		params := store.SetAttendanceStatusParams{
			GameID: ev.gameID, SessionID: ev.sessionID, PersonID: subject,
		}
		if v, ok := raw["status"]; ok {
			status, verr := optionalString(v, "status")
			if verr != nil {
				writeError(w, verr)
				return
			}
			if status != nil && !validAttendanceStatus[*status] {
				writeError(w, errValidation("status must be present, absent, late, excused or null"))
				return
			}
			params.SetStatus, params.Status = true, status
		}
		if v, ok := raw["note"]; ok {
			note, verr := optionalString(v, "note")
			if verr != nil {
				writeError(w, verr)
				return
			}
			params.SetStatusNote, params.StatusNote = true, note
		}
		if _, err := s.attendanceLine(r.Context(), ev, subject); err != nil {
			writeError(w, err)
			return
		}

		if err := s.store.EnsureAttendance(r.Context(), store.EnsureAttendanceParams{
			GameID: ev.gameID, SessionID: ev.sessionID, PersonID: subject,
		}); err != nil {
			writeError(w, err)
			return
		}
		recorder := oc.callerID
		params.RecordedByPersonID = &recorder
		if _, err := s.store.SetAttendanceStatus(r.Context(), params); err != nil {
			writeError(w, err)
			return
		}
		line, err := s.attendanceLine(r.Context(), ev, subject)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, attendanceEntryDTO(line))
	}
}

// --- being asked ------------------------------------------------------------

// notifySquad tells a team's players and their parents that there is something to answer.
//
// This is what makes the register work rather than exist. A coach schedules a fixture, a
// sheet full of "has not replied" appears, and until somebody is told there is nothing to
// reply to, it stays that way and the coach goes back to the group chat -- which is the
// problem the table was added to solve.
//
// Called after the write and outside any transaction, for the reason
// handleCreateInvitation gives: a push about a fixture that failed to save is worse than
// no push, and the send must never be able to fail a request that has already succeeded.
// Notify queues and returns, so nothing here waits for Apple.
func (s *Server) notifySquad(ctx context.Context, teamID uuid.UUID, note Notification) {
	// With no notifier installed -- which is what an unset APNS_* configuration means --
	// there is nobody to tell, and the recipient lookup is a query per fixture written
	// for an answer nothing would read.
	if s.notifier == nil {
		return
	}
	people, err := s.store.ListReachablePeopleForTeam(ctx, store.ListReachablePeopleForTeamParams{
		TeamID: teamID, ActorPersonID: personIDFrom(ctx),
	})
	if err != nil {
		// Logged and dropped. The fixture is written and readable; who was told about it
		// is a convenience, and failing the request now would report a notification
		// problem as a scheduling one.
		log.Printf("attendance: looking up who to notify for team %s: %v", teamID, err)
		return
	}
	for _, personID := range people {
		s.notify(ctx, personID, note)
	}
}

// notifyTeamByID is notifySquad for a caller holding a team's id but not its name, which
// every message here needs. The build function runs only once there is a name and a
// notifier, so an unconfigured push costs neither the lookup nor the message.
func (s *Server) notifyTeamByID(ctx context.Context, teamID uuid.UUID, build func(teamName string) Notification) {
	if s.notifier == nil {
		return
	}
	team, err := s.store.GetTeam(ctx, teamID)
	if err != nil {
		log.Printf("attendance: naming team %s for a notification: %v", teamID, err)
		return
	}
	s.notifySquad(ctx, teamID, build(team.Name))
}

// fixtureNote builds one of these messages, and is where the same decision gets made for
// all of them: no time in the text.
//
// The server knows a kickoff as an instant and does not know which zone the club reads it
// in -- there is no timezone on an organization, a team or a game. Rendering one here
// would print the server's UTC, which shows a Saturday evening match as Sunday to anyone
// far enough east, and a push that names the wrong day is worse than one that names none.
// The instant rides in the payload instead, where the app formats it in the device's own
// zone, and the screen the tap opens shows it correctly.
func fixtureNote(title, body, eventType string, eventID, teamID uuid.UUID, kickoff *string) Notification {
	data := map[string]string{
		"type":   eventType,
		"teamId": teamID.String(),
		// One key for the event's id whichever kind it is, so a client handling a tap has
		// one thing to read rather than a branch. `type` says what it points at.
		"eventId": eventID.String(),
		"action":  "rsvp",
	}
	if kickoff != nil {
		data["startsAt"] = *kickoff
	}
	return Notification{Title: title, Body: body, Data: data}
}

// fixtureName is how a fixture reads on a lock screen: the team, and who they are playing
// when that is known. A game against nobody in particular is a real row -- opponent is
// nullable and a coach often schedules before the draw -- so it has to read as a sentence
// either way.
func fixtureName(teamName string, opponent *string) string {
	if opponent != nil && *opponent != "" {
		return teamName + " vs " + *opponent
	}
	return teamName
}

// --- shared ------------------------------------------------------------------

// eventID is whichever of the two keys is set.
func (e eventRef) eventID() uuid.UUID {
	if e.gameID != nil {
		return *e.gameID
	}
	if e.sessionID != nil {
		return *e.sessionID
	}
	return uuid.Nil
}

// attendanceLine reads one person's line off the sheet, and is the authorization check
// both writes make: the query returns a row only for someone on the team's active roster
// or already recorded at this event, so a 404 here is "that person has nothing to do with
// this fixture" and nothing gets opened for them.
func (s *Server) attendanceLine(
	ctx context.Context, ev eventRef, personID uuid.UUID,
) (store.ListAttendanceForEventRow, error) {
	rows, err := s.store.ListAttendanceForEvent(ctx, store.ListAttendanceForEventParams{
		TeamID: ev.teamID, GameID: ev.gameID, SessionID: ev.sessionID,
		AllPeople: false, PersonIds: []uuid.UUID{personID},
	})
	if err != nil {
		return store.ListAttendanceForEventRow{}, err
	}
	if len(rows) == 0 {
		return store.ListAttendanceForEventRow{},
			errNotFound("that person is not on this team's roster")
	}
	return rows[0], nil
}

// ownPeople is the set of people a non-staff caller may be shown: themselves, plus the
// children they are a recorded guardian of. Loaded once rather than asked per row, the
// same trade visibleRoster makes for the same reason.
func (s *Server) ownPeople(ctx context.Context, oc orgContext) (map[uuid.UUID]bool, error) {
	mine := map[uuid.UUID]bool{oc.callerID: true}
	if !oc.roles[roleParent] {
		return mine, nil
	}
	children, err := s.store.ListChildren(ctx, oc.callerID)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		mine[child.ID] = true
	}
	return mine, nil
}

// countAttendance tallies the whole sheet. Both halves count a "neither" bucket, because
// the difference between a squad that has not been asked and one that answered no is the
// entire point of looking.
func countAttendance(rows []store.ListAttendanceForEventRow) AttendanceCounts {
	var c AttendanceCounts
	for _, row := range rows {
		switch {
		case row.Rsvp == nil:
			c.NoReply++
		case *row.Rsvp == "going":
			c.Going++
		case *row.Rsvp == "maybe":
			c.Maybe++
		case *row.Rsvp == "not_going":
			c.NotGoing++
		}
		switch {
		case row.Status == nil:
			c.NotRecorded++
		case *row.Status == "present":
			c.Present++
		case *row.Status == "absent":
			c.Absent++
		case *row.Status == "late":
			c.Late++
		case *row.Status == "excused":
			c.Excused++
		}
	}
	return c
}
