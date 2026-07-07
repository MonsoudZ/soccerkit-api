package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

type createTeamRequest struct {
	Name     string  `json:"name"`
	CrestURL *string `json:"crestUrl"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	var req createTeamRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" {
		writeError(w, errValidation("name is required"))
		return
	}
	ownerID := userIDFrom(r.Context())

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	team, err := q.CreateTeam(r.Context(), store.CreateTeamParams{
		Name: req.Name, CrestUrl: req.CrestURL, OwnerID: ownerID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := q.AddTeamMember(r.Context(), store.AddTeamMemberParams{
		TeamID: team.ID, UserID: ownerID, Role: "OWNER",
	}); err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, teamDTO(team, 1))
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListTeams(r.Context(), store.ListTeamsParams{
		Q:   queryStrPtr(r, "q"),
		Lim: queryInt(r, "limit", 20, 1, 100),
		Off: queryInt(r, "offset", 0, 0, 1_000_000),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Team, len(rows))
	for i, t := range rows {
		out[i] = teamDTO(store.Team{
			ID: t.ID, Name: t.Name, CrestUrl: t.CrestUrl, OwnerID: t.OwnerID,
			CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		}, t.MemberCount)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.store.GetTeam(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("team not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	memberRows, err := s.store.ListTeamMembers(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	members := make([]TeamMember, len(memberRows))
	for i, m := range memberRows {
		members[i] = teamMemberRowDTO(m)
	}
	writeJSON(w, http.StatusOK, TeamDetail{
		Team: teamDTO(team, int64(len(members))), Members: members,
	})
}

func (s *Server) handleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.requireTeamOwner(r, id)
	if err != nil {
		writeError(w, err)
		return
	}
	_ = team

	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		writeError(w, err)
		return
	}
	params := store.UpdateTeamParams{ID: id}
	if v, ok := raw["name"].(string); ok {
		if v == "" {
			writeError(w, errValidation("name cannot be empty"))
			return
		}
		params.Name = &v
	}
	if _, ok := raw["crestUrl"]; ok {
		params.SetCrestUrl = true
		if v, ok := raw["crestUrl"].(string); ok {
			params.CrestUrl = &v
		}
	}

	updated, err := s.store.UpdateTeam(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	count, err := s.store.CountTeamMembers(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, teamDTO(updated, count))
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.requireTeamOwner(r, id); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteTeam(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type addMemberRequest struct {
	UserID       string  `json:"userId"`
	Role         *string `json:"role"`
	JerseyNumber *int32  `json:"jerseyNumber"`
}

func (s *Server) handleAddTeamMember(w http.ResponseWriter, r *http.Request) {
	teamID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.requireTeamOwner(r, teamID); err != nil {
		writeError(w, err)
		return
	}
	var req addMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	userID, err := parseUUIDParam(req.UserID, "userId")
	if err != nil {
		writeError(w, err)
		return
	}
	role := "PLAYER"
	if req.Role != nil {
		if *req.Role != "CAPTAIN" && *req.Role != "PLAYER" {
			writeError(w, errValidation("role must be CAPTAIN or PLAYER"))
			return
		}
		role = *req.Role
	}

	if _, err := s.store.GetUserByID(r.Context(), userID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errBadRequest("userId does not reference an existing player"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetTeamMember(r.Context(), store.GetTeamMemberParams{TeamID: teamID, UserID: userID}); err == nil {
		writeError(w, errConflict("that player is already on the team"))
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}

	member, err := s.store.AddTeamMember(r.Context(), store.AddTeamMemberParams{
		TeamID: teamID, UserID: userID, Role: role, JerseyNumber: req.JerseyNumber,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	user, err := s.store.GetUserByID(r.Context(), userID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, TeamMember{
		ID: member.ID, Role: member.Role, JerseyNumber: member.JerseyNumber,
		JoinedAt: rfc3339(member.JoinedAt), User: publicUser(user),
	})
}

func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	teamID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.requireTeamOwner(r, teamID)
	if err != nil {
		writeError(w, err)
		return
	}
	userID, err := pathUUID(r, "userId")
	if err != nil {
		writeError(w, err)
		return
	}
	if userID == team.OwnerID {
		writeError(w, errBadRequest("the owner cannot be removed from the team"))
		return
	}
	rows, err := s.store.DeleteTeamMember(r.Context(), store.DeleteTeamMemberParams{TeamID: teamID, UserID: userID})
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == 0 {
		writeError(w, errNotFound("that player is not on the team"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requireTeamOwner loads a team and verifies the caller owns it.
func (s *Server) requireTeamOwner(r *http.Request, teamID uuid.UUID) (store.Team, error) {
	team, err := s.store.GetTeam(r.Context(), teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Team{}, errNotFound("team not found")
	} else if err != nil {
		return store.Team{}, err
	}
	if team.OwnerID != userIDFrom(r.Context()) {
		return store.Team{}, errForbidden("only the team owner can do that")
	}
	return team, nil
}
