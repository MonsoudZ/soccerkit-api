package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/authz"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

// How somebody who already has an account joins an organization that is not their own.
//
// Every other way in is either wrong or unsafe. POST /persons creates a Person with no
// login — right for a U9 athlete, useless for a coach who has already signed in with
// Apple. POST /members refuses a person with no existing linkage to the org, because an
// id is not consent and accepting one would make "grant a role" a way to read a
// stranger's record. Sign in with Apple always provisions a fresh Person in that
// person's OWN personal org, so a parent signing up on their own lands nowhere near the
// club their child plays for.
//
// An invitation supplies the missing consent in both directions. Staff say who they are
// inviting and as what; the invitee proves it is them by redeeming a token only they
// were given, with an account they authenticated themselves. The membership is created
// for the account that redeemed it — never for an id somebody typed into a form.

// inviteTTL is how long an invitation stays redeemable. Long enough to survive a
// weekend, a parent who reads club email on Sundays, and a resend; short enough that a
// link forwarded into a group chat two seasons ago is dead.
const inviteTTL = 14 * 24 * time.Hour

// maxInvitationPage bounds the listing, like every other list in this API.
const maxInvitationPage = 200

type createInvitationRequest struct {
	Role string  `json:"role"`
	Note *string `json:"note"`
	// Email optionally binds the invitation to one address: only an account whose
	// Apple-verified address matches may redeem it, which turns a leaked link into a
	// dead one. Leave it unset for anyone who might use Hide My Email — see
	// handleAcceptInvitation.
	Email *string `json:"email"`
	// ChildPersonIds are the athletes a parent invitation makes its redeemer the
	// guardian of. Only meaningful for role=parent.
	ChildPersonIDs []string `json:"childPersonIds"`
}

