package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

type recordStatRequest struct {
	UserID        string `json:"userId"`
	Goals         *int32 `json:"goals"`
	Assists       *int32 `json:"assists"`
	YellowCards   *int32 `json:"yellowCards"`
	RedCards      *int32 `json:"redCards"`
	MinutesPlayed *int32 `json:"minutesPlayed"`
}

func val(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func (r recordStatRequest) validate() error {
	if val(r.Goals) < 0 || val(r.Assists) < 0 || val(r.YellowCards) < 0 ||
		val(r.RedCards) < 0 || val(r.MinutesPlayed) < 0 {
		return errValidation("stat values must be non-negative")
	}
	return nil
}

func (s *Server) handleRecordMatchStat(w http.ResponseWriter, r *http.Request) {
	matchID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req recordStatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, err)
		return
	}
	userID, err := parseUUIDParam(req.UserID, "userId")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetMatch(r.Context(), matchID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("match not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if err := s.ensureUser(r, userID); err != nil {
		writeError(w, err)
		return
	}

	stat, err := s.store.UpsertMatchStat(r.Context(), store.UpsertMatchStatParams{
		UserID: userID, MatchID: &matchID, Goals: val(req.Goals), Assists: val(req.Assists),
		YellowCards: val(req.YellowCards), RedCards: val(req.RedCards), MinutesPlayed: val(req.MinutesPlayed),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, playerStatDTO(stat, nil))
}

func (s *Server) handleRecordFixtureStat(w http.ResponseWriter, r *http.Request) {
	fixtureID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req recordStatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, err)
		return
	}
	userID, err := parseUUIDParam(req.UserID, "userId")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetFixture(r.Context(), fixtureID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("fixture not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if err := s.ensureUser(r, userID); err != nil {
		writeError(w, err)
		return
	}

	stat, err := s.store.UpsertFixtureStat(r.Context(), store.UpsertFixtureStatParams{
		UserID: userID, FixtureID: &fixtureID, Goals: val(req.Goals), Assists: val(req.Assists),
		YellowCards: val(req.YellowCards), RedCards: val(req.RedCards), MinutesPlayed: val(req.MinutesPlayed),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, playerStatDTO(stat, nil))
}

func (s *Server) handleListMatchStats(w http.ResponseWriter, r *http.Request) {
	matchID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListMatchStats(r.Context(), &matchID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]PlayerStat, len(rows))
	for i, row := range rows {
		user := PublicUser{
			ID: row.UserID, Email: row.Email, DisplayName: row.DisplayName,
			Position: row.Position, SkillLevel: row.SkillLevel, Bio: row.Bio,
			AvatarURL: row.AvatarUrl, CreatedAt: rfc3339(row.UserCreatedAt),
		}
		out[i] = PlayerStat{
			ID: row.ID, UserID: row.UserID, MatchID: row.MatchID, FixtureID: row.FixtureID,
			Goals: row.Goals, Assists: row.Assists, YellowCards: row.YellowCards,
			RedCards: row.RedCards, MinutesPlayed: row.MinutesPlayed, User: &user,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListFixtureStats(w http.ResponseWriter, r *http.Request) {
	fixtureID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListFixtureStats(r.Context(), &fixtureID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]PlayerStat, len(rows))
	for i, row := range rows {
		user := PublicUser{
			ID: row.UserID, Email: row.Email, DisplayName: row.DisplayName,
			Position: row.Position, SkillLevel: row.SkillLevel, Bio: row.Bio,
			AvatarURL: row.AvatarUrl, CreatedAt: rfc3339(row.UserCreatedAt),
		}
		out[i] = PlayerStat{
			ID: row.ID, UserID: row.UserID, MatchID: row.MatchID, FixtureID: row.FixtureID,
			Goals: row.Goals, Assists: row.Assists, YellowCards: row.YellowCards,
			RedCards: row.RedCards, MinutesPlayed: row.MinutesPlayed, User: &user,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ensureUser(r *http.Request, userID uuid.UUID) error {
	if _, err := s.store.GetUserByID(r.Context(), userID); errors.Is(err, pgx.ErrNoRows) {
		return errBadRequest("userId does not reference an existing player")
	} else if err != nil {
		return err
	}
	return nil
}
