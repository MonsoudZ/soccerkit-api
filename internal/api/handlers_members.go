package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/authz"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

// Roles and who holds them. The model itself lives in internal/authz; this file is the
// HTTP surface over it — the catalogue a client builds its UI from, the caller's own
// effective permissions, and the grant/revoke pair that changes them.

// handleListRoles publishes the role catalogue: every role, what it means, and exactly
// what it may do.
//
// It exists so the iOS app can gate its UI on the server's table instead of a second
// copy of it. A client that hardcodes "hide the roster button unless coach" is a copy of
// the matrix that nobody updates, and it drifts in the direction that shows people
// buttons the API will refuse.
func (s *Server) handleListRoles(w http.ResponseWriter, _ *http.Request) {
	out := make([]RoleInfo, 0, len(authz.All))
	for _, r := range authz.All {
		out = append(out, roleInfoDTO(r))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetMyAccess answers "what may I do here, right now" for the organization the
// caller is acting in (X-Organization-ID, or their default org). Roles are resolved per
// request rather than baked into the access token, so this reflects a role granted a
// second ago without re-authenticating.
func (s *Server) handleGetMyAccess(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accessDTO(oc))
}

// handleListMembers lists who belongs to the organization and as what.
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapMemberRead, "you cannot see who belongs to this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListOrgMembers(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]OrgMember, len(rows))
	for i, row := range rows {
		out[i] = memberDTO(row)
	}
	writeJSON(w, http.StatusOK, out)
}

type grantRoleRequest struct {
	PersonID string `json:"personId"`
	Role     string `json:"role"`
}

// handleGrantRole gives a person a role in the caller's organization.
//
// Three guards, and each one is load-bearing:
//
//   - member.grant, so a coach cannot staff the club;
//   - the rank ceiling (authz.CanGrant), so a director cannot promote themselves to
//     admin — the capability alone would be a ladder to the top of the org;
//   - the target must already be linked to this organization, so this is not a way to
//     enroll arbitrary Persons by id.
//
// That last guard means there is no invitation flow here yet: staffing a club with a
// coach who does not have a record in it needs one, and it needs to be tied to Sign in
// with Apple rather than to an address somebody typed. Adding people to the org is
// POST /persons; this endpoint changes what they may do once they are in it.
func (s *Server) handleGrantRole(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapMemberGrant, "you cannot change roles in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	var req grantRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	personID, err := parseUUIDParam(req.PersonID, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	role := authz.Role(req.Role)
	if !role.Valid() {
		writeError(w, errValidation("unknown role: "+req.Role))
		return
	}
	if !oc.roles.CanGrant(role) {
		writeError(w, errForbidden("you cannot grant a role with more authority than your own"))
		return
	}
	if err := s.personVisibleTo(r.Context(), oc, personID); err != nil {
		writeError(w, err)
		return
	}

	// CreateMembership is ON CONFLICT DO NOTHING, so granting a role somebody already
	// holds returns no row. That is success, not failure: a client retrying a request it
	// is unsure landed must not be told the grant failed.
	if _, err := s.store.CreateMembership(r.Context(), store.CreateMembershipParams{
		PersonID: personID, OrganizationID: oc.orgID, Role: string(role),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}
	s.respondWithMemberRoles(w, r, oc, personID, http.StatusCreated)
}

// handleRevokeRole takes one role away from one person. It removes a role, never the
// person: someone who is no longer a coach is usually still a parent, and their athlete
// records must survive the demotion.
func (s *Server) handleRevokeRole(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapMemberRevoke, "you cannot change roles in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	personID, err := pathUUID(r, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	role := authz.Role(chi.URLParam(r, "role"))
	if !role.Valid() {
		writeError(w, errValidation("unknown role: "+chi.URLParam(r, "role")))
		return
	}
	if !oc.roles.CanRevoke(role) {
		writeError(w, errForbidden("you cannot revoke a role with more authority than your own"))
		return
	}

	// The count and the delete are one transaction, because the invariant they protect
	// is one an organization cannot recover from. An org with no admin is unadministrable
	// through the API — member.grant is an admin/director capability and the rank ceiling
	// means nobody remaining could hand the role back — and two concurrent revokes of two
	// different admins would each read "there are 2" and each commit. LockAdminMembershipIDs
	// takes the row locks that make the second one see the first one's write.
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	if role == authz.RoleAdmin {
		admins, lerr := q.LockAdminMembershipIDs(r.Context(), oc.orgID)
		if lerr != nil {
			writeError(w, lerr)
			return
		}
		// Applies to an admin stepping down as much as to one being removed: there is no
		// undo for this one, so it is refused rather than warned about.
		if len(admins) <= 1 {
			writeError(w, errConflict("an organization must keep at least one admin"))
			return
		}
	}

	rows, err := q.DeleteMembership(r.Context(), store.DeleteMembershipParams{
		PersonID: personID, OrganizationID: oc.orgID, Role: string(role),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == 0 {
		writeError(w, errNotFound("that person does not hold that role here"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	s.respondWithMemberRoles(w, r, oc, personID, http.StatusOK)
}

// respondWithMemberRoles answers a grant/revoke with the person's roles as they now
// stand, so a client never has to guess the result of its own write or re-fetch the
// whole member list to find out.
func (s *Server) respondWithMemberRoles(w http.ResponseWriter, r *http.Request, oc orgContext, personID uuid.UUID, status int) {
	roles, err := s.store.ListRolesInOrg(r.Context(), store.ListRolesInOrgParams{
		PersonID: personID, OrganizationID: oc.orgID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, MemberRoles{
		PersonID:       personID,
		OrganizationID: oc.orgID,
		Roles:          roleNames(authz.NewSet(roles...).Roles()),
	})
}

type linkGuardianRequest struct {
	GuardianPersonID string `json:"guardianPersonId"`
}

// handleLinkGuardian records that one person is the guardian of another. It is the
// join the whole parent tier stands on: a parent's reads are scoped to the children
// they are linked to (see personVisibleTo), so without a link a parent membership can
// see nothing at all — which is the correct default, and the reason this is staff-only.
func (s *Server) handleLinkGuardian(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapGuardianLink, "you cannot link guardians in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	childID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req linkGuardianRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	guardianID, err := parseUUIDParam(req.GuardianPersonID, "guardianPersonId")
	if err != nil {
		writeError(w, err)
		return
	}
	if guardianID == childID {
		writeError(w, errValidation("a person cannot be their own guardian"))
		return
	}
	// Both ends must be people this organization can see. Without the guardian side of
	// that check, a link is a way to hand an outsider a scoped read into the club.
	if err := s.personVisibleTo(r.Context(), oc, childID); err != nil {
		writeError(w, err)
		return
	}
	if err := s.personVisibleTo(r.Context(), oc, guardianID); err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.CreateGuardianship(r.Context(), store.CreateGuardianshipParams{
		GuardianPersonID: guardianID, ChildPersonID: childID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// ErrNoRows is the ON CONFLICT DO NOTHING path: the link is already there, which
		// is the state the caller asked for.
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"guardianPersonId": guardianID, "childPersonId": childID,
	})
}

// handleListMyChildren returns the children the caller is the registered guardian of.
// It needs no capability and no organization: it is the parent's own household, and it
// is the first call the parent app makes — everything else it shows hangs off these ids.
func (s *Server) handleListMyChildren(w http.ResponseWriter, r *http.Request) {
	children, err := s.store.ListChildren(r.Context(), personIDFrom(r.Context()))
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Person, 0, len(children))
	for _, c := range children {
		if c.Deleted {
			continue
		}
		out = append(out, personDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}
