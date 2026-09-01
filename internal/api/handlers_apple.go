package api

import (
	"context"
	"errors"
	"fmt"
	"log"
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
// a Person: an existing account linked by Apple sub, or a freshly provisioned identity
// (Person + UserAccount + personal Org + admin/director/coach memberships + seed
// templates — the same provisioning as email registration). Returns an access
// token identifying the Person, matching the app's `{ token, personID }` shape.
//
// What it deliberately no longer does is match an existing account *by email address*
// and link the Apple identity to it. See the refusal below.
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

	// 2. First Apple sign-in for this subject: provision a whole new identity.
	email := appleEmail(identity)

	// An account already holds this address, so the sign-in is refused rather than
	// merged into it.
	//
	// The merge that used to live here linked the Apple identity to whatever account
	// held the address and signed the caller into it. Both halves of a merge have to be
	// trustworthy and only one of them was: Apple's half is checked (its email_verified
	// claim, see appleEmail), while user_accounts.email is an address somebody typed
	// into POST /auth/register, which sends no verification mail and has no verified
	// column.
	//
	// So registering an address you do not own was a way to be handed the account of
	// whoever later signed in with Apple at it. Permanently: the password stays the
	// attacker's, nothing notifies the victim, and no endpoint unlinks an Apple sub. In
	// an app holding minors' medical notes and emergency contacts that is the worst
	// thing here, and its only preconditions are knowing an email address and arriving
	// first — which is every user of a newly shipped app.
	//
	// Proof of control has to come from something an attacker cannot hold at the same
	// time. That is the existing account's password, so linking moved to
	// POST /me/apple-link, which runs against an authenticated session. The cost is one
	// extra step, once, for a coach who genuinely has both.
	if _, err := s.store.GetUserAccountByEmail(ctx, email); err == nil {
		writeError(w, errEmailAlreadyRegistered())
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	q := s.store.WithTx(tx)

	person, account, err := s.provisionAppleIdentity(ctx, q, identity, req.FullName, email)
	if err != nil {
		// The lookup above races a concurrent registration at the same address. The
		// unique constraint catches that, and it is the same conflict, so it gets the
		// same answer rather than surfacing as a 500.
		if isUniqueViolation(err) {
			writeError(w, errEmailAlreadyRegistered())
			return
		}
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

type appleLinkRequest struct {
	IdentityToken string `json:"identityToken"`
}

// handleLinkApple attaches an Apple identity to the account the caller is already
// authenticated as. It is the other half of the refusal in handleAppleAuth: a coach who
// registered with a password and now wants Sign in with Apple links the two here, where
// the proof that both belong to the same person is the session they are already holding
// — a thing an attacker who merely typed the address into the registration form does not
// have.
//
// Deliberately not on /auth: it requires a bearer token, so it belongs behind
// requireAuth with the rest of /me.
func (s *Server) handleLinkApple(w http.ResponseWriter, r *http.Request) {
	var req appleLinkRequest
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

	account, err := s.store.GetUserAccountByPersonID(ctx, personIDFrom(ctx))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errUnauthorized("this account no longer exists"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if account.AppleSub != nil {
		// Re-linking the same Apple ID is what a client retrying a lost response does.
		if *account.AppleSub == identity.Sub {
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		writeError(w, errConflict("this account is already linked to a different Apple ID"))
		return
	}

	linked, err := s.store.LinkAppleSub(ctx, store.LinkAppleSubParams{
		ID: account.ID, AppleSub: &identity.Sub,
	})
	if err != nil {
		// user_accounts.apple_sub is UNIQUE, so this is an Apple ID that already belongs
		// to somebody else's account. Refusing keeps one Apple identity to one account,
		// which is what makes branch 1 of handleAppleAuth unambiguous.
		if isUniqueViolation(err) {
			writeError(w, errConflict("that Apple ID is already linked to another account"))
			return
		}
		writeError(w, err)
		return
	}
	if linked == 0 {
		// A concurrent link won between the read above and this write.
		writeError(w, errConflict("this account is already linked to a different Apple ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
	//
	// Deriving it from a public namespace makes the id predictable, which is the point
	// for reconciliation and a liability here: the insert must create the row, never
	// adopt one that is already there. See CreatePersonWithID for what adopting cost.
	personID := derivePersonID(identity.Sub)
	person, err := q.CreatePersonWithID(ctx, store.CreatePersonWithIDParams{
		ID: personID, DisplayName: displayName, Email: &email,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Someone else's row is sitting on this identity's id. Refusing is the only safe
		// answer — the alternative is handing them an account built on a row they do not
		// control — but it does mean a pre-claim denies this Apple ID sign-in until the
		// row is removed, so it is logged as the security event it is rather than
		// disappearing into a 409.
		log.Printf("apple provisioning refused: persons row %s already exists and was not "+
			"created by this sign-in; the Apple identity that derives it cannot be provisioned "+
			"until that row is investigated and removed", personID)
		return store.Person{}, store.UserAccount{}, errConflict(
			"this Apple ID cannot be set up right now because its account record is already " +
				"in use; please contact support")
	}
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
		Name: displayName + "'s Club", Kind: "personal", OwnerPersonID: &person.ID,
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

// An unverified address is treated as no address at all. user_accounts.email is UNIQUE
// and is what /auth/login and /auth/apple both key on, so writing one Apple has not
// vouched for would plant an address the account holder may not own — and an address
// nobody proved control of is exactly what made the merge this endpoint used to do a
// takeover primitive. Apple sets email_verified for genuine Apple IDs, including the
// private relay, so no ordinary sign-in ends up here.
func appleEmail(identity appleIdentity) string {
	if identity.EmailVerified {
		if e := strings.ToLower(strings.TrimSpace(identity.Email)); e != "" {
			return e
		}
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
