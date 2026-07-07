package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.GetUserByID(r.Context(), userIDFrom(r.Context()))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicUser(user))
}

type updateProfileRequest struct {
	DisplayName *string `json:"displayName"`
	Position    *string `json:"position"`
	SkillLevel  *int32  `json:"skillLevel"`
	Bio         *string `json:"bio"`
	AvatarURL   *string `json:"avatarUrl"`
	// Presence flags let clients distinguish "omit" from "set to null".
	setPosition  bool
	setBio       bool
	setAvatarURL bool
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	// Decode into a raw map first to detect which nullable fields were provided.
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		writeError(w, err)
		return
	}
	var req updateProfileRequest
	if v, ok := raw["displayName"].(string); ok {
		req.DisplayName = &v
	}
	if _, ok := raw["position"]; ok {
		req.setPosition = true
		if v, ok := raw["position"].(string); ok {
			req.Position = &v
		}
	}
	if v, ok := raw["skillLevel"].(float64); ok {
		n := int32(v)
		req.SkillLevel = &n
	}
	if _, ok := raw["bio"]; ok {
		req.setBio = true
		if v, ok := raw["bio"].(string); ok {
			req.Bio = &v
		}
	}
	if _, ok := raw["avatarUrl"]; ok {
		req.setAvatarURL = true
		if v, ok := raw["avatarUrl"].(string); ok {
			req.AvatarURL = &v
		}
	}

	if req.DisplayName != nil && *req.DisplayName == "" {
		writeError(w, errValidation("displayName cannot be empty"))
		return
	}
	if req.setPosition {
		if err := validatePosition(req.Position); err != nil {
			writeError(w, err)
			return
		}
	}
	if req.SkillLevel != nil && (*req.SkillLevel < 1 || *req.SkillLevel > 5) {
		writeError(w, errValidation("skillLevel must be between 1 and 5"))
		return
	}

	user, err := s.store.UpdateUserProfile(r.Context(), store.UpdateUserProfileParams{
		ID:           userIDFrom(r.Context()),
		DisplayName:  req.DisplayName,
		SetPosition:  req.setPosition,
		Position:     req.Position,
		SkillLevel:   req.SkillLevel,
		SetBio:       req.setBio,
		Bio:          req.Bio,
		SetAvatarUrl: req.setAvatarURL,
		AvatarUrl:    req.AvatarURL,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicUser(user))
}

func (s *Server) handleListPlayers(w http.ResponseWriter, r *http.Request) {
	pos := queryStrPtr(r, "position")
	if err := validatePosition(pos); err != nil {
		writeError(w, err)
		return
	}
	var minSkill *int32
	if v := r.URL.Query().Get("minSkill"); v != "" {
		n := queryInt(r, "minSkill", 1, 1, 5)
		minSkill = &n
	}
	limit := queryInt(r, "limit", 20, 1, 100)
	offset := queryInt(r, "offset", 0, 0, 1_000_000)

	params := store.ListPlayersParams{
		Q: queryStrPtr(r, "q"), Position: pos, MinSkill: minSkill, Lim: limit, Off: offset,
	}
	users, err := s.store.ListPlayers(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	total, err := s.store.CountPlayers(r.Context(), store.CountPlayersParams{
		Q: params.Q, Position: params.Position, MinSkill: params.MinSkill,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	items := make([]PublicUser, len(users))
	for i, u := range users {
		items[i] = publicUser(u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (s *Server) handleGetPlayer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	user, err := s.store.GetUserByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("player not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicUser(user))
}

func (s *Server) handlePlayerCareerStats(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetUserByID(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("player not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	agg, err := s.store.CareerStats(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, CareerStats{
		Appearances: agg.Appearances, Goals: agg.Goals, Assists: agg.Assists,
		YellowCards: agg.YellowCards, RedCards: agg.RedCards, MinutesPlayed: agg.MinutesPlayed,
	})
}
