package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// Org-scoping guards for the by-id routes.
//
// teamInOrg and gameInOrg already did this for their own resources. Persons,
// templates and form instances had no guard at all: the handlers went from
// pathUUID straight to the query, so any authenticated caller who could name an
// id could read any athlete's medical notes or any submitted evaluation.
//
// These report a resource the caller may not see as 404, not 403. A coach's
// Person id is a derived UUIDv5 of their Apple sub (see derivePersonID) rather
// than a random value, so it is guessable, and a 403 would confirm it exists.

// personVisible reports whether a Person is reachable from the given org.
//
// persons carries no organization_id: the whole-castle schema ties a Person to
// an org only through a membership (an athlete enrolled via POST /persons, or
// the coach themselves) or through the roster of one of that org's teams. Those
// two edges are the entire visibility rule.
func (s *Server) personVisible(r *http.Request, oc orgContext, id uuid.UUID) (bool, error) {
	// sqlc types the `EXISTS(...) OR EXISTS(...)` projection as nullable even
	// though it cannot be NULL; treat a nil as not-visible rather than trusting it.
	visible, err := s.store.PersonVisibleInOrg(r.Context(), store.PersonVisibleInOrgParams{
		PersonID: id, OrganizationID: oc.orgID,
	})
	if err != nil {
		return false, err
	}
	return visible != nil && *visible, nil
}

// scopedPerson resolves the caller's org and the {id} path param together and
// verifies the person is visible in it. Every /persons/{id} route needs all three.
func (s *Server) scopedPerson(r *http.Request) (uuid.UUID, error) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return uuid.Nil, err
	}
	visible, err := s.personVisible(r, oc, id)
	if err != nil {
		return uuid.Nil, err
	}
	if !visible {
		return uuid.Nil, errNotFound("person not found")
	}
	return id, nil
}

// templateInOrg verifies the template belongs to the caller's org or was authored
// by them — the same predicate ListFormTemplates already filters on, which the
// by-id read never applied.
func (s *Server) templateInOrg(r *http.Request, oc orgContext, tpl store.FormTemplate) bool {
	if tpl.OrganizationID != nil && *tpl.OrganizationID == oc.orgID {
		return true
	}
	return tpl.AuthorPersonID != nil && *tpl.AuthorPersonID == personIDFrom(r.Context())
}

// teamIDInOrg verifies a team id belongs to the caller's org.
func (s *Server) teamIDInOrg(r *http.Request, oc orgContext, id uuid.UUID) (bool, error) {
	team, err := s.store.GetTeam(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return team.OrganizationID == oc.orgID, nil
}

// instanceInOrg verifies a submitted evaluation is in the caller's org. An
// instance carries no organization_id; it inherits its scope from its subject,
// and the table's CHECK guarantees at least one of the two subjects is set. Both
// are verified when both are present.
func (s *Server) instanceInOrg(r *http.Request, oc orgContext, inst store.FormInstance) (bool, error) {
	if inst.SubjectTeamID == nil && inst.SubjectPersonID == nil {
		return false, nil
	}
	if inst.SubjectTeamID != nil {
		ok, err := s.teamIDInOrg(r, oc, *inst.SubjectTeamID)
		if err != nil || !ok {
			return false, err
		}
	}
	if inst.SubjectPersonID != nil {
		ok, err := s.personVisible(r, oc, *inst.SubjectPersonID)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}
