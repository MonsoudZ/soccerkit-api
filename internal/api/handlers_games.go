package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
	// A fixture nobody was told about is a register nobody fills in. See notifySquad.
	s.notifySquad(r.Context(), team.ID, fixtureNote(
		"New fixture for "+team.Name,
		fixtureName(team.Name, game.Opponent)+" — can you make it?",
		"game", game.ID, team.ID, timePtr(game.KickoffAt),
	))
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
	// The game's team has to still exist. DELETE /teams tombstones rather than dropping
	// (so the deletion reaches sync clients), and games carry no tombstone of their own,
	// so a game stayed readable — and patchable — by id after its team was gone: the
	// team 404s, its game list 404s, and you could still record the result of the match.
	// teamByIDInOrg is the same check every other team-scoped route already makes.
	if _, err := s.teamByIDInOrg(r.Context(), oc, game.TeamID); err != nil {
		return store.Game{}, errNotFound("game not found")
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
//
// PATCH semantics mean absence and null are different, so the body is decoded one key
// at a time. Each key is decoded into its typed target and only then marks itself as
// set: the previous version set the "field was supplied" flag on mere presence and
// assigned the value only if the type matched, so a wrong-typed field wrote NULL and
// skipped its own validation. `{"homeAway": true}` returned 200 and erased the result
// of a match.
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
	raw, err := decodePatch(r, updatableGameFields)
	if err != nil {
		writeError(w, err)
		return
	}

	params := store.UpdateGameParams{ID: game.ID, KickoffAt: nullTimestamptz()}

	if v, ok := raw["opponent"]; ok {
		opponent, err := optionalString(v, "opponent")
		if err != nil {
			writeError(w, err)
			return
		}
		params.SetOpponent, params.Opponent = true, opponent
	}
	if v, ok := raw["kickoffAt"]; ok {
		// Explicit null clears it, the same as every other optional field here — see
		// optionalTimestamptz, which is where this decoding moved once sessions needed it.
		kickoff, verr := optionalTimestamptz(v, "kickoffAt")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetKickoffAt, params.KickoffAt = true, kickoff
	}
	if v, ok := raw["homeAway"]; ok {
		homeAway, err := optionalString(v, "homeAway")
		if err != nil {
			writeError(w, err)
			return
		}
		if homeAway != nil && !validHomeAway[*homeAway] {
			writeError(w, errValidation("homeAway must be home, away or neutral"))
			return
		}
		params.SetHomeAway, params.HomeAway = true, homeAway
	}

	_, hasOur := raw["ourScore"]
	_, hasOpp := raw["opponentScore"]
	if hasOur || hasOpp {
		if !hasOur || !hasOpp {
			writeError(w, errValidation("ourScore and opponentScore must be provided together"))
			return
		}
		ours, err := optionalInt32(raw["ourScore"], "ourScore")
		if err != nil {
			writeError(w, err)
			return
		}
		theirs, err := optionalInt32(raw["opponentScore"], "opponentScore")
		if err != nil {
			writeError(w, err)
			return
		}
		params.SetScores = true
		params.OurScore, params.OpponentScore = ours, theirs
	}

	if v, ok := raw["status"]; ok {
		var status string
		if err := json.Unmarshal(v, &status); err != nil || !validGameStatus[status] {
			writeError(w, errValidation("invalid status"))
			return
		}
		params.Status = &status
	}

	updated, err := s.store.UpdateGame(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	s.notifyIfFixtureChanged(r.Context(), game, updated)
	writeJSON(w, http.StatusOK, gameDTO(updated))
}

// notifyIfFixtureChanged tells the squad when an edit is one they have to act on.
//
// Two of them are: a kickoff that moved, and a fixture that is off. Everything else this
// endpoint writes -- an opponent's name corrected, a scoreline entered at full time -- is
// not, and pushing on those would train a squad to swipe these away, which costs the two
// that matter. The comparison is against the row as it was, which is why gameInOrg's
// result is still in hand here.
func (s *Server) notifyIfFixtureChanged(ctx context.Context, before, after store.Game) {
	cancelled := after.Status == "cancelled" && before.Status != "cancelled"
	moved := !sameInstant(before.KickoffAt, after.KickoffAt)
	if !cancelled && !moved {
		return
	}
	s.notifyTeamByID(ctx, after.TeamID, func(teamName string) Notification {
		name := fixtureName(teamName, after.Opponent)
		// Cancellation wins when both changed. A squad told the time moved for a match
		// that is off would turn up to the new one.
		title, body := "Kickoff changed for "+teamName,
			name+" — the kickoff time has changed. Check you can still make it."
		if cancelled {
			title, body = "Fixture cancelled for "+teamName, name+" is off."
		}
		return fixtureNote(title, body, "game", after.ID, after.TeamID, timePtr(after.KickoffAt))
	})
}

// sameInstant compares two nullable timestamps, where "both unset" counts as unchanged.
// pgtype.Timestamptz is a struct with a location pointer inside it, so == would call a
// kickoff moved because the row came back from Postgres with a different *time.Location
// than the one that was written.
func sameInstant(a, b pgtype.Timestamptz) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Time.Equal(b.Time)
}

// updatableGameFields is the set PATCH /games/{id} accepts.
var updatableGameFields = map[string]bool{
	"opponent": true, "kickoffAt": true, "homeAway": true,
	"ourScore": true, "opponentScore": true, "status": true,
}
