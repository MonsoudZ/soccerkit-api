package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/authz"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	person, err := s.store.GetPerson(r.Context(), personIDFrom(r.Context()))
	if errors.Is(err, pgx.ErrNoRows) {
		// The access token outlives the row it names — handleDeleteMe says so, and is
		// built around it. This is the read side of the same fact, and it used to fall
		// through to a 500: for up to JWT_ACCESS_TTL after a successful account
		// deletion, the app's own "who am I" call reported a server fault. A client
		// cannot tell that from an outage, so the reasonable response — keep the
		// session, retry — is the wrong one, and the deletion the 204 completed looks
		// half-finished. 401 says the true thing: the token is valid and identifies
		// nobody, so sign out.
		writeError(w, errUnauthorized("this account no longer exists"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	me, err := buildMe(r.Context(), s.store, person)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

type createPersonRequest struct {
	DisplayName           string  `json:"displayName"`
	GivenName             *string `json:"givenName"`
	FamilyName            *string `json:"familyName"`
	Birthdate             *string `json:"birthdate"` // YYYY-MM-DD
	Email                 *string `json:"email"`
	Phone                 *string `json:"phone"`
	EmergencyContactName  *string `json:"emergencyContactName"`
	EmergencyContactPhone *string `json:"emergencyContactPhone"`
	MedicalNotes          *string `json:"medicalNotes"`
	// Role the new person holds in the caller's organization. Defaults to "player".
	// Always produces a membership: a Person with no org linkage would be visible to
	// nobody, not even the coach who just created them.
	Role *string `json:"role"`
}

// personRoles are the roles POST /persons may grant. The endpoint creates Persons with
// no login, so it deliberately cannot mint the privileged admin/director/coach roles —
// those are handed out at POST /members, where the rank ceiling applies. Staffing a club
// through the endpoint that creates login-less athlete records would be a way around it.
var personRoles = map[authz.Role]bool{authz.RolePlayer: true, authz.RoleParent: true}

// handleCreatePerson creates an athlete (a Person, usually with no login) in the
// coach's organization.
func (s *Server) handleCreatePerson(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.can(authz.CapPersonCreate) {
		writeError(w, errForbidden("you cannot add people to this organization"))
		return
	}
	var req createPersonRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.DisplayName == "" {
		writeError(w, errValidation("displayName is required"))
		return
	}
	bd, err := parseDate(req.Birthdate)
	if err != nil {
		writeError(w, err)
		return
	}
	role := "player"
	if req.Role != nil {
		role = *req.Role
	}
	if !personRoles[authz.Role(role)] {
		writeError(w, errValidation("role must be player or parent"))
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	q := s.store.WithTx(tx)

	person, err := q.CreatePerson(r.Context(), store.CreatePersonParams{
		DisplayName: req.DisplayName, GivenName: req.GivenName, FamilyName: req.FamilyName,
		Birthdate: bd, Email: req.Email, Phone: req.Phone,
		EmergencyContactName: req.EmergencyContactName, EmergencyContactPhone: req.EmergencyContactPhone,
		MedicalNotes: req.MedicalNotes,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := q.CreateMembership(r.Context(), store.CreateMembershipParams{
		PersonID: person.ID, OrganizationID: oc.orgID, Role: role,
	}); err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, personDTO(person))
}

func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request) {
	id, _, err := s.visiblePersonFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	person, err := s.store.GetPerson(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("person not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, personDTO(person))
}

func (s *Server) handleListPersonInstances(w http.ResponseWriter, r *http.Request) {
	id, oc, err := s.visiblePersonFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.can(authz.CapEvaluationRead) {
		writeError(w, errForbidden("you cannot read evaluations in this organization"))
		return
	}
	rows, err := s.store.ListInstancesForPerson(r.Context(), store.ListInstancesForPersonParams{
		SubjectPersonID: &id,
		Context:         queryStrPtr(r, "context"),
		Lim:             queryInt(r, "limit", 50, 1, 200),
		Off:             queryInt(r, "offset", 0, 0, 1_000_000),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]InstanceSummary, len(rows))
	for i, row := range rows {
		out[i] = instanceSummaryDTO(row)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePersonAggregate returns cross-instance score averages for an athlete —
// the readiness-mean / effort-trend query that is the product's analytical core.
func (s *Server) handlePersonAggregate(w http.ResponseWriter, r *http.Request) {
	id, oc, err := s.visiblePersonFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.can(authz.CapEvaluationRead) {
		writeError(w, errForbidden("you cannot read evaluations in this organization"))
		return
	}
	rows, err := s.store.AggregateScoresForPerson(r.Context(), store.AggregateScoresForPersonParams{
		SubjectPersonID: &id,
		Context:         queryStrPtr(r, "context"),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]ScoreAggregate, len(rows))
	for i, row := range rows {
		out[i] = aggregateDTO(row)
	}
	writeJSON(w, http.StatusOK, out)
}

// visiblePersonFromPath resolves the {id} path parameter to a Person the caller's
// organization is allowed to see. Every read keyed on a person id goes through here —
// these endpoints return birthdate, contact details and medical notes, so an
// unauthenticated-by-org read is a disclosure of a minor's PII.
func (s *Server) visiblePersonFromPath(r *http.Request) (uuid.UUID, orgContext, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return uuid.Nil, orgContext{}, err
	}
	oc, err := s.requireCapability(r, authz.CapPersonRead, "you cannot look up people in this organization")
	if err != nil {
		return uuid.Nil, orgContext{}, err
	}
	if err := s.personVisibleTo(r.Context(), oc, id); err != nil {
		return uuid.Nil, orgContext{}, err
	}
	return id, oc, nil
}
