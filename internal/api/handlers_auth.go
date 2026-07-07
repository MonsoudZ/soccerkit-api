package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

type registerRequest struct {
	Email       string  `json:"email"`
	Password    string  `json:"password"`
	DisplayName string  `json:"displayName"`
	Position    *string `json:"position"`
	SkillLevel  *int32  `json:"skillLevel"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !looksLikeEmail(email) {
		writeError(w, errValidation("a valid email is required"))
		return
	}
	if len(req.Password) < 8 {
		writeError(w, errValidation("password must be at least 8 characters"))
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		writeError(w, errValidation("displayName is required"))
		return
	}
	if err := validatePosition(req.Position); err != nil {
		writeError(w, err)
		return
	}
	skill := int32(3)
	if req.SkillLevel != nil {
		if *req.SkillLevel < 1 || *req.SkillLevel > 5 {
			writeError(w, errValidation("skillLevel must be between 1 and 5"))
			return
		}
		skill = *req.SkillLevel
	}

	ctx := r.Context()
	if _, err := s.store.GetUserByEmail(ctx, email); err == nil {
		writeError(w, errConflict("an account with that email already exists"))
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}

	hash, err := hashPassword(req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	user, err := s.store.CreateUser(ctx, store.CreateUserParams{
		Email: email, PasswordHash: hash, DisplayName: strings.TrimSpace(req.DisplayName),
		Position: req.Position, SkillLevel: skill,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	resp, err := s.issueTokens(ctx, user)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	user, err := s.store.GetUserByEmail(r.Context(), email)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errUnauthorized("invalid email or password"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if !verifyPassword(req.Password, user.PasswordHash) {
		writeError(w, errUnauthorized("invalid email or password"))
		return
	}

	resp, err := s.issueTokens(r.Context(), user)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()

	stored, err := s.store.GetRefreshToken(ctx, req.RefreshToken)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errUnauthorized("invalid or expired refresh token"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if stored.RevokedAt.Valid || stored.ExpiresAt.Time.Before(time.Now()) {
		writeError(w, errUnauthorized("invalid or expired refresh token"))
		return
	}

	// Rotate: revoke the presented token, issue a fresh pair.
	if err := s.store.RevokeRefreshToken(ctx, stored.ID); err != nil {
		writeError(w, err)
		return
	}
	user, err := s.store.GetUserByID(ctx, stored.UserID)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.issueTokens(ctx, user)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.RevokeRefreshTokenByToken(r.Context(), req.RefreshToken); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// issueTokens persists a rotating refresh token and signs an access token.
func (s *Server) issueTokens(ctx context.Context, user store.User) (AuthResponse, error) {
	refresh, err := newRefreshToken()
	if err != nil {
		return AuthResponse{}, err
	}
	if _, err := s.store.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		Token:     refresh,
		UserID:    user.ID,
		ExpiresAt: timestamptz(time.Now().Add(s.cfg.JWTRefreshTTL)),
	}); err != nil {
		return AuthResponse{}, err
	}
	access, err := s.signAccessToken(user.ID, user.Email)
	if err != nil {
		return AuthResponse{}, err
	}
	return AuthResponse{AccessToken: access, RefreshToken: refresh, User: publicUser(user)}, nil
}

// --- small validators -----------------------------------------------------

func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && strings.IndexByte(s[at+1:], '.') >= 0
}

var validPositions = map[string]bool{"GK": true, "DEF": true, "MID": true, "FWD": true}

func validatePosition(p *string) error {
	if p != nil && !validPositions[*p] {
		return errValidation("position must be one of GK, DEF, MID, FWD")
	}
	return nil
}
