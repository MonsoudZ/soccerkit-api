package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// --- drills ---------------------------------------------------------------

type createDrillRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

func (s *Server) handleCreateDrill(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req createDrillRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" {
		writeError(w, errValidation("name is required"))
		return
	}
	personID := personIDFrom(r.Context())
	drillID := uuid.New()
	drill, err := s.store.CreateDrill(r.Context(), store.CreateDrillParams{
		ID: drillID, OrganizationID: oc.orgID, AuthorPersonID: &personID,
		SyncAccountID: &personID, Name: req.Name, Description: req.Description,
		Payload: newDrillPayload(drillID, req.Name, req.Description),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, drillDTO(drill))
}

func (s *Server) handleListDrills(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.isStaff() {
		writeError(w, errForbidden("only staff can see the coaching library"))
		return
	}
	drills, err := s.store.ListDrillsInOrg(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Drill, len(drills))
	for i, d := range drills {
		out[i] = drillDTO(d)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- sessions -------------------------------------------------------------

type createSessionRequest struct {
	Title       string                `json:"title"`
	TeamID      *string               `json:"teamId"`
	ScheduledAt *string               `json:"scheduledAt"`
	Notes       *string               `json:"notes"`
	Blocks      []sessionBlockRequest `json:"blocks"`
}

// sessionBlockRequest is one entry in a session's plan. Named rather than anonymous now
// that the payload builder takes it too.
type sessionBlockRequest struct {
	Title       string  `json:"title"`
	DrillID     *string `json:"drillId"`
	DurationMin *int32  `json:"durationMin"`
	Notes       *string `json:"notes"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req createSessionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Title == "" {
		writeError(w, errValidation("title is required"))
		return
	}
	teamID, err := s.optionalTeamInOrg(r, oc, req.TeamID)
	if err != nil {
		writeError(w, err)
		return
	}
	scheduled := nullTimestamptz()
	if req.ScheduledAt != nil && *req.ScheduledAt != "" {
		t, perr := time.Parse(time.RFC3339, *req.ScheduledAt)
		if perr != nil {
			writeError(w, errValidation("scheduledAt must be an RFC3339 timestamp"))
			return
		}
		scheduled = timestamptz(t)
	}
	// Parse each block's drill id once, here, and carry the result to the insert below.
	// That insert used to re-parse the same string and drop the error, which was only
	// safe because of this loop thirty lines earlier — a correctness argument that had
	// to be reconstructed from two places to be checked.
	blockDrillIDs := make([]*uuid.UUID, len(req.Blocks))
	seen := make(map[uuid.UUID]bool, len(req.Blocks))
	var referenced []uuid.UUID
	for i, b := range req.Blocks {
		if b.DrillID == nil {
			continue
		}
		id, perr := parseUUIDParam(*b.DrillID, "drillId")
		if perr != nil {
			writeError(w, perr)
			return
		}
		blockDrillIDs[i] = &id
		if !seen[id] {
			seen[id] = true
			referenced = append(referenced, id)
		}
	}
	// Every referenced drill has to belong to the org, asked in one round trip rather
	// than one GetDrill per block: a ten-block session was ten queries before the
	// transaction even opened, and the answer is the same for all of them.
	if len(referenced) > 0 {
		found, cerr := s.store.CountDrillsInOrgByIDs(r.Context(), store.CountDrillsInOrgByIDsParams{
			Ids: referenced, OrganizationID: oc.orgID,
		})
		if cerr != nil {
			writeError(w, cerr)
			return
		}
		if int(found) != len(referenced) {
			writeError(w, errBadRequest("drillId does not reference a drill in your organization"))
			return
		}
	}

	personID := personIDFrom(r.Context())
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	// Ids are minted here rather than defaulted by the column, because the session's
	// payload has to carry its own id and every block's, and it is written in the same
	// statement as the session.
	sessionID := uuid.New()
	blockIDs := make([]uuid.UUID, len(req.Blocks))
	for i := range blockIDs {
		blockIDs[i] = uuid.New()
	}
	// The app requires a date and the server always has one: the scheduled time when it
	// was given, the creation time when it was not.
	sessionDate := time.Now()
	if scheduled.Valid {
		sessionDate = scheduled.Time
	}

	session, err := q.CreateSession(r.Context(), store.CreateSessionParams{
		ID: sessionID, OrganizationID: oc.orgID, AuthorPersonID: &personID,
		SyncAccountID: &personID, TeamID: teamID,
		Title: req.Title, ScheduledAt: scheduled, Notes: req.Notes,
		Payload: newSessionPayload(sessionID, teamID, req.Title, sessionDate, req.Notes,
			blockIDs, blockDrillIDs, req.Blocks),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	blocks := make([]SessionBlock, 0, len(req.Blocks))
	for i, b := range req.Blocks {
		created, berr := q.CreateSessionBlock(r.Context(), store.CreateSessionBlockParams{
			ID: blockIDs[i], SessionID: session.ID, DrillID: blockDrillIDs[i], Title: b.Title,
			DurationMin: b.DurationMin, Position: int32(i), Notes: b.Notes,
		})
		if berr != nil {
			writeError(w, berr)
			return
		}
		blocks = append(blocks, SessionBlock{
			ID: created.ID, Title: created.Title, DrillID: created.DrillID,
			DurationMin: created.DurationMin, Position: created.Position, Notes: created.Notes,
		})
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	// Training a squad is expected at is the same ask as a fixture, so it gets the same
	// push. Only when the session has a team: without one there is no roster to tell, and
	// a plan a coach is drafting for themselves is not an event anybody attends.
	if teamID != nil {
		s.notifyTeamByID(r.Context(), *teamID, func(teamName string) Notification {
			return fixtureNote(
				"New training for "+teamName,
				session.Title+" — can you make it?",
				"session", session.ID, *teamID, timePtr(session.ScheduledAt),
			)
		})
	}
	writeJSON(w, http.StatusCreated, sessionDTO(session, blocks))
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.isStaff() {
		writeError(w, errForbidden("only staff can see the coaching library"))
		return
	}
	var teamFilter *uuid.UUID
	if v := r.URL.Query().Get("teamId"); v != "" {
		id, perr := parseUUIDParam(v, "teamId")
		if perr != nil {
			writeError(w, perr)
			return
		}
		teamFilter = &id
	}
	sessions, err := s.store.ListSessionsInOrg(r.Context(), store.ListSessionsInOrgParams{
		OrganizationID: oc.orgID, TeamID: teamFilter,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Session, len(sessions))
	for i, sess := range sessions {
		out[i] = sessionDTO(sess, nil)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.isStaff() {
		writeError(w, errForbidden("only staff can see the coaching library"))
		return
	}
	session, err := s.sessionInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	blockRows, err := s.store.ListSessionBlocks(r.Context(), session.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	blocks := make([]SessionBlock, len(blockRows))
	for i, b := range blockRows {
		blocks[i] = sessionBlockRowDTO(b)
	}
	writeJSON(w, http.StatusOK, sessionDTO(session, blocks))
}

// updatableSessionFields is the set PATCH /sessions/{id} accepts. Blocks are not in it:
// see UpdateSession in db/queries/content.sql for why editing the plan is a different
// operation from editing the session.
var updatableSessionFields = map[string]bool{
	"title": true, "teamId": true, "scheduledAt": true, "notes": true,
}

// handleUpdateSession moves, renames and re-notes a training session.
//
// Sessions were the one scheduled thing that could be created and deleted but never
// edited, so moving Tuesday training to Thursday meant deleting it and building it again
// — and since a session now carries a register, that threw away everyone's reply along
// with it. It is also why a moved kickoff pushed to the squad and moved training did not:
// there was no endpoint for training to move through.
//
// Every field here is in the sync contract, so each is written twice — once into the
// column this API reads, once into the payload a pull returns. See UpdateSession.
func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	session, err := s.sessionInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := decodePatch(r, updatableSessionFields)
	if err != nil {
		writeError(w, err)
		return
	}

	params := store.UpdateSessionParams{ID: session.ID}
	patch := syncPatch{}
	if v, ok := raw["title"]; ok {
		title, verr := requiredString(v, "title")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.Title = &title
		patch.set("title", title)
	}
	if v, ok := raw["notes"]; ok {
		notes, verr := optionalString(v, "notes")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetNotes, params.Notes = true, notes
		// The app calls it the objective; this API calls it notes. Same field, and
		// newSessionPayload already maps it that way on the create path.
		patch.set("objective", syncString(notes))
	}
	if v, ok := raw["scheduledAt"]; ok {
		when, verr := optionalTimestamptz(v, "scheduledAt")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetScheduledAt, params.ScheduledAt = true, when
		// The app's record requires a date and loses the whole session without one, so a
		// cleared schedule falls back to when the session was created — the same answer
		// newSessionPayload gives a session created without a time. The column is still
		// NULL; it is only the payload that cannot be empty here.
		date := session.CreatedAt.Time
		if when.Valid {
			date = when.Time
		}
		patch.set("date", swiftDate(date))
	}
	if v, ok := raw["teamId"]; ok {
		rawTeam, verr := optionalString(v, "teamId")
		if verr != nil {
			writeError(w, verr)
			return
		}
		teamID, terr := s.optionalTeamInOrg(r, oc, rawTeam)
		if terr != nil {
			writeError(w, terr)
			return
		}
		if !sameTeam(session.TeamID, teamID) {
			// Moving a session whose register has been started would carry one squad's
			// replies onto another squad's training, attributing them to people who were
			// never asked. Before anyone has answered there is nothing to carry, which is
			// when a mistyped team is worth being able to fix.
			answered, cerr := s.store.CountAnsweredAttendanceForSession(r.Context(), &session.ID)
			if cerr != nil {
				writeError(w, cerr)
				return
			}
			if answered > 0 {
				writeError(w, errConflict(
					"that session already has a register; create a new session for the other team"))
				return
			}
		}
		params.SetTeamID, params.TeamID = true, teamID
		// Written as null when cleared rather than left out. `||` merges keys and cannot
		// remove one, so an omitted key would leave the old team id in the payload and
		// put the session on a team the server says it is not on. teamID is optional in
		// the app's record, which is what makes null safe to decode here.
		if teamID != nil {
			patch.set("teamID", teamID.String())
		} else {
			patch.set("teamID", nil)
		}
	}
	params.PayloadPatch, params.PatchPayload = patch.marshal()

	updated, err := s.store.UpdateSession(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	// Training that moved is the squad's business, exactly as a kickoff that moved is.
	// Only the time: a renamed session is not something anybody has to act on.
	if !sameInstant(session.ScheduledAt, updated.ScheduledAt) && updated.TeamID != nil {
		teamID := *updated.TeamID
		s.notifyTeamByID(r.Context(), teamID, func(teamName string) Notification {
			return fixtureNote(
				"Training moved for "+teamName,
				updated.Title+" — the time has changed. Check you can still make it.",
				"session", updated.ID, teamID, timePtr(updated.ScheduledAt),
			)
		})
	}

	blockRows, err := s.store.ListSessionBlocks(r.Context(), updated.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	blocks := make([]SessionBlock, len(blockRows))
	for i, b := range blockRows {
		blocks[i] = sessionBlockRowDTO(b)
	}
	writeJSON(w, http.StatusOK, sessionDTO(updated, blocks))
}

// sameTeam compares two optional team ids, where "both unset" counts as unchanged.
func sameTeam(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	session, err := s.sessionInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteSession(r.Context(), session.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// sessionInOrg loads the session named in the path and verifies it belongs to the
// caller's active organization. Sessions were the one resource without such a helper,
// so both routes that read one made the load / 404 / org-compare by hand; teams have
// teamInOrg, games gameInOrg and templates templateFor.
func (s *Server) sessionInOrg(r *http.Request, oc orgContext) (store.Session, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return store.Session{}, err
	}
	session, err := s.store.GetSession(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Session{}, errNotFound("session not found")
	} else if err != nil {
		return store.Session{}, err
	}
	if session.OrganizationID != oc.orgID {
		return store.Session{}, errForbidden("that session is not in your organization")
	}
	return session, nil
}

// optionalTeamInOrg parses an optional team id and verifies org ownership.
func (s *Server) optionalTeamInOrg(r *http.Request, oc orgContext, raw *string) (*uuid.UUID, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	id, err := parseUUIDParam(*raw, "teamId")
	if err != nil {
		return nil, err
	}
	team, err := s.store.GetTeam(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && team.OrganizationID != oc.orgID) {
		return nil, errBadRequest("teamId does not reference a team in your organization")
	} else if err != nil {
		return nil, err
	}
	return &id, nil
}

// swiftReferenceDate is 2001-01-01 UTC, the epoch Swift's Date measures from. Its
// JSONEncoder writes a Date as seconds since that instant, as a Double, which is what
// the app's decoder expects and what TestContractSwiftDatesSurviveAsNumbers pins.
var swiftReferenceDate = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

// swiftDate renders a time the way the app's records spell one.
func swiftDate(t time.Time) float64 { return t.Sub(swiftReferenceDate).Seconds() }

// newDrillPayload builds the record the app will decode for a drill created over REST.
//
// Three keys, because POST /drills collects three things. The app's decoder used to
// require a category, a duration and coaching points as well, and Codable loses the
// whole record over one missing key — so a drill made here was not merely incomplete on
// the phone, it never appeared. Those fields are optional there now, and left out here
// rather than invented: a category nobody chose is worse in a coach's library than a
// blank they can fill in.
func newDrillPayload(id uuid.UUID, title string, fieldSetup *string) []byte {
	payload, err := json.Marshal(map[string]any{
		"id":         id.String(),
		"title":      title,
		"fieldSetup": syncString(fieldSetup),
	})
	if err != nil {
		return nil
	}
	return payload
}

// newSessionPayload builds the record the app will decode for a session created over
// REST, including its blocks.
//
// teamID and a block's drillID are written only when present. Both were required by the
// app until this change and both are optional here, which is exactly why a session
// without a team — or with a warm-up block that runs no drill — used to be created
// successfully and then never arrive.
func newSessionPayload(
	id uuid.UUID, teamID *uuid.UUID, title string, date time.Time, objective *string,
	blockIDs []uuid.UUID, blockDrillIDs []*uuid.UUID, blocks []sessionBlockRequest,
) []byte {
	wire := make([]map[string]any, 0, len(blocks))
	for i, b := range blocks {
		block := map[string]any{
			"id":      blockIDs[i].String(),
			"minutes": int32Value(b.DurationMin),
			// The app calls it focus; this API calls it a block title. Same field: the
			// short label a coach reads down the session plan.
			"focus": b.Title,
		}
		if drillID := blockDrillIDs[i]; drillID != nil {
			block["drillID"] = drillID.String()
		}
		wire = append(wire, block)
	}
	record := map[string]any{
		"id":        id.String(),
		"title":     title,
		"date":      swiftDate(date),
		"objective": syncString(objective),
		"blocks":    wire,
	}
	if teamID != nil {
		record["teamID"] = teamID.String()
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil
	}
	return payload
}

// int32Value is zero for an absent duration, which is what the app defaults to.
func int32Value(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}
