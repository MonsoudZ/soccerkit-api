package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

type createTeamRequest struct {
	Name     string  `json:"name"`
	AgeGroup *string `json:"ageGroup"`
	Season   *string `json:"season"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req createTeamRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" {
		writeError(w, errValidation("name is required"))
		return
	}
	team, err := s.store.CreateTeam(r.Context(), store.CreateTeamParams{
		OrganizationID: oc.orgID, Name: req.Name, AgeGroup: req.AgeGroup, Season: req.Season,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, teamDTO(team, 0))
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListTeamsInOrg(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Team, len(rows))
	for i, t := range rows {
		out[i] = teamDTO(store.Team{
			ID: t.ID, OrganizationID: t.OrganizationID, Name: t.Name, AgeGroup: t.AgeGroup,
			Season: t.Season, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		}, t.ActiveRosterCount)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	roster, err := s.store.ListActiveRoster(r.Context(), team.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	entries := make([]RosterEntry, len(roster))
	for i, row := range roster {
		entries[i] = rosterRowDTO(row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"team":   teamDTO(team, int64(len(entries))),
		"roster": entries,
	})
}

type addRosterRequest struct {
	PersonID     string  `json:"personId"`
	JerseyNumber *int32  `json:"jerseyNumber"`
	Position     *string `json:"position"`
	JoinedOn     *string `json:"joinedOn"`
}

func (s *Server) handleAddRoster(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	var req addRosterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	personID, err := parseUUIDParam(req.PersonID, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetPerson(r.Context(), personID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errBadRequest("personId does not reference an existing person"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	joinedOn, err := parseDate(req.JoinedOn)
	if err != nil {
		writeError(w, err)
		return
	}

	membership, err := s.store.AddRosterMembership(r.Context(), store.AddRosterMembershipParams{
		PersonID: personID, TeamID: team.ID, JerseyNumber: req.JerseyNumber,
		Position: req.Position, JoinedOn: joinedOn,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, errConflict("that person already has an active roster spot on this team"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": membership.ID, "personId": membership.PersonID, "teamId": membership.TeamID,
		"jerseyNumber": membership.JerseyNumber, "position": membership.Position,
		"joinedOn": dateStr(membership.JoinedOn), "status": membership.Status,
	})
}

// handleEndRoster closes a player's active membership (they left the team, or
// are being moved — the caller opens a new membership elsewhere).
func (s *Server) handleEndRoster(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	personID, err := pathUUID(r, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	membership, err := s.store.GetActiveRosterMembership(r.Context(), store.GetActiveRosterMembershipParams{
		PersonID: personID, TeamID: team.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("that person has no active roster spot on this team"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.EndRosterMembership(r.Context(), store.EndRosterMembershipParams{ID: membership.ID}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteTeam(r.Context(), team.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- shared authorization helpers -----------------------------------------

func (s *Server) requireCoach(r *http.Request) (orgContext, error) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		return orgContext{}, err
	}
	if !oc.hasAnyRole("admin", "director", "coach") {
		return orgContext{}, errForbidden("only coaches can do that")
	}
	return oc, nil
}

// teamInOrg loads the team named in the path and verifies it belongs to the
// caller's active organization.
func (s *Server) teamInOrg(r *http.Request, oc orgContext) (store.Team, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return store.Team{}, err
	}
	team, err := s.store.GetTeam(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Team{}, errNotFound("team not found")
	} else if err != nil {
		return store.Team{}, err
	}
	if team.OrganizationID != oc.orgID {
		return store.Team{}, errForbidden("that team is not in your organization")
	}
	return team, nil
}
