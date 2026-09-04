package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

// invitationTTL is how long an invitation stands.
//
// Long enough that a coach can send one on a Friday and have it answered after the
// weekend, short enough that a role granted by someone who has since left the club does
// not sit waiting indefinitely. The roles on an invitation are checked against the
// inviter's rank when it is written, not when it is answered, so the window is also how
// long that authority outlives them.
const invitationTTL = 14 * 24 * time.Hour

// Invitation is an offer of membership, as the club sees it.
type Invitation struct {
	ID             string   `json:"id"`
	OrganizationID string   `json:"organizationId"`
	Email          string   `json:"email"`
	Roles          []string `json:"roles"`
	Status         string   `json:"status"`
	ExpiresAt      string   `json:"expiresAt"`
	CreatedAt      string   `json:"createdAt"`
	InvitedByName  *string  `json:"invitedByName,omitempty"`
	// OrganizationName is filled on the invitee's side, where the club's name is the
	// only thing that makes the offer meaningful.
	OrganizationName *string `json:"organizationName,omitempty"`
}

type createInvitationRequest struct {
	Email string   `json:"email"`
	Roles []string `json:"roles"`
}

// handleCreateInvitation offers membership to an address.
//
// It replaced POST /organizations/{id}/members, which added the person outright. That
// endpoint's own note said the address requirement was standing in for consent until
// there was a way to ask; this is the way to ask, so the direct path is gone rather than
// left beside it. Two doors into an organization, one of which skips the agreement, is
// the same as having no agreement.
//
// The address need not belong to an account yet. An invitation written for someone who
// has not signed up waits for them, and appears the first time they sign in with that
// address -- which is also why this cannot report whether an account exists, and so
// cannot be used to probe for who has signed up.
func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
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
	var req createInvitationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	roles, err := s.checkGrantableRoles(oc, req.Roles)
	if err != nil {
		writeError(w, err)
		return
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		writeError(w, err)
		return
	}
	// Someone already in the club has nothing to accept. Checked here rather than at
	// acceptance so the mistake is reported to whoever made it, while they are looking.
	if personID, err := s.store.GetPersonIDByAccountEmail(r.Context(), email); err == nil {
		existing, err := s.store.ListRolesForPersonInOrg(r.Context(), store.ListRolesForPersonInOrgParams{
			PersonID: personID, OrganizationID: oc.orgID,
		})
		if err != nil {
			writeError(w, err)
			return
		}
		if len(existing) > 0 {
			writeError(w, errConflict("that person is already a member; PATCH their roles instead"))
			return
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}

	invite, err := s.store.CreateInvitation(r.Context(), store.CreateInvitationParams{
		OrganizationID: oc.orgID, Email: email, Roles: roles,
		InvitedByPersonID: &oc.callerID,
		ExpiresAt:         timestamptz(time.Now().Add(invitationTTL)),
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, errConflict(
				"that address already has an invitation to this organization; revoke it first"))
			return
		}
		writeError(w, err)
		return
	}
	// Told, rather than left to discover. This is why the notifier exists: an invitation
	// is addressed to someone who may not have the app open, and /me/invitations only
	// helps a person who already suspects there is something to look at.
	//
	// After the insert and outside any transaction, deliberately. A push for an
	// invitation that failed to save is worse than no push, and the send itself must not
	// be able to fail the request -- Notify queues and returns.
	//
	// An address with no account has nobody to notify, which is the waiting case working
	// as intended: they will see it in /me/invitations at their first sign-in.
	if personID, err := s.store.GetPersonIDByAccountEmail(r.Context(), email); err == nil {
		s.notify(r.Context(), personID, Notification{
			Title: "Invitation to " + org.Name,
			Body:  invitationBody(org.Name, roles),
			Data: map[string]string{
				"type":         "invitation",
				"invitationId": invite.ID.String(),
			},
		})
	}
	writeJSON(w, http.StatusCreated, invitationDTO(invite, nil, nil))
}

// invitationBody is what shows on a lock screen, so it says what is being offered and
// nothing else. The club's name and the roles are the whole of it -- the inviter's name
// would be a third party's information on a device that has not yet accepted anything.
func invitationBody(orgName string, roles []string) string {
	return "You have been invited to join " + orgName + " as " + strings.Join(roles, ", ") + "."
}

