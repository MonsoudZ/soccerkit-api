package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

type createMatchRequest struct {
	Title       string  `json:"title"`
	Description *string `json:"description"`
	Format      string  `json:"format"`
	MaxPlayers  int32   `json:"maxPlayers"`
	KickoffAt   string  `json:"kickoffAt"`
	DurationMin *int32  `json:"durationMin"`
	VenueID     *string `json:"venueId"`
}

func (s *Server) handleCreateMatch(w http.ResponseWriter, r *http.Request) {
	var req createMatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Title == "" || req.Format == "" {
		writeError(w, errValidation("title and format are required"))
		return
	}
	if req.MaxPlayers < 2 || req.MaxPlayers > 40 {
		writeError(w, errValidation("maxPlayers must be between 2 and 40"))
		return
	}
	kickoff, err := time.Parse(time.RFC3339, req.KickoffAt)
	if err != nil {
		writeError(w, errValidation("kickoffAt must be an RFC3339 timestamp"))
		return
	}
	duration := int32(60)
	if req.DurationMin != nil {
		duration = *req.DurationMin
	}

	ctx := r.Context()
	venueID, err := s.resolveVenueID(ctx, req.VenueID)
	if err != nil {
		writeError(w, err)
		return
	}
	hostID := userIDFrom(ctx)

	// Create match and auto-RSVP the host in one transaction.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	q := s.store.WithTx(tx)

	match, err := q.CreateMatch(ctx, store.CreateMatchParams{
		HostID: hostID, VenueID: venueID, Title: req.Title, Description: req.Description,
		Format: req.Format, MaxPlayers: req.MaxPlayers, KickoffAt: timestamptz(kickoff),
		DurationMin: duration,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := q.UpsertRsvp(ctx, store.UpsertRsvpParams{
		MatchID: match.ID, UserID: hostID, Status: "GOING",
	}); err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, matchDTO(match, 1))
}

func (s *Server) handleListMatches(w http.ResponseWriter, r *http.Request) {
	params := store.ListMatchesParams{
		Status:       queryStrPtr(r, "status"),
		UpcomingOnly: r.URL.Query().Get("upcoming") == "true",
		Lim:          queryInt(r, "limit", 20, 1, 100),
		Off:          queryInt(r, "offset", 0, 0, 1_000_000),
	}
	if v := r.URL.Query().Get("venueId"); v != "" {
		id, err := parseUUIDParam(v, "venueId")
		if err != nil {
			writeError(w, err)
			return
		}
		params.VenueID = &id
	}
	if v := r.URL.Query().Get("hostId"); v != "" {
		id, err := parseUUIDParam(v, "hostId")
		if err != nil {
			writeError(w, err)
			return
		}
		params.HostID = &id
	}

	rows, err := s.store.ListMatches(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Match, len(rows))
	for i, m := range rows {
		out[i] = matchRowDTO(m)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetMatch(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	match, err := s.store.GetMatch(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("match not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	rsvpRows, err := s.store.ListMatchRsvps(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	rsvps := make([]Rsvp, len(rsvpRows))
	going := int64(0)
	for i, row := range rsvpRows {
		rsvps[i] = rsvpRowDTO(row)
		if row.Status == "GOING" {
			going++
		}
	}
	writeJSON(w, http.StatusOK, MatchDetail{Match: matchDTO(match, going), Rsvps: rsvps})
}

type updateMatchRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Format      *string `json:"format"`
	MaxPlayers  *int32  `json:"maxPlayers"`
	KickoffAt   *string `json:"kickoffAt"`
	DurationMin *int32  `json:"durationMin"`
	VenueID     *string `json:"venueId"`
	Status      *string `json:"status"`
}

func (s *Server) handleUpdateMatch(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		writeError(w, err)
		return
	}

	match, err := s.store.GetMatch(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("match not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if match.HostID != userIDFrom(r.Context()) {
		writeError(w, errForbidden("only the host can modify this match"))
		return
	}

	params := store.UpdateMatchParams{ID: id, KickoffAt: nullTimestamptz()}
	if v, ok := raw["title"].(string); ok {
		params.Title = &v
	}
	if _, ok := raw["description"]; ok {
		params.SetDescription = true
		if v, ok := raw["description"].(string); ok {
			params.Description = &v
		}
	}
	if v, ok := raw["format"].(string); ok {
		params.Format = &v
	}
	if v, ok := raw["maxPlayers"].(float64); ok {
		n := int32(v)
		params.MaxPlayers = &n
	}
	if v, ok := raw["kickoffAt"].(string); ok {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			writeError(w, errValidation("kickoffAt must be an RFC3339 timestamp"))
			return
		}
		params.KickoffAt = timestamptz(t)
	}
	if v, ok := raw["durationMin"].(float64); ok {
		n := int32(v)
		params.DurationMin = &n
	}
	if _, ok := raw["venueId"]; ok {
		params.SetVenueID = true
		if v, ok := raw["venueId"].(string); ok {
			vid, verr := s.resolveVenueID(r.Context(), &v)
			if verr != nil {
				writeError(w, verr)
				return
			}
			params.VenueID = vid
		}
	}
	if v, ok := raw["status"].(string); ok {
		if !validMatchStatus[v] {
			writeError(w, errValidation("invalid status"))
			return
		}
		params.Status = &v
	}

	updated, err := s.store.UpdateMatch(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	going, err := s.store.CountMatchGoing(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, matchDTO(updated, going))
}

func (s *Server) handleCancelMatch(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	match, err := s.store.GetMatch(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("match not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if match.HostID != userIDFrom(r.Context()) {
		writeError(w, errForbidden("only the host can cancel this match"))
		return
	}
	if err := s.store.SetMatchStatus(r.Context(), store.SetMatchStatusParams{ID: id, Status: "CANCELLED"}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type rsvpRequest struct {
	Status *string `json:"status"`
}

func (s *Server) handleRsvp(w http.ResponseWriter, r *http.Request) {
	matchID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req rsvpRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	desired := "GOING"
	if req.Status != nil {
		desired = *req.Status
	}
	if desired != "GOING" && desired != "MAYBE" && desired != "DECLINED" {
		writeError(w, errValidation("status must be GOING, MAYBE or DECLINED"))
		return
	}

	status, err := s.doRsvp(r.Context(), matchID, userIDFrom(r.Context()), desired)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) handleLeaveMatch(w http.ResponseWriter, r *http.Request) {
	matchID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.doRsvp(r.Context(), matchID, userIDFrom(r.Context()), "DECLINED"); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// doRsvp performs the capacity-aware RSVP in a transaction: a full match sends
// GOING requests to the waitlist, and vacating a GOING slot promotes the
// longest-waiting player.
func (s *Server) doRsvp(ctx context.Context, matchID, userID uuid.UUID, desired string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	q := s.store.WithTx(tx)

	match, err := q.GetMatch(ctx, matchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound("match not found")
	} else if err != nil {
		return "", err
	}
	if match.Status == "CANCELLED" || match.Status == "COMPLETED" {
		return "", errBadRequest("cannot RSVP to a " + match.Status + " match")
	}

	existing, err := q.GetRsvp(ctx, store.GetRsvpParams{MatchID: matchID, UserID: userID})
	hadRsvp := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	status := desired
	if desired == "GOING" {
		going, cerr := q.CountGoingExcludingUser(ctx, store.CountGoingExcludingUserParams{
			MatchID: matchID, UserID: userID,
		})
		if cerr != nil {
			return "", cerr
		}
		if going >= int64(match.MaxPlayers) {
			status = "WAITLIST"
		}
	}

	record, err := q.UpsertRsvp(ctx, store.UpsertRsvpParams{MatchID: matchID, UserID: userID, Status: status})
	if err != nil {
		return "", err
	}

	// If a GOING slot was vacated, promote the oldest waitlisted player.
	if hadRsvp && existing.Status == "GOING" && status != "GOING" {
		next, nerr := q.GetOldestWaitlisted(ctx, matchID)
		if nerr == nil {
			if err := q.SetRsvpStatus(ctx, store.SetRsvpStatusParams{ID: next.ID, Status: "GOING"}); err != nil {
				return "", err
			}
		} else if !errors.Is(nerr, pgx.ErrNoRows) {
			return "", nerr
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return record.Status, nil
}

func (s *Server) resolveVenueID(ctx context.Context, raw *string) (*uuid.UUID, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	id, err := parseUUIDParam(*raw, "venueId")
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetVenue(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil, errBadRequest("venueId does not reference an existing venue")
	} else if err != nil {
		return nil, err
	}
	return &id, nil
}

var validMatchStatus = map[string]bool{
	"SCHEDULED": true, "IN_PROGRESS": true, "COMPLETED": true, "CANCELLED": true,
}
