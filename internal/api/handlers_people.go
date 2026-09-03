package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
// no login, so it deliberately cannot mint the privileged admin/director/coach roles.
var personRoles = map[string]bool{"player": true, "parent": true}

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
	role := "player"
	if req.Role != nil {
		role = *req.Role
	}
	if !personRoles[role] {
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
	id, _, err := s.visiblePersonFromPath(r)
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
	id, _, err := s.visiblePersonFromPath(r)
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

// visiblePersonFromPath resolves the {id} path parameter to a Person the caller's
// organization is allowed to see. Every read keyed on a person id goes through here —
// these endpoints return birthdate, contact details and medical notes, so an
// unauthenticated-by-org read is a disclosure of a minor's PII.
func (s *Server) visiblePersonFromPath(r *http.Request) (uuid.UUID, orgContext, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return uuid.Nil, orgContext{}, err
	}
	oc, err := s.resolveOrg(r)
	if err != nil {
		return uuid.Nil, orgContext{}, err
	}
	if err := s.personVisibleTo(r.Context(), oc, id); err != nil {
		return uuid.Nil, orgContext{}, err
	}
	return id, oc, nil
}

// updatablePersonFields is the set PATCH /persons/{id} accepts.
var updatablePersonFields = map[string]bool{
	"displayName": true, "givenName": true, "familyName": true, "birthdate": true,
	"email": true, "phone": true, "emergencyContactName": true,
	"emergencyContactPhone": true, "medicalNotes": true,
}

// handleUpdatePerson edits an athlete's details, or the caller's own. Persons could be
// created and read but never edited, so a birthdate typed wrong at registration stayed
// wrong, and a changed phone number or a new allergy had no way in at all.
//
// The fields split in two, and the split is the sync contract rather than a preference:
//
//   - displayName, the emergency contact pair and medicalNotes are what SyncUpsertPerson
//     projects out of the app's payload, so each is written twice -- column and payload --
//     and the edit reaches the phone.
//   - givenName, familyName, birthdate, email and phone are REST's alone. No push writes
//     them and SyncTombstonePerson deliberately spares them, so they need no propagation,
//     and inventing keys for them in the payload would put fields in the app's record
//     that the app never wrote and cannot read.
//
// email is contact information, not a credential: sign-in matches on
// user_accounts.email, which comes from a verified Apple claim and is not reachable
// from here. Editing it cannot affect who can sign in as this person.
func (s *Server) handleUpdatePerson(w http.ResponseWriter, r *http.Request) {
	id, err := s.editablePersonFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := decodePatch(r, updatablePersonFields)
	if err != nil {
		writeError(w, err)
		return
	}

	params := store.UpdatePersonParams{ID: id}
	patch := syncPatch{}

	// --- projected into the payload, and so onto the phone ---
	if v, ok := raw["displayName"]; ok {
		name, verr := requiredString(v, "displayName")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.DisplayName = &name
		patch.set("name", name) // the app's record calls it "name"
	}
	if v, ok := raw["emergencyContactName"]; ok {
		ecName, verr := optionalString(v, "emergencyContactName")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetEcName, params.EcName = true, ecName
		patch.set("emergencyContactName", syncString(ecName))
	}
	if v, ok := raw["emergencyContactPhone"]; ok {
		ecPhone, verr := optionalString(v, "emergencyContactPhone")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetEcPhone, params.EcPhone = true, ecPhone
		patch.set("emergencyContactPhone", syncString(ecPhone))
	}
	if v, ok := raw["medicalNotes"]; ok {
		medical, verr := optionalString(v, "medicalNotes")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetMedical, params.Medical = true, medical
		patch.set("medicalNotes", syncString(medical))
	}

	// --- REST-only: no payload key, because the app has no such field ---
	if v, ok := raw["givenName"]; ok {
		given, verr := optionalString(v, "givenName")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetGivenName, params.GivenName = true, given
	}
	if v, ok := raw["familyName"]; ok {
		family, verr := optionalString(v, "familyName")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetFamilyName, params.FamilyName = true, family
	}
	if v, ok := raw["birthdate"]; ok {
		bd, verr := optionalDate(v, "birthdate")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetBirthdate, params.Birthdate = true, bd
	}
	if v, ok := raw["email"]; ok {
		email, verr := optionalString(v, "email")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetEmail, params.Email = true, email
	}
	if v, ok := raw["phone"]; ok {
		phone, verr := optionalString(v, "phone")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetPhone, params.Phone = true, phone
	}
	params.PayloadPatch, params.PatchPayload = patch.marshal()

	updated, err := s.store.UpdatePerson(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, personDTO(updated))
}

// editablePersonFromPath resolves {id} to a Person the caller may edit, which is a
// narrower set than the one they may read.
//
// You may always edit yourself. Otherwise a coach may edit a person in their
// organization who holds no user account -- the loginless athletes POST /persons creates
// and a coach is responsible for. Anyone with an account edits their own row: a club has
// several coaches and a director, and letting any of them rewrite another's name,
// contact details or medical notes is an authority nobody granted. It is the same line
// handleCreatePerson draws when it refuses to mint admin, director or coach roles.
func (s *Server) editablePersonFromPath(r *http.Request) (uuid.UUID, error) {
	id, oc, err := s.visiblePersonFromPath(r)
	if err != nil {
		return uuid.Nil, err
	}
	if id == personIDFrom(r.Context()) {
		return id, nil
	}
	if !oc.hasAnyRole("admin", "director", "coach") {
		return uuid.Nil, errForbidden("only coaches can edit people")
	}
	hasAccount, err := s.store.PersonHasUserAccount(r.Context(), id)
	if err != nil {
		return uuid.Nil, err
	}
	if hasAccount {
		return uuid.Nil, errForbidden(
			"that person has their own account, so only they can edit their details")
	}
	return id, nil
}
