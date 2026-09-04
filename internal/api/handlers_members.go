package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// Member is one person's place in an organization: who they are, what they may do, and
// whether the club is theirs.
type Member struct {
	PersonID    uuid.UUID `json:"personId"`
	DisplayName string    `json:"displayName"`
	Email       *string   `json:"email"`
	Roles       []string  `json:"roles"`
	IsOwner     bool      `json:"isOwner"`
}

func memberDTO(row store.ListOrganizationMembersRow) Member {
	return Member{
		PersonID: row.PersonID, DisplayName: row.DisplayName, Email: row.Email,
		Roles: row.Roles, IsOwner: row.IsOwner,
	}
}

// orgFromPath resolves the {id} of an organization route and insists it is the one the
// caller is acting in.
//
// Every route in this API acts in a single organization, resolved from X-Organization-ID
// or the caller's only membership. Letting these routes reach sideways into another org
// the caller happens to belong to would make them the one exception, so a mismatch is
// refused with the header to set rather than quietly resolved a second way.
func (s *Server) orgFromPath(r *http.Request, oc orgContext) (store.Organization, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return store.Organization{}, err
	}
	if id != oc.orgID {
		return store.Organization{}, errForbidden(
			"that is not the organization you are acting in; set X-Organization-ID to it")
	}
	org, err := s.store.GetOrganization(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Organization{}, errNotFound("organization not found")
	} else if err != nil {
		return store.Organization{}, err
	}
	return org, nil
}

// handleListMembers lists who is in the organization and what they hold.
//
// Staff only. The list carries names and sign-in addresses, which is a club directory --
// useful to the people running the club, and not something a parent is owed about every
// other family.
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.orgFromPath(r, oc); err != nil {
		writeError(w, err)
		return
	}
	if !oc.isStaff() {
		writeError(w, errForbidden("only staff can list an organization's members"))
		return
	}
	rows, err := s.store.ListOrganizationMembers(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Member, len(rows))
	for i, row := range rows {
		out[i] = memberDTO(row)
	}
	writeJSON(w, http.StatusOK, out)
}

type setRolesRequest struct {
	Roles []string `json:"roles"`
}

