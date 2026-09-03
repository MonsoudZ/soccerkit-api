package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// Organization is the club (or personal workspace) a coach works in.
type Organization struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"createdAt"`
}

func organizationDTO(o store.Organization) Organization {
	return Organization{
		ID: o.ID.String(), Name: o.Name, Kind: o.Kind, CreatedAt: rfc3339(o.CreatedAt),
	}
}

// updatableOrganizationFields is the set PATCH /organizations/{id} accepts.
//
// `kind` is not in it. Flipping an org between 'personal' and 'club' changes what
// account deletion destroys -- handleDeleteMe deletes the personal orgs the caller owns
// and orphans their clubs -- so it is a product decision with a data-loss edge on one
// side of it, not a field on a PATCH body. See UpdateOrganization in
// db/queries/identity.sql.
var updatableOrganizationFields = map[string]bool{"name": true}

// handleUpdateOrganization renames the club the caller is acting in.
//
// Unlike teams and persons, organizations carry no sync spine at all -- no
// sync_account_id, payload, deleted or seq column -- so there is no second copy of the
// name to keep in step and nothing to propagate. The club exists only on this side of
// the wire; the app learns its name from /me.
func (s *Server) handleUpdateOrganization(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	// Every route in this API acts in one organization at a time, the one resolved from
	// X-Organization-ID (or the caller's only org). Editing a different one you happen
	// to belong to would be the single exception, so it is refused with the header to
	// set rather than quietly resolved a second way.
	if id != oc.orgID {
		writeError(w, errForbidden(
			"that is not the organization you are acting in; set X-Organization-ID to it"))
		return
	}
	org, err := s.store.GetOrganization(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("organization not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	// An owner who somehow holds no admin membership can still rename what they own;
	// otherwise this is an administrative act, not a coaching one.
	callerID := personIDFrom(r.Context())
	owns := org.OwnerPersonID != nil && *org.OwnerPersonID == callerID
	if !owns && !oc.hasAnyRole("admin", "director") {
		writeError(w, errForbidden("only an admin, director or the owner can rename an organization"))
		return
	}

	raw, err := decodePatch(r, updatableOrganizationFields)
	if err != nil {
		writeError(w, err)
		return
	}
	params := store.UpdateOrganizationParams{ID: id}
	if v, ok := raw["name"]; ok {
		name, verr := requiredString(v, "name")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.Name = &name
	}
	updated, err := s.store.UpdateOrganization(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, organizationDTO(updated))
}
