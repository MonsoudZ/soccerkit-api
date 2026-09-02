package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// The only way into an account is Sign in with Apple; see handlers_apple.go. This file
// is what happens after that: rotating the session, ending it, and assembling the
// caller's view of themselves.
//
// There used to be email+password registration and login here as well. Nothing shipped
// used them — the iOS client authenticates with Apple and renews with /auth/refresh, and
// never called either — while they were a live, unauthenticated way for anyone with curl
// to create an account at any address. That is what made the pre-hijack in
// docs/AUDIT-3.md C1 possible (an address nobody verified became a merge key) and what
// forced /auth/register to disclose which addresses are taken (L5). Deleting them closes
// both by removing the thing rather than guarding it, and takes password reset and email
// verification off the list of things this service owes its users.
//
// Bringing them back means bringing back verified addresses with them: an Android or web
// client needs a credential, and a credential needs an address somebody proved they own.

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// refreshReplayGrace is how long after a rotation the superseded token may be presented
// again without being treated as a stolen-chain replay. Long enough to cover a client
// retrying a refresh whose response it never received, short enough that a leaked token
// is of no practical use.
const refreshReplayGrace = 30 * time.Second

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()

	stored, err := s.store.GetRefreshToken(ctx, hashRefreshToken(req.RefreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errUnauthorized("invalid or expired refresh token"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	// A token that exists but is already revoked was rotated away by an earlier
	// refresh, so someone is presenting a copy. Rotation alone would leave the live
	// chain intact for whoever holds it, so the whole family is revoked and both
	// parties must sign in again.
	//
	// Except immediately after the rotation, which is the one time a replay is more
	// likely to be honest than hostile: this backs an offline-first phone app, and a
	// refresh whose response was lost to a dropped connection gets retried with the
	// same token. Inside the grace window that is refused without the cascade, so a
	// flaky network costs one retry rather than every session on every device.
	// An expired token is just expired — no signal, no cascade.
	if stored.RevokedAt.Valid {
		if time.Since(stored.RevokedAt.Time) > refreshReplayGrace {
			if err := s.store.RevokeRefreshTokensForAccount(ctx, stored.UserAccountID); err != nil {
				writeError(w, err)
				return
			}
		}
		writeError(w, errUnauthorized("invalid or expired refresh token"))
		return
	}
	if stored.ExpiresAt.Time.Before(time.Now()) {
		writeError(w, errUnauthorized("invalid or expired refresh token"))
		return
	}
	// The revoke is the guard, not a formality after one. Reading the row and then
	// revoking it unconditionally is a check-then-act with nothing between the two
	// statements, and simultaneous presentations of one token all passed the check
	// before any of them wrote: 32 concurrent redemptions of a single token produced six
	// live families, none of which tripped the replay cascade. Zero rows here means
	// another request took this token microseconds ago, which is the same situation the
	// grace window above exists for — a retry, not a theft — so it is refused without
	// the cascade rather than treated as evidence.
	rotated, err := s.store.RevokeRefreshToken(ctx, stored.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if rotated == 0 {
		writeError(w, errUnauthorized("invalid or expired refresh token"))
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
	if err := s.store.DeleteRefreshTokenByToken(r.Context(), hashRefreshToken(req.RefreshToken)); err != nil {
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
		TokenHash:     hashRefreshToken(refresh),
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