// handleSetMemberRoles replaces what one member holds in the organization.
//
// Roles are rows, so this is a delete of the set and an insert of the new one, in a
// transaction: a partial application would leave someone holding a mix of what they had
// and what they were given.
func (s *Server) handleSetMemberRoles(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	org, err := s.orgFromPath(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireMemberManager(oc, org); err != nil {
		writeError(w, err)
		return
	}
	personID, err := pathUUID(r, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	var req setRolesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	roles, err := s.checkGrantableRoles(oc, req.Roles)
	if err != nil {
		writeError(w, err)
		return
	}
	current, err := s.memberRoles(r, oc, personID)
	if err != nil {
		writeError(w, err)
		return
	}
	// You may not rewrite someone who outranks you. Without this the ceiling on granting
	// is decoration: a director cannot appoint an admin, but could demote the one above
	// them and appoint a replacement.
	if highestRankOf(current) > oc.highestRank() {
		writeError(w, errForbidden("that member outranks you"))
		return
	}
	if err := s.wouldStrandOrg(r, oc, org, personID, current, roles); err != nil {
		writeError(w, err)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after a successful Commit
	q := s.store.WithTx(tx)
	if err := q.DeleteMembershipsForPersonInOrg(r.Context(), store.DeleteMembershipsForPersonInOrgParams{
		PersonID: personID, OrganizationID: oc.orgID,
	}); err != nil {
		writeError(w, err)
		return
	}
	for _, role := range roles {
		if _, err := q.CreateMembership(r.Context(), store.CreateMembershipParams{
			PersonID: personID, OrganizationID: oc.orgID, Role: role,
		}); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"personId": personID, "roles": roles})
}

// handleRemoveMember takes a person out of the organization entirely.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	org, err := s.orgFromPath(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireMemberManager(oc, org); err != nil {
		writeError(w, err)
		return
	}
	personID, err := pathUUID(r, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	current, err := s.memberRoles(r, oc, personID)
	if err != nil {
		writeError(w, err)
		return
	}
	if highestRankOf(current) > oc.highestRank() {
		writeError(w, errForbidden("that member outranks you"))
		return
	}
	if err := s.wouldStrandOrg(r, oc, org, personID, current, nil); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteMembershipsForPersonInOrg(r.Context(), store.DeleteMembershipsForPersonInOrgParams{
		PersonID: personID, OrganizationID: oc.orgID,
	}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- shared guards ---------------------------------------------------------

// requireMemberManager gates every write here. Deliberately narrower than isStaff: a
// coach runs training, and the authority to appoint another coach -- or to remove the
// director who appointed them -- is a different thing than the one the role is for. The
// owner qualifies whatever membership they hold, so an owner cannot be locked out of
// their own club by a role change.
func (s *Server) requireMemberManager(oc orgContext, org store.Organization) error {
	if org.OwnerPersonID != nil && *org.OwnerPersonID == oc.callerID {
		return nil
	}
	if !oc.canManageMembers() {
		return errForbidden("only an admin, director or the owner can manage members")
	}
	return nil
}

// checkGrantableRoles validates a requested role set and enforces the ceiling: nobody
// grants a role above their own highest. See roleRank.
func (s *Server) checkGrantableRoles(oc orgContext, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, errValidation("at least one role is required (" + strings.Join(allRoles, ", ") + ")")
	}
	seen := map[string]bool{}
	roles := make([]string, 0, len(requested))
	for _, role := range requested {
		if !validRole(role) {
			return nil, errValidation("unknown role " + role + "; expected one of " +
				strings.Join(allRoles, ", "))
		}
		if seen[role] {
			continue
		}
		if roleRank[role] > oc.highestRank() {
			return nil, errForbidden("you cannot grant " + role + ", which is above your own role")
		}
		seen[role] = true
		roles = append(roles, role)
	}
	slices.Sort(roles)
	return roles, nil
}

// memberRoles loads the target's current roles, and is where "not a member" becomes a
// 404 rather than an empty change that reports success.
func (s *Server) memberRoles(r *http.Request, oc orgContext, personID uuid.UUID) ([]string, error) {
	current, err := s.store.ListRolesForPersonInOrg(r.Context(), store.ListRolesForPersonInOrgParams{
		PersonID: personID, OrganizationID: oc.orgID,
	})
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return nil, errNotFound("that person is not a member of this organization")
	}
	return current, nil
}

// wouldStrandOrg refuses the one mistake this API cannot undo.
//
// An organization whose last admin is demoted or removed has nobody left who may manage
// members, and no endpoint that can put one back -- the club is stranded, and the fix is
// a database console. The owner is refused outright for the same reason: owner_person_id
// is what handleDeleteMe reads to decide whose orgs to delete, and a club whose owner is
// not a member is a state nothing else here expects.
func (s *Server) wouldStrandOrg(
	r *http.Request, oc orgContext, org store.Organization,
	personID uuid.UUID, current, next []string,
) error {
	if org.OwnerPersonID != nil && *org.OwnerPersonID == personID {
		if !slices.Contains(next, roleAdmin) {
			return errConflict("that person owns this organization, so they keep the admin role")
		}
		return nil
	}
	if !slices.Contains(current, roleAdmin) || slices.Contains(next, roleAdmin) {
		return nil
	}
	others, err := s.store.CountOtherAdminsInOrg(r.Context(), store.CountOtherAdminsInOrgParams{
		OrganizationID: oc.orgID, PersonID: personID,
	})
	if err != nil {
		return err
	}
	if others == 0 {
		return errConflict("this organization would be left with no admin; appoint another first")
	}
	return nil
}
