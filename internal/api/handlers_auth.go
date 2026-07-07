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
	Email        string `json:"email"`
	Password     string `json:"password"`
	DisplayName  string `json:"displayName"`
	Organization string `json:"organizationName"` // optional; defaults to a personal org
}

// handleRegister provisions a full identity in one transaction: a Person, their
// UserAccount, a personal Organization, admin/director/coach memberships, and
// the seeded pre/post-game evaluation templates.
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

	ctx := r.Context()
	if _, err := s.store.GetUserAccountByEmail(ctx, email); err == nil {
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
	orgName := strings.TrimSpace(req.Organization)
	if orgName == "" {
		orgName = req.DisplayName + "'s Club"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	q := s.store.WithTx(tx)

	person, err := q.CreatePerson(ctx, store.CreatePersonParams{
		DisplayName: strings.TrimSpace(req.DisplayName),
		Email:       &email,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	account, err := q.CreateUserAccount(ctx, store.CreateUserAccountParams{
		PersonID: person.ID, Email: email, PasswordHash: &hash,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: orgName, Kind: "personal"})
	if err != nil {
		writeError(w, err)
		return
	}
	// Solo coach holds all three top roles in their personal org.
	for _, role := range []string{"admin", "director", "coach"} {
		if _, err := q.CreateMembership(ctx, store.CreateMembershipParams{
			PersonID: person.ID, OrganizationID: org.ID, Role: role,
		}); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := seedDefaultTemplates(ctx, q, org.ID, person.ID); err != nil {
		writeError(w, err)
		return
	}

	resp, err := s.issueTokens(ctx, q, account, person)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
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

	account, err := s.store.GetUserAccountByEmail(r.Context(), email)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errUnauthorized("invalid email or password"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if account.PasswordHash == nil || !verifyPassword(req.Password, *account.PasswordHash) {
		writeError(w, errUnauthorized("invalid email or password"))
		return
	}
	person, err := s.store.GetPerson(r.Context(), account.PersonID)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.issueTokens(r.Context(), s.store, account, person)
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
	if err := s.store.RevokeRefreshToken(ctx, stored.ID); err != nil {
		writeError(w, err)
		return
	}
	account, err := s.store.GetUserAccountByID(ctx, stored.UserAccountID)
	if err != nil {
		writeError(w, err)
		return
	}
	person, err := s.store.GetPerson(ctx, account.PersonID)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.issueTokens(ctx, s.store, account, person)
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

// issueTokens persists a rotating refresh token (keyed to the account) and signs
// an access token identifying the person, returning the full auth payload.
func (s *Server) issueTokens(ctx context.Context, q *store.Queries, account store.UserAccount, person store.Person) (AuthResponse, error) {
	refresh, err := newRefreshToken()
	if err != nil {
		return AuthResponse{}, err
	}
	if _, err := q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		Token:         refresh,
		UserAccountID: account.ID,
		ExpiresAt:     timestamptz(time.Now().Add(s.cfg.JWTRefreshTTL)),
	}); err != nil {
		return AuthResponse{}, err
	}
	access, err := s.signAccessToken(person.ID, account.Email)
	if err != nil {
		return AuthResponse{}, err
	}
	me, err := buildMe(ctx, q, person)
	if err != nil {
		return AuthResponse{}, err
	}
	return AuthResponse{AccessToken: access, RefreshToken: refresh, Me: me}, nil
}

// buildMe assembles the person + their memberships view.
func buildMe(ctx context.Context, q *store.Queries, person store.Person) (Me, error) {
	memberships, err := q.ListMembershipsForPerson(ctx, person.ID)
	if err != nil {
		return Me{}, err
	}
	views := make([]MembershipView, len(memberships))
	for i, m := range memberships {
		views[i] = MembershipView{
			OrganizationID: m.OrganizationID, OrganizationName: m.OrganizationName,
			OrganizationKind: m.OrganizationKind, Role: m.Role,
		}
	}
	return Me{Person: personDTO(person), Memberships: views}, nil
}

func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && strings.IndexByte(s[at+1:], '.') >= 0
}
