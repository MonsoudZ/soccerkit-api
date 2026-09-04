package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/google/uuid"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// Guardianships link a parent to their child, and only their child.
//
// The table has existed since the first migration with no endpoints on it, which left
// the parent role meaning "a member of this club who is not staff" -- and that is not a
// useful thing for it to mean. personVisibleTo now reads these rows to decide what a
// parent may see, so this is what makes the role narrower than the organization: a
// parent with no guardianship sees nobody, and one with two sees two children.

type addGuardianRequest struct {
	PersonID string `json:"personId"`
}

// handleAddGuardian records that one person is a guardian of another.
//
// Staff only, and both ends must already be visible in the organization. That second
// half is the important one: without it this would be a way to attach yourself to any
// child id in the database and then read them through the parent rule, which is exactly
// the disclosure personVisibleTo exists to prevent.
func (s *Server) handleAddGuardian(w http.ResponseWriter, r *http.Request) {
	child, guardian, oc, err := s.guardianPair(r, "")
	if err != nil {
		writeError(w, err)
		return
	}
	_ = oc
	// ON CONFLICT DO NOTHING with RETURNING gives back no row when the link already
	// exists, which pgx reports as ErrNoRows. That is success, not failure: recording a
	// guardianship twice is what a client retrying a flaky request does, and answering
	// 500 would make the retry look like a server fault it should keep retrying.
	_, err = s.store.CreateGuardianship(r.Context(), store.CreateGuardianshipParams{
		GuardianPersonID: guardian, ChildPersonID: child,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"childPersonId": child, "guardianPersonId": guardian,
	})
}

// handleRemoveGuardian unlinks a guardian from a child. Idempotent: the delete affects
// nothing when the link is already gone, and a caller retrying a flaky request should
// not get a 404 for having succeeded the first time.
func (s *Server) handleRemoveGuardian(w http.ResponseWriter, r *http.Request) {
	child, guardian, _, err := s.guardianPair(r, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteGuardianship(r.Context(), store.DeleteGuardianshipParams{
		GuardianPersonID: guardian, ChildPersonID: child,
	}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleListGuardians lists who is recorded as a guardian of this child. Gated by the
// ordinary person rules, so staff see it, a parent sees it for their own children, and
// nobody else can ask.
func (s *Server) handleListGuardians(w http.ResponseWriter, r *http.Request) {
	childID, _, err := s.visiblePersonFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListGuardiansForChild(r.Context(), childID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Person, len(rows))
	for i, p := range rows {
		out[i] = personDTO(p)
	}
	writeJSON(w, http.StatusOK, out)
}

// guardianPair resolves the {id} child and the guardian -- from the path when pathKey is
// set, otherwise from the request body -- and checks the caller may link them.
//
// Both ends go through personVisibleTo, and the caller must be staff. A parent may not
// enrol themselves as guardian of another child, which would otherwise be a one-call
// escalation from "sees my own children" to "sees whoever I claim".
func (s *Server) guardianPair(r *http.Request, pathKey string) (child, guardian uuid.UUID, oc orgContext, err error) {
	childID, orgCtx, err := s.visiblePersonFromPath(r)
	if err != nil {
		return uuid.Nil, uuid.Nil, orgContext{}, err
	}
	if !orgCtx.isStaff() {
		return uuid.Nil, uuid.Nil, orgContext{}, errForbidden("only staff can change a child's guardians")
	}

	var guardianID uuid.UUID
	if pathKey != "" {
		guardianID, err = pathUUID(r, pathKey)
		if err != nil {
			return uuid.Nil, uuid.Nil, orgContext{}, err
		}
	} else {
		var req addGuardianRequest
		if err := decodeJSON(r, &req); err != nil {
			return uuid.Nil, uuid.Nil, orgContext{}, err
		}
		guardianID, err = parseUUIDParam(req.PersonID, "personId")
		if err != nil {
			return uuid.Nil, uuid.Nil, orgContext{}, err
		}
	}
	if guardianID == childID {
		return uuid.Nil, uuid.Nil, orgContext{}, errValidation("a person cannot be their own guardian")
	}
	if err := s.personVisibleTo(r.Context(), orgCtx, guardianID); err != nil {
		return uuid.Nil, uuid.Nil, orgContext{}, err
	}
	return childID, guardianID, orgCtx, nil
}