// handleListInvitations shows the club what it has outstanding. Staff, matching who may
// read the member list.
func (s *Server) handleListInvitations(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, errForbidden("only staff can list an organization's invitations"))
		return
	}
	rows, err := s.store.ListPendingInvitationsForOrg(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Invitation, len(rows))
	for i, row := range rows {
		out[i] = invitationDTO(store.Invitation{
			ID: row.ID, OrganizationID: row.OrganizationID, Email: row.Email, Roles: row.Roles,
			Status: row.Status, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		}, row.InvitedByName, nil)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeInvitation withdraws an offer that has not been answered.
func (s *Server) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
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
	inviteID, err := pathUUID(r, "invitationId")
	if err != nil {
		writeError(w, err)
		return
	}
	// Scoped to this organization in the statement, so an id from another club matches
	// nothing rather than being revoked by whoever happens to hold it.
	rows, err := s.store.RevokeInvitation(r.Context(), store.RevokeInvitationParams{
		ID: inviteID, OrganizationID: oc.orgID,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == 0 {
		writeError(w, errNotFound("no pending invitation with that id"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleListMyInvitations shows the caller what they have been offered.
//
// Keyed on the address on their account, which came from a verified Apple claim, so this
// is the whole of the authentication an invitation needs: only the person who can sign
// in as that address ever sees it.
func (s *Server) handleListMyInvitations(w http.ResponseWriter, r *http.Request) {
	email, err := s.callerEmail(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListPendingInvitationsForEmail(r.Context(), email)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Invitation, len(rows))
	for i, row := range rows {
		name := row.OrganizationName
		out[i] = invitationDTO(store.Invitation{
			ID: row.ID, OrganizationID: row.OrganizationID, Email: row.Email, Roles: row.Roles,
			Status: row.Status, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		}, nil, &name)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAcceptInvitation is the yes. It grants the roles the invitation names and marks
// it answered, in one transaction.
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	s.respondToInvitation(w, r, "accepted")
}

// handleDeclineInvitation is the no. It exists so that declining is a recorded answer
// rather than an invitation left to rot: the club sees it leave their pending list, and
// the address is free to be invited again.
func (s *Server) handleDeclineInvitation(w http.ResponseWriter, r *http.Request) {
	s.respondToInvitation(w, r, "declined")
}

func (s *Server) respondToInvitation(w http.ResponseWriter, r *http.Request, status string) {
	email, err := s.callerEmail(r)
	if err != nil {
		writeError(w, err)
		return
	}
	inviteID, err := pathUUID(r, "invitationId")
	if err != nil {
		writeError(w, err)
		return
	}
	invite, err := s.store.GetInvitation(r.Context(), inviteID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("invitation not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	// Not addressed to you is 404, not 403: an invitation id is a handle to a fact about
	// somebody else's address, and confirming one exists is a disclosure of its own.
	if invite.Email != email {
		writeError(w, errNotFound("invitation not found"))
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after a successful Commit
	q := s.store.WithTx(tx)

	// The status and expiry tests live inside this UPDATE, so two accepts racing cannot
	// both find it pending and both grant the roles. Whoever loses matches no row.
	rows, err := q.RespondToInvitation(r.Context(), store.RespondToInvitationParams{
		ID: inviteID, Status: status,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == 0 {
		writeError(w, errConflict("that invitation has already been answered, or has expired"))
		return
	}

	if status == "accepted" {
		// CreateMembership is ON CONFLICT DO NOTHING, so a role the caller already holds
		// is left alone rather than duplicated -- accepting is additive, and cannot take
		// away something they were given another way.
		for _, role := range invite.Roles {
			if _, err := q.CreateMembership(r.Context(), store.CreateMembershipParams{
				PersonID: personIDFrom(r.Context()), OrganizationID: invite.OrganizationID, Role: role,
			}); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": invite.ID, "organizationId": invite.OrganizationID,
		"status": status, "roles": invite.Roles,
	})
}

// callerEmail is the verified address on the caller's account, and the only thing an
// invitation is matched against. A Person with no user_account cannot hold one, which is
// right: an athlete with no login has nothing to sign in and accept with.
func (s *Server) callerEmail(r *http.Request) (string, error) {
	account, err := s.store.GetUserAccountByPersonID(r.Context(), personIDFrom(r.Context()))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errForbidden("this account cannot hold invitations")
	} else if err != nil {
		return "", err
	}
	return strings.ToLower(account.Email), nil
}

// normalizeEmail lower-cases and trims an invited address so it matches the form
// appleEmail writes to user_accounts. Without this an invitation to Coach@Example.com
// would be invisible to the account that owns coach@example.com.
func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" {
		return "", errValidation("email is required")
	}
	if at := strings.IndexByte(email, '@'); at <= 0 || at == len(email)-1 || strings.ContainsAny(email, " \t") {
		return "", errValidation("email must be an address")
	}
	return email, nil
}

func invitationDTO(inv store.Invitation, invitedBy, orgName *string) Invitation {
	return Invitation{
		ID: inv.ID.String(), OrganizationID: inv.OrganizationID.String(), Email: inv.Email,
		Roles: inv.Roles, Status: inv.Status,
		ExpiresAt: rfc3339(inv.ExpiresAt), CreatedAt: rfc3339(inv.CreatedAt),
		InvitedByName: invitedBy, OrganizationName: orgName,
	}
}