// handleCreateInvitation issues an invitation and returns its token exactly once.
func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapInviteSend, "you cannot invite people to this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	var req createInvitationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	role := authz.Role(req.Role)
	if !role.Valid() {
		writeError(w, errValidation("unknown role: "+req.Role))
		return
	}
	// The same ceiling a direct grant is under, and for a coach a stricter one. Without
	// this, an invitation is a way around the grant rules: invite yourself as admin,
	// accept it, own the club.
	if !oc.roles.CanInvite(role) {
		writeError(w, errForbidden("you cannot invite someone as "+string(role)))
		return
	}

	children, err := s.parseInvitationChildren(r, oc, role, req.ChildPersonIDs)
	if err != nil {
		writeError(w, err)
		return
	}
	email, err := normalizeInviteEmail(req.Email)
	if err != nil {
		writeError(w, err)
		return
	}

	token, err := newInviteToken()
	if err != nil {
		writeError(w, err)
		return
	}
	inviter := personIDFrom(r.Context())

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	inv, err := q.CreateInvitation(r.Context(), store.CreateInvitationParams{
		OrganizationID: oc.orgID, TokenHash: hashInviteToken(token), Role: string(role),
		Email: email, Note: req.Note, InvitedByPersonID: &inviter,
		ExpiresAt: timestamptz(time.Now().Add(inviteTTL)),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	for _, child := range children {
		if err := q.AddInvitationChild(r.Context(), store.AddInvitationChildParams{
			InvitationID: inv.ID, ChildPersonID: child.ID,
		}); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}

	org, err := s.store.GetOrganization(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	// The only time the token is ever returned. It is not recoverable afterwards — the
	// database holds a hash — so a lost invitation is reissued, not looked up.
	writeJSON(w, http.StatusCreated, CreatedInvitation{
		Invitation: invitationDTO(inv, org.Name, children),
		Token:      token,
	})
}

// handleListInvitations shows what the organization has sent and what came of it.
func (s *Server) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapInviteSend, "you cannot see this organization's invitations")
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListInvitationsInOrg(r.Context(), store.ListInvitationsInOrgParams{
		OrganizationID: oc.orgID,
		Limit:          queryInt(r, "limit", 50, 1, maxInvitationPage),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	org, err := s.store.GetOrganization(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	ids := make([]uuid.UUID, len(rows))
	for i, inv := range rows {
		ids[i] = inv.ID
	}
	children, err := s.childRefsByInvitation(r.Context(), ids)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Invitation, len(rows))
	for i, inv := range rows {
		out[i] = invitationDTO(inv, org.Name, children[inv.ID])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeInvitation kills an outstanding link. An invitation that has already been
// accepted is not revoked — the membership it created is what to remove then, at
// DELETE /members/{personId}/roles/{role}.
func (s *Server) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapInviteSend, "you cannot manage this organization's invitations")
	if err != nil {
		writeError(w, err)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	inv, err := s.store.GetInvitation(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && inv.OrganizationID != oc.orgID) {
		writeError(w, errNotFound("invitation not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	// A coach may take back what they sent; cleaning up after somebody else is the job
	// of whoever staffs the club.
	mine := inv.InvitedByPersonID != nil && *inv.InvitedByPersonID == personIDFrom(r.Context())
	if !mine && !oc.can(authz.CapMemberGrant) {
		writeError(w, errForbidden("you can only revoke invitations you sent"))
		return
	}
	rows, err := s.store.RevokeInvitation(r.Context(), store.RevokeInvitationParams{
		ID: id, OrganizationID: oc.orgID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == 0 {
		writeError(w, errConflict("that invitation is already "+invitationStatus(inv)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "revoked"})
}

// handlePreviewInvitation answers "what am I being asked to join?" without joining it,
// so the app can show the club, the role and the children by name before the person
// commits. It needs the token, which is the only thing that proves they were invited.
//
// It is a POST, and the token is in the body, because the token is a credential. A GET
// would put it in the URL, and chi's request logger writes r.RequestURI — so every
// preview would print a working invitation into stdout, and from there into whatever
// aggregates logs, plus any proxy access log on the way. /auth/refresh takes its token
// in a body for the same reason. The verb is the price of not leaking the secret.
func (s *Server) handlePreviewInvitation(w http.ResponseWriter, r *http.Request) {
	var req acceptInvitationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	inv, org, children, err := s.invitationFromToken(r.Context(), req.Token)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, invitationDTO(inv, org.Name, children))
}

type acceptInvitationRequest struct {
	Token string `json:"token"`
}

// handleAcceptInvitation redeems an invitation for the authenticated caller.
//
// The caller is whoever signed in with Apple, and that is the whole security model of
// this endpoint: the membership goes to the account that presented the token, so an
// invitation cannot be redeemed on somebody else's behalf, and there is no address for
// this service to verify because Apple already did.
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var req acceptInvitationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	inv, org, children, err := s.invitationFromToken(r.Context(), req.Token)
	if err != nil {
		writeError(w, err)
		return
	}
	if st := invitationStatus(inv); st != invitationPending {
		writeError(w, errConflict("that invitation is "+st))
		return
	}
	callerID := personIDFrom(r.Context())
	if err := s.invitationEmailMatches(r.Context(), inv, callerID); err != nil {
		writeError(w, err)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	// The conditional UPDATE is what decides whether this redemption is the one that
	// counts. The status check above exists to give a person a clear answer, not to
	// guard the write: people forward these links, and two devices opening the same one
	// at the same time both read "pending".
	rows, err := q.AcceptInvitation(r.Context(), store.AcceptInvitationParams{
		ID: inv.ID, AcceptedByPersonID: &callerID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == 0 {
		writeError(w, errConflict("that invitation has already been used"))
		return
	}
	// DO NOTHING on conflict: somebody who already holds the role is not an error, and
	// re-inviting an existing member is a normal way to attach new children.
	if _, err := q.CreateMembership(r.Context(), store.CreateMembershipParams{
		PersonID: callerID, OrganizationID: inv.OrganizationID, Role: inv.Role,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}
	for _, child := range children {
		if _, err := q.CreateGuardianship(r.Context(), store.CreateGuardianshipParams{
			GuardianPersonID: callerID, ChildPersonID: child.ID,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}

	// Answer with what they can now do, so the app's next screen does not need a second
	// round trip to find out.
	roles, err := s.store.ListRolesInOrg(r.Context(), store.ListRolesInOrgParams{
		PersonID: callerID, OrganizationID: inv.OrganizationID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, InvitationAccepted{
		OrganizationID:   inv.OrganizationID,
		OrganizationName: org.Name,
		Role:             inv.Role,
		LinkedChildren:   children,
		Access:           accessDTO(orgContext{orgID: inv.OrganizationID, roles: authz.NewSet(roles...)}),
	})
}

// --- shared helpers -------------------------------------------------------

// invitationFromToken resolves a presented token to its invitation, organization and
// children. A token that matches nothing is a 404 and says nothing else: whether a
// string is a real invitation is not something an unrecognized caller gets to learn.
func (s *Server) invitationFromToken(ctx context.Context, token string) (store.Invitation, store.Organization, []PersonRef, error) {
	if token == "" {
		return store.Invitation{}, store.Organization{}, nil, errValidation("token is required")
	}
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInviteToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Invitation{}, store.Organization{}, nil, errNotFound("that invitation link is not valid")
	} else if err != nil {
		return store.Invitation{}, store.Organization{}, nil, err
	}
	org, err := s.store.GetOrganization(ctx, inv.OrganizationID)
	if err != nil {
		return store.Invitation{}, store.Organization{}, nil, err
	}
	children, err := s.childRefsByInvitation(ctx, []uuid.UUID{inv.ID})
	if err != nil {
		return store.Invitation{}, store.Organization{}, nil, err
	}
	return inv, org, children[inv.ID], nil
}

// invitationEmailMatches enforces the optional address binding.
//
// Apple's Hide My Email is why this is optional rather than always on: an invitee who
// hides their address signs in with a private relay that will never equal what the club
// typed, so a bound invitation would lock out precisely the person it was for. When a
// club knows the address will match, the binding makes a forwarded link useless; when it
// does not, the token's own entropy is what stands in the way.
func (s *Server) invitationEmailMatches(ctx context.Context, inv store.Invitation, callerID uuid.UUID) error {
	if inv.Email == nil {
		return nil
	}
	account, err := s.store.GetUserAccountByPersonID(ctx, callerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errForbidden("this invitation was issued to a specific email address")
	} else if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(account.Email), *inv.Email) {
		return errForbidden("this invitation was issued to a different email address")
	}
	return nil
}

// parseInvitationChildren validates the athletes a parent invitation will link to.
func (s *Server) parseInvitationChildren(r *http.Request, oc orgContext, role authz.Role, ids []string) ([]PersonRef, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Guardianship of an athlete is what a parent role means. Attaching children to a
	// coach or a player invitation would be silently doing something else.
	if role != authz.RoleParent {
		return nil, errValidation("childPersonIds is only meaningful for a parent invitation")
	}
	out := make([]PersonRef, 0, len(ids))
	for _, raw := range ids {
		id, err := parseUUIDParam(raw, "childPersonIds")
		if err != nil {
			return nil, err
		}
		// The athlete has to be one this organization can see, checked now rather than
		// at redemption: an invitation that cannot be honoured should fail in the hands
		// of the coach who made the mistake, not the parent who received it.
		if err := s.personVisibleTo(r.Context(), oc, id); err != nil {
			return nil, err
		}
		person, err := s.store.GetPerson(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound("person not found")
		} else if err != nil {
			return nil, err
		}
		out = append(out, PersonRef{ID: person.ID, DisplayName: person.DisplayName})
	}
	return out, nil
}

// childRefsByInvitation groups the children of many invitations in one query, so a page
// of invitations does not become a page of queries.
func (s *Server) childRefsByInvitation(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]PersonRef, error) {
	out := map[uuid.UUID][]PersonRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.store.ListInvitationChildRefs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.InvitationID] = append(out[row.InvitationID], PersonRef{
			ID: row.PersonID, DisplayName: row.DisplayName,
		})
	}
	return out, nil
}

// normalizeInviteEmail stores the address in one shape so the comparison at redemption
// is not at the mercy of what somebody typed.
func normalizeInviteEmail(email *string) (*string, error) {
	if email == nil {
		return nil, nil
	}
	trimmed := strings.ToLower(strings.TrimSpace(*email))
	if trimmed == "" {
		return nil, nil
	}
	if !strings.Contains(trimmed, "@") {
		return nil, errValidation("email must be an email address")
	}
	return &trimmed, nil
}
