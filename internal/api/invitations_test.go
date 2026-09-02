package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The invitation flow: the only way somebody who already has an account joins an
// organization that is not their own.

// invite issues an invitation as the given caller and returns its one-time token.
func invite(t *testing.T, token, orgID string, payload map[string]any) resp {
	t.Helper()
	return doAs(t, http.MethodPost, "/api/v1/invitations", token, orgID, payload)
}

// TestInviteAParentAndRedeemIt is the flow the parent tier stands on, end to end: a
// coach who knows which child this is issues the invitation, the parent redeems it with
// their own Apple account, and comes out the other side able to see their own child and
// nothing else.
func TestInviteAParentAndRedeemIt(t *testing.T) {
	resetDB(t)
	coach, coachPerson := signInCoach(t, "invitecoach@e.com")
	org := orgOwnedBy(t, coachPerson)

	sam := createAthlete(t, coach, "Sam Smith")
	someoneElse := createAthlete(t, coach, "Another Child")

	created := invite(t, coach, org, map[string]any{
		"role": "parent", "childPersonIds": []string{sam}, "note": "Sam's mum",
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create invitation: %d %s", created.status, created.raw)
	}
	token, _ := created.body["token"].(string)
	if !strings.HasPrefix(token, "skinv_") {
		t.Fatalf("expected a prefixed invitation token, got %q", token)
	}
	if created.body["status"] != "pending" || created.body["role"] != "parent" {
		t.Errorf("unexpected invitation: %s", created.raw)
	}
	if kids := created.body["children"].([]any); len(kids) != 1 ||
		kids[0].(map[string]any)["id"] != sam {
		t.Errorf("the invitation should name the child it is about: %s", created.raw)
	}

	// The parent signs in on their own. Apple gives them their own Person and their own
	// personal org; nothing so far connects them to this club.
	parent, parentPerson := signInCoach(t, "sammum@e.com")
	if r := doAs(t, http.MethodGet, "/api/v1/persons/"+sam, parent, org, nil); r.status == http.StatusOK {
		t.Fatal("an uninvited account must not be able to read the club's athletes")
	}

	// What am I being asked to join?
	preview := do(t, http.MethodPost, "/api/v1/invitations/preview", parent, map[string]any{"token": token})
	if preview.status != http.StatusOK {
		t.Fatalf("preview: %d %s", preview.status, preview.raw)
	}
	if preview.body["organizationId"] != org || preview.body["role"] != "parent" {
		t.Errorf("preview should name the club and the role: %s", preview.raw)
	}
	if preview.body["organizationName"] == "" {
		t.Error("preview must name the organization; it is what the person is deciding about")
	}

	accepted := do(t, http.MethodPost, "/api/v1/invitations/accept", parent, map[string]any{"token": token})
	if accepted.status != http.StatusOK {
		t.Fatalf("accept: %d %s", accepted.status, accepted.raw)
	}
	if accepted.body["organizationId"] != org || accepted.body["role"] != "parent" {
		t.Errorf("acceptance should say what was joined: %s", accepted.raw)
	}
	access := accepted.body["access"].(map[string]any)
	if access["scope"] != "own" {
		t.Errorf("a parent's reads are scoped to their household: %s", accepted.raw)
	}
	if roles := access["roles"].([]any); len(roles) != 1 || roles[0] != "parent" {
		t.Errorf("expected exactly the parent role: %v", roles)
	}

	// And now the point of all of it: their own child, and only their own child.
	if r := doAs(t, http.MethodGet, "/api/v1/persons/"+sam, parent, org, nil); r.status != http.StatusOK {
		t.Errorf("the parent must be able to read the child they were invited for: %d %s", r.status, r.raw)
	}
	if r := doAs(t, http.MethodGet, "/api/v1/persons/"+someoneElse, parent, org, nil); r.status != http.StatusNotFound {
		t.Errorf("another family's child must stay invisible: %d %s", r.status, r.raw)
	}
	kids := do(t, http.MethodGet, "/api/v1/me/children", parent, nil)
	if k := kids.arr(); len(k) != 1 || k[0].(map[string]any)["id"] != sam {
		t.Errorf("GET /me/children should be exactly Sam: %s", kids.raw)
	}
	// The membership is the redeemer's, not the id of anyone named in the request.
	if n := countRows(t,
		`SELECT count(*) FROM memberships WHERE person_id = $1 AND organization_id = $2 AND role = 'parent'`,
		parentPerson, org); n != 1 {
		t.Errorf("expected one parent membership for the redeeming account, got %d", n)
	}

	// A link that has been used is used. People forward these.
	if again := do(t, http.MethodPost, "/api/v1/invitations/accept", parent, map[string]any{"token": token}); again.status != http.StatusConflict {
		t.Errorf("a second redemption must be refused, got %d %s", again.status, again.raw)
	}
	list := doAs(t, http.MethodGet, "/api/v1/invitations", coach, org, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list invitations: %d %s", list.status, list.raw)
	}
	rows := list.arr()
	if len(rows) != 1 || rows[0].(map[string]any)["status"] != "accepted" {
		t.Errorf("the club should see what became of its invitation: %s", list.raw)
	}
	// The token exists in exactly one response, ever. Listing invitations must not be a
	// way to read outstanding credentials.
	if strings.Contains(string(list.raw), token) || strings.Contains(string(preview.raw), token) {
		t.Error("an invitation token must never be returned again after it is issued")
	}
	// Nor is it recoverable from the database, which holds only its hash.
	if n := countRows(t, `SELECT count(*) FROM invitations WHERE token_hash = $1`, token); n != 0 {
		t.Error("the raw token must not be stored")
	}
}

func TestInvitationsCannotOutrunTheGrantCeiling(t *testing.T) {
	resetDB(t)
	_, ownerPerson := signInCoach(t, "cclub@e.com")
	org := orgOwnedBy(t, ownerPerson)
	director, directorPerson := signInCoach(t, "cdirector@e.com")
	joinOrgAs(t, directorPerson, org, "director")
	coach, coachPerson := signInCoach(t, "ccoach@e.com")
	joinOrgAs(t, coachPerson, org, "coach")

	// The escalation this rule exists to stop: an invitation you can accept yourself is
	// a grant with an extra step.
	if r := invite(t, director, org, map[string]any{"role": "admin"}); r.status != http.StatusForbidden {
		t.Errorf("a director must not be able to invite an admin: %d %s", r.status, r.raw)
	}
	if r := invite(t, director, org, map[string]any{"role": "coach"}); r.status != http.StatusCreated {
		t.Errorf("a director staffs the club: %d %s", r.status, r.raw)
	}
	// A coach brings in the parents and players of their own athletes, and staffs nobody.
	if r := invite(t, coach, org, map[string]any{"role": "parent"}); r.status != http.StatusCreated {
		t.Errorf("a coach must be able to invite a parent: %d %s", r.status, r.raw)
	}
	for _, role := range []string{"coach", "director", "admin"} {
		if r := invite(t, coach, org, map[string]any{"role": role}); r.status != http.StatusForbidden {
			t.Errorf("a coach must not invite a %s: %d %s", role, r.status, r.raw)
		}
	}
	if r := invite(t, coach, org, map[string]any{"role": "wizard"}); r.status != http.StatusBadRequest {
		t.Errorf("unknown role: %d %s", r.status, r.raw)
	}

	// A parent invites nobody at all.
	parent, parentPerson := signInCoach(t, "cparent@e.com")
	joinOrgAs(t, parentPerson, org, "parent")
	if r := invite(t, parent, org, map[string]any{"role": "player"}); r.status != http.StatusForbidden {
		t.Errorf("a parent holds no invite.send: %d %s", r.status, r.raw)
	}
	if r := doAs(t, http.MethodGet, "/api/v1/invitations", parent, org, nil); r.status != http.StatusForbidden {
		t.Errorf("nor may they read the club's invitations: %d %s", r.status, r.raw)
	}
}

func TestARevokedOrExpiredInvitationIsDead(t *testing.T) {
	resetDB(t)
	coach, coachPerson := signInCoach(t, "revoker@e.com")
	org := orgOwnedBy(t, coachPerson)
	stranger, _ := signInCoach(t, "recipient@e.com")

	revoked := invite(t, coach, org, map[string]any{"role": "parent"})
	revokedToken := revoked.body["token"].(string)
	id := revoked.body["id"].(string)
	if r := doAs(t, http.MethodDelete, "/api/v1/invitations/"+id, coach, org, nil); r.status != http.StatusOK {
		t.Fatalf("revoke: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPost, "/api/v1/invitations/accept", stranger, map[string]any{"token": revokedToken}); r.status != http.StatusConflict {
		t.Errorf("a revoked invitation must not be redeemable: %d %s", r.status, r.raw)
	}
	// Revoking twice is not a second revocation.
	if r := doAs(t, http.MethodDelete, "/api/v1/invitations/"+id, coach, org, nil); r.status != http.StatusConflict {
		t.Errorf("re-revoking should say so: %d %s", r.status, r.raw)
	}

	expired := invite(t, coach, org, map[string]any{"role": "parent"})
	expiredToken := expired.body["token"].(string)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE invitations SET expires_at = now() - interval '1 day' WHERE id = $1`,
		expired.body["id"].(string)); err != nil {
		t.Fatalf("age the invitation: %v", err)
	}
	if r := do(t, http.MethodPost, "/api/v1/invitations/accept", stranger, map[string]any{"token": expiredToken}); r.status != http.StatusConflict {
		t.Errorf("an expired invitation must not be redeemable: %d %s", r.status, r.raw)
	}
	// A token nobody issued says nothing about whether it could have existed.
	if r := do(t, http.MethodPost, "/api/v1/invitations/accept", stranger, map[string]any{"token": "skinv_nope"}); r.status != http.StatusNotFound {
		t.Errorf("an unknown token is a 404: %d %s", r.status, r.raw)
	}
	// Nothing was joined by any of that.
	if n := countRows(t, `SELECT count(*) FROM memberships WHERE organization_id = $1`, org); n != 3 {
		t.Errorf("only the owner's three memberships should exist, found %d", n)
	}
}

func TestAnEmailBoundInvitationOnlyWorksForThatAddress(t *testing.T) {
	resetDB(t)
	coach, coachPerson := signInCoach(t, "binder@e.com")
	org := orgOwnedBy(t, coachPerson)

	// Bound to the address the club knows. A forwarded link is then useless.
	created := invite(t, coach, org, map[string]any{"role": "parent", "email": "Intended@E.com"})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	token := created.body["token"].(string)

	wrong, _ := signInCoach(t, "someoneelse@e.com")
	if r := do(t, http.MethodPost, "/api/v1/invitations/accept", wrong, map[string]any{"token": token}); r.status != http.StatusForbidden {
		t.Errorf("a different address must not redeem it: %d %s", r.status, r.raw)
	}
	// Still pending: a refused attempt must not burn the invitation, or anyone holding
	// the link could deny it to the person it was for.
	if r := do(t, http.MethodPost, "/api/v1/invitations/preview", wrong, map[string]any{"token": token}); r.body["status"] != "pending" {
		t.Errorf("a refused redemption must leave the invitation alone: %s", r.raw)
	}

	// Case and surrounding whitespace are not the point of the check.
	intended, _ := signInCoach(t, "intended@e.com")
	if r := do(t, http.MethodPost, "/api/v1/invitations/accept", intended, map[string]any{"token": token}); r.status != http.StatusOK {
		t.Errorf("the intended address must be able to redeem it: %d %s", r.status, r.raw)
	}
}

func TestInvitationValidationAndRevokeAuthority(t *testing.T) {
	resetDB(t)
	_, ownerPerson := signInCoach(t, "vclub@e.com")
	org := orgOwnedBy(t, ownerPerson)
	director, directorPerson := signInCoach(t, "vdirector@e.com")
	joinOrgAs(t, directorPerson, org, "director")
	coach, coachPerson := signInCoach(t, "vcoach@e.com")
	joinOrgAs(t, coachPerson, org, "coach")

	// Children belong to a parent invitation and to nothing else: attaching them to a
	// coach invitation would be silently doing something other than what was asked.
	athlete := createAthlete(t, director, "An Athlete")
	if r := invite(t, director, org, map[string]any{
		"role": "coach", "childPersonIds": []string{athlete},
	}); r.status != http.StatusBadRequest {
		t.Errorf("children on a non-parent invitation: %d %s", r.status, r.raw)
	}
	// An athlete from outside the club fails in the hands of the person who made the
	// mistake, not the parent who receives the link.
	outsider, outsiderPerson := signInCoach(t, "outsider@e.com")
	stranger := createAthlete(t, outsider, "Somebody Else's Athlete")
	_ = outsiderPerson
	if r := invite(t, director, org, map[string]any{
		"role": "parent", "childPersonIds": []string{stranger},
	}); r.status != http.StatusNotFound {
		t.Errorf("a child outside the club: %d %s", r.status, r.raw)
	}

	// A coach may take back what they sent, and not what somebody else sent.
	mine := invite(t, coach, org, map[string]any{"role": "parent"})
	theirs := invite(t, director, org, map[string]any{"role": "coach"})
	if r := doAs(t, http.MethodDelete, "/api/v1/invitations/"+theirs.body["id"].(string), coach, org, nil); r.status != http.StatusForbidden {
		t.Errorf("a coach must not revoke the director's invitation: %d %s", r.status, r.raw)
	}
	if r := doAs(t, http.MethodDelete, "/api/v1/invitations/"+mine.body["id"].(string), coach, org, nil); r.status != http.StatusOK {
		t.Errorf("a coach may revoke their own: %d %s", r.status, r.raw)
	}
	// Whoever staffs the club cleans up after everyone.
	if r := doAs(t, http.MethodDelete, "/api/v1/invitations/"+theirs.body["id"].(string), director, org, nil); r.status != http.StatusOK {
		t.Errorf("a director may revoke anyone's: %d %s", r.status, r.raw)
	}
	// And an invitation from another organization is not theirs to see or touch.
	otherOrg := orgOwnedBy(t, outsiderPerson)
	elsewhere := invite(t, outsider, otherOrg, map[string]any{"role": "parent"})
	if r := doAs(t, http.MethodDelete, "/api/v1/invitations/"+elsewhere.body["id"].(string), director, org, nil); r.status != http.StatusNotFound {
		t.Errorf("another club's invitation: %d %s", r.status, r.raw)
	}
}

// A coach who joins by invitation gets the coach surface, which is the other half of
// what the flow is for: the club tiers were unreachable without it.
func TestAnInvitedCoachCanRunTheirTeams(t *testing.T) {
	resetDB(t)
	director, directorPerson := signInCoach(t, "staffing@e.com")
	org := orgOwnedBy(t, directorPerson)

	created := invite(t, director, org, map[string]any{"role": "coach"})
	if created.status != http.StatusCreated {
		t.Fatalf("invite a coach: %d %s", created.status, created.raw)
	}
	newCoach, newCoachPerson := signInCoach(t, "newcoach@e.com")
	if r := do(t, http.MethodPost, "/api/v1/invitations/accept", newCoach, map[string]any{
		"token": created.body["token"].(string),
	}); r.status != http.StatusOK {
		t.Fatalf("accept: %d %s", r.status, r.raw)
	}

	team := doAs(t, http.MethodPost, "/api/v1/teams", newCoach, org, map[string]any{"name": "U12 Green"})
	if team.status != http.StatusCreated {
		t.Fatalf("an invited coach must be able to create a team: %d %s", team.status, team.raw)
	}
	if team.body["organizationId"] != org {
		t.Errorf("the team belongs to the club they joined, not their own org: %s", team.raw)
	}
	// They joined a club; they did not take it over.
	if r := doAs(t, http.MethodPost, "/api/v1/members", newCoach, org, map[string]any{
		"personId": newCoachPerson, "role": "admin",
	}); r.status != http.StatusForbidden {
		t.Errorf("an invited coach must not be able to staff the club: %d %s", r.status, r.raw)
	}
	// Their own personal org is untouched and still theirs.
	if r := do(t, http.MethodGet, "/api/v1/me/access", newCoach, nil); r.status != http.StatusOK {
		t.Fatalf("me/access: %d %s", r.status, r.raw)
	} else if r.body["organizationId"] == org {
		t.Error("joining a club must not repoint their default organization")
	}
}
