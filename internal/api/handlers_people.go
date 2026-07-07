package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	person, err := s.store.GetPerson(r.Context(), personIDFrom(r.Context()))
	if err != nil {
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
	// When true (default), the new person is enrolled as a player in the org.
	AsPlayer *bool `json:"asPlayer"`
}

// handleCreatePerson creates an athlete (a Person, usually with no login) in the
// coach's organization.
func (s *Server) handleCreatePerson(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !oc.hasAnyRole("admin", "director", "coach") {
		writeError(w, errForbidden("only coaches can add people"))
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
	if req.AsPlayer == nil || *req.AsPlayer {
		if _, err := q.CreateMembership(r.Context(), store.CreateMembershipParams{
			PersonID: person.ID, OrganizationID: oc.orgID, Role: "player",
		}); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, personDTO(person))
}

func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
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
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
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
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
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
