package api

import (
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
	drill, err := s.store.CreateDrill(r.Context(), store.CreateDrillParams{
		OrganizationID: oc.orgID, AuthorPersonID: &personID, Name: req.Name, Description: req.Description,
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
	Title       string  `json:"title"`
	TeamID      *string `json:"teamId"`
	ScheduledAt *string `json:"scheduledAt"`
	Notes       *string `json:"notes"`
	Blocks      []struct {
		Title       string  `json:"title"`
		DrillID     *string `json:"drillId"`
		DurationMin *int32  `json:"durationMin"`
		Notes       *string `json:"notes"`
	} `json:"blocks"`
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

	session, err := q.CreateSession(r.Context(), store.CreateSessionParams{
		OrganizationID: oc.orgID, AuthorPersonID: &personID, TeamID: teamID,
		Title: req.Title, ScheduledAt: scheduled, Notes: req.Notes,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	blocks := make([]SessionBlock, 0, len(req.Blocks))
	for i, b := range req.Blocks {
		created, berr := q.CreateSessionBlock(r.Context(), store.CreateSessionBlockParams{
			SessionID: session.ID, DrillID: blockDrillIDs[i], Title: b.Title,
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
	writeJSON(w, http.StatusCreated, sessionDTO(session, blocks))
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
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
