package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

var validHomeAway = map[string]bool{"home": true, "away": true, "neutral": true}
var validGameStatus = map[string]bool{
	"scheduled": true, "in_progress": true, "completed": true, "cancelled": true,
}

type createGameRequest struct {
	Opponent  *string `json:"opponent"`
	KickoffAt *string `json:"kickoffAt"`
	HomeAway  *string `json:"homeAway"`
}

func (s *Server) handleCreateGame(w http.ResponseWriter, r *http.Request) {
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
	var req createGameRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.HomeAway != nil && !validHomeAway[*req.HomeAway] {
		writeError(w, errValidation("homeAway must be home, away or neutral"))
		return
	}
	kickoff := nullTimestamptz()
	if req.KickoffAt != nil && *req.KickoffAt != "" {
		t, perr := time.Parse(time.RFC3339, *req.KickoffAt)
		if perr != nil {
			writeError(w, errValidation("kickoffAt must be an RFC3339 timestamp"))
			return
		}
		kickoff = timestamptz(t)
	}
	game, err := s.store.CreateGame(r.Context(), store.CreateGameParams{
		OrganizationID: oc.orgID, TeamID: team.ID, Opponent: req.Opponent,
		KickoffAt: kickoff, HomeAway: req.HomeAway,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, gameDTO(game))
}

func (s *Server) handleListGames(w http.ResponseWriter, r *http.Request) {
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
	games, err := s.store.ListGamesForTeam(r.Context(), team.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Game, len(games))
	for i, g := range games {
		out[i] = gameDTO(g)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) gameInOrg(r *http.Request, oc orgContext) (store.Game, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return store.Game{}, err
	}
	game, err := s.store.GetGame(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Game{}, errNotFound("game not found")
	} else if err != nil {
		return store.Game{}, err
	}
	if game.OrganizationID != oc.orgID {
		return store.Game{}, errForbidden("that game is not in your organization")
	}
	return game, nil
}

func (s *Server) handleGetGame(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	game, err := s.gameInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gameDTO(game))
}

// handleUpdateGame records game-day changes: kickoff, status, and the result.
func (s *Server) handleUpdateGame(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	game, err := s.gameInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		writeError(w, err)
		return
	}

	params := store.UpdateGameParams{ID: game.ID, KickoffAt: nullTimestamptz()}
	if _, ok := raw["opponent"]; ok {
		params.SetOpponent = true
		if v, ok := raw["opponent"].(string); ok {
			params.Opponent = &v
		}
	}
	if v, ok := raw["kickoffAt"].(string); ok {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			writeError(w, errValidation("kickoffAt must be an RFC3339 timestamp"))
			return
		}
		params.KickoffAt = timestamptz(t)
	}
	if _, ok := raw["homeAway"]; ok {
		params.SetHomeAway = true
		if v, ok := raw["homeAway"].(string); ok {
			if !validHomeAway[v] {
				writeError(w, errValidation("homeAway must be home, away or neutral"))
				return
			}
			params.HomeAway = &v
		}
	}
	_, hasOur := raw["ourScore"]
	_, hasOpp := raw["opponentScore"]
	if hasOur || hasOpp {
		if !hasOur || !hasOpp {
			writeError(w, errValidation("ourScore and opponentScore must be provided together"))
			return
		}
		params.SetScores = true
		if v, ok := raw["ourScore"].(float64); ok {
			n := int32(v)
			params.OurScore = &n
		}
		if v, ok := raw["opponentScore"].(float64); ok {
			n := int32(v)
			params.OpponentScore = &n
		}
	}
	if v, ok := raw["status"].(string); ok {
		if !validGameStatus[v] {
			writeError(w, errValidation("invalid status"))
			return
		}
		params.Status = &v
	}

	updated, err := s.store.UpdateGame(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gameDTO(updated))
}
