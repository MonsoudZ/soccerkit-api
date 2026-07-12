package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// appleAuthRequest mirrors the iOS app's AppleAuthRequest: the identity token to
// verify, plus the one-time authorization code and the full name (Apple only
// sends the name on the very first authorization).
type appleAuthRequest struct {
	IdentityToken     string  `json:"identityToken"`
	AuthorizationCode *string `json:"authorizationCode"`
	FullName          *string `json:"fullName"`
}

// appleAuthResponse mirrors the app's AuthResponse: the session token and the
// Person the account maps to.
//
// RefreshToken is additive (the app's Codable ignores keys it doesn't know). It
// exists because this endpoint used to hand back a bare access token and no way
// to renew it: with JWT_ACCESS_TTL at 15m, an Apple-signed-in coach was logged
// out mid-training-session and the app's only recovery was to re-run the whole
// Sign in with Apple flow. Register and login have always returned one.
type appleAuthResponse struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
	PersonID     string `json:"personID"`
}

// handleAppleAuth verifies a Sign in with Apple identity token and resolves it to
// a Person: an existing account linked by Apple sub, an existing account matched
// by email (linked on the spot), or a freshly provisioned identity (Person +
// UserAccount + personal Org + admin/director/coach memberships + seed
// templates — the same provisioning as email registration). Returns an access
// token identifying the Person, matching the app's `{ token, personID }` shape.
func (s *Server) handleAppleAuth(w http.ResponseWriter, r *http.Request) {
	var req appleAuthRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(req.IdentityToken) == "" {
		writeError(w, errValidation("identityToken is required"))
		return
	}

	ctx := r.Context()
	identity, err := s.apple.verify(ctx, req.IdentityToken)
	if err != nil {
		writeError(w, errUnauthorized("Apple sign-in could not be verified"))
		return
	}

	// 1. Returning user — already linked by Apple sub.
	if account, err := s.store.GetUserAccountByAppleSub(ctx, &identity.Sub); err == nil {
		person, err := s.store.GetPerson(ctx, account.PersonID)
		if err != nil {
			writeError(w, err)
			return
		}
		auth, err := s.issueTokens(ctx, s.store, account, person)
		if err != nil {
			writeError(w, err)
			return
		}
		respondAppleAuth(w, auth, person.ID)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}

	// 2. First Apple sign-in — link an existing email account or provision anew,
	// atomically.
	email := appleEmail(identity)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	q := s.store.WithTx(tx)

	var account store.UserAccount
	var person store.Person

	existing, err := q.GetUserAccountByEmail(ctx, email)
	switch {
	case err == nil:
		// An email/password account already exists for this address — link it.
		if err := q.LinkAppleSub(ctx, store.LinkAppleSubParams{ID: existing.ID, AppleSub: &identity.Sub}); err != nil {
			writeError(w, err)
			return
		}
		linked, perr := q.GetPerson(ctx, existing.PersonID)
		if perr != nil {
			writeError(w, perr)
			return
		}
		account, person = existing, linked
	case errors.Is(err, pgx.ErrNoRows):
		provisioned, created, perr := s.provisionAppleIdentity(ctx, q, identity, req.FullName, email)
		if perr != nil {
			writeError(w, perr)
			return
		}
		account, person = created, provisioned
	default:
		writeError(w, err)
		return
	}

	// Issued inside the transaction: the refresh token is a row, so it must land
	// or roll back with the identity it belongs to.
	auth, err := s.issueTokens(ctx, q, account, person)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, err)
		return
	}
	respondAppleAuth(w, auth, person.ID)
}

// provisionAppleIdentity creates the full identity for a first-time Apple user,
// mirroring email registration (Person + UserAccount + personal Org + roles +
// seeded templates).
func (s *Server) provisionAppleIdentity(
	ctx context.Context, q *store.Queries, identity appleIdentity, fullName *string, email string,
) (store.Person, store.UserAccount, error) {
	displayName := appleDisplayName(fullName, email)

	// The coach's person id is derived from the Apple subject, so it matches the
	// id the app derives locally — the account Person and the synced Person are
	// one row (see derivePersonID).
	person, err := q.CreatePersonWithID(ctx, store.CreatePersonWithIDParams{
		ID: derivePersonID(identity.Sub), DisplayName: displayName, Email: &email,
	})
	if err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	sub := identity.Sub
	account, err := q.CreateUserAccount(ctx, store.CreateUserAccountParams{
		PersonID: person.ID, Email: email, AppleSub: &sub,
	})
	if err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: displayName + "'s Club", Kind: "personal",
	})
	if err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	for _, role := range []string{"admin", "director", "coach"} {
		if _, err := q.CreateMembership(ctx, store.CreateMembershipParams{
			PersonID: person.ID, OrganizationID: org.ID, Role: role,
		}); err != nil {
			return store.Person{}, store.UserAccount{}, err
		}
	}
	if err := seedDefaultTemplates(ctx, q, org.ID, person.ID); err != nil {
		return store.Person{}, store.UserAccount{}, err
	}
	return person, account, nil
}

// respondAppleAuth renders the app's `{ token, refreshToken, personID }` shape
// from the same AuthResponse register and login return.
func respondAppleAuth(w http.ResponseWriter, auth AuthResponse, personID uuid.UUID) {
	writeJSON(w, http.StatusOK, appleAuthResponse{
		Token:        auth.AccessToken,
		RefreshToken: auth.RefreshToken,
		PersonID:     personID.String(),
	})
}

// appleEmail returns the token's email, or a stable synthesized address when the
// user chose to hide it (Apple omits email on later sign-ins and for hidden
// relays), so the NOT NULL UNIQUE user_accounts.email constraint always holds.
// coachPersonNamespace is a fixed UUID namespace shared verbatim with the iOS
// client. Both derive the coach's Person id as UUIDv5(namespace, appleSub), so
// the same Apple user maps to the same Person id on every device and the server
// — no id round-tripping, no migration. Keep this value identical in the client.
var coachPersonNamespace = uuid.MustParse("2b6f0cc9-04e9-4c8e-8f1a-7c3d5e2a9b40")

// derivePersonID maps an Apple subject to its stable coach Person id.
func derivePersonID(appleSub string) uuid.UUID {
	return uuid.NewSHA1(coachPersonNamespace, []byte(appleSub))
}

func appleEmail(identity appleIdentity) string {
	if e := strings.ToLower(strings.TrimSpace(identity.Email)); e != "" {
		return e
	}
	return fmt.Sprintf("apple_%s@users.soccercoachkit.app", identity.Sub)
}

// appleDisplayName prefers the name Apple sent on first authorization, else the
// email local part, else a neutral default.
func appleDisplayName(fullName *string, email string) string {
	if fullName != nil {
		if t := strings.TrimSpace(*fullName); t != "" {
			return t
		}
	}
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" {
		return local
	}
	return "Coach"
}
