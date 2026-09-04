package api_test

import (
	"context"
	"net/http"
	"testing"
)

// invitationsFor returns the caller's pending invitations.
func invitationsFor(t *testing.T, token string) []any {
	t.Helper()
	r := do(t, http.MethodGet, "/api/v1/me/invitations", token, nil)
	if r.status != http.StatusOK {
		t.Fatalf("list my invitations: %d %s", r.status, r.raw)
	}
	return r.arr()
}

// TestInvitationRoundTrip is the flow the feature exists for: an offer, a look at it,
// and a yes that is what actually creates the membership.
func TestInvitationRoundTrip(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "inv-admin@e.com")
	orgID := orgOf(t, admin)
	invitee, inviteeID := signInCoach(t, "inv-coach@e.com")

	created := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "inv-coach@e.com", "roles": []string{"coach"}})
	if created.status != http.StatusCreated {
		t.Fatalf("invite: %d %s", created.status, created.raw)
	}
	inviteID := created.body["id"].(string)

	// Nothing has been granted yet -- that is the whole point of the change.
	members := do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", admin, nil)
	if len(members.arr()) != 1 {
		t.Fatalf("an invitation must not add anyone; members=%s", members.raw)
	}

	// The invitee sees it, with the club's name on it.
	mine := invitationsFor(t, invitee)
	if len(mine) != 1 {
		t.Fatalf("the invitee should see one invitation, got %s", mine)
	}
	if name, _ := mine[0].(map[string]any)["organizationName"].(string); name == "" {
		t.Errorf("an invitation without the club's name says nothing: %v", mine[0])
	}

	accept := do(t, http.MethodPost, "/api/v1/invitations/"+inviteID+"/accept", invitee, nil)
	if accept.status != http.StatusOK {
		t.Fatalf("accept: %d %s", accept.status, accept.raw)
	}

	after := do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", admin, nil)
	found := false
	for _, m := range after.arr() {
		row := m.(map[string]any)
		if row["personId"] == inviteeID {
			found = true
			roles := row["roles"].([]any)
			if len(roles) != 1 || roles[0] != "coach" {
				t.Errorf("expected exactly the invited role, got %v", roles)
			}
		}
	}
	if !found {
		t.Errorf("accepting did not create the membership: %s", after.raw)
	}
	// And it is no longer outstanding on either side.
	if left := invitationsFor(t, invitee); len(left) != 0 {
		t.Errorf("an answered invitation should leave the invitee's list, got %v", left)
	}
	pending := do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/invitations", admin, nil)
	if len(pending.arr()) != 0 {
		t.Errorf("an answered invitation should leave the club's list, got %s", pending.raw)
	}
}

// TestOnlyTheAddressedAccountCanAnswer is the authentication the flow rests on. There is
// no token, so the invitation's whole protection is that it matches the verified address
// on the caller's account.
func TestOnlyTheAddressedAccountCanAnswer(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "own-admin@e.com")
	orgID := orgOf(t, admin)
	signInCoach(t, "own-invitee@e.com")
	stranger, _ := signInCoach(t, "own-stranger@e.com")

	created := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "own-invitee@e.com", "roles": []string{"director"}})
	inviteID := created.body["id"].(string)

	// Someone else holding the id learns nothing and gains nothing.
	if seen := invitationsFor(t, stranger); len(seen) != 0 {
		t.Errorf("a stranger should see no invitations, got %v", seen)
	}
	steal := do(t, http.MethodPost, "/api/v1/invitations/"+inviteID+"/accept", stranger, nil)
	if steal.status != http.StatusNotFound {
		t.Fatalf("an invitation addressed elsewhere must 404, got %d %s", steal.status, steal.raw)
	}
	members := do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", admin, nil)
	if len(members.arr()) != 1 {
		t.Errorf("the stranger joined anyway: %s", members.raw)
	}
}

// TestInvitationCannotBeAnsweredTwice pins the concurrency guard. The status test lives
// inside the UPDATE, so a second accept matches no row rather than granting the roles
// again -- which matters because accept is a POST a client may well retry.
func TestInvitationCannotBeAnsweredTwice(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "twice-admin@e.com")
	orgID := orgOf(t, admin)
	invitee, _ := signInCoach(t, "twice-invitee@e.com")

	created := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "twice-invitee@e.com", "roles": []string{"coach"}})
	inviteID := created.body["id"].(string)

	if r := do(t, http.MethodPost, "/api/v1/invitations/"+inviteID+"/accept", invitee, nil); r.status != http.StatusOK {
		t.Fatalf("first accept: %d %s", r.status, r.raw)
	}
	again := do(t, http.MethodPost, "/api/v1/invitations/"+inviteID+"/accept", invitee, nil)
	if again.status != http.StatusConflict {
		t.Errorf("a second accept should be refused, got %d %s", again.status, again.raw)
	}
	decline := do(t, http.MethodPost, "/api/v1/invitations/"+inviteID+"/decline", invitee, nil)
	if decline.status != http.StatusConflict {
		t.Errorf("an answered invitation cannot then be declined, got %d %s", decline.status, decline.raw)
	}
}

// TestDeclinedAndRevokedInvitationsGrantNothing covers the two ways an offer ends
// without a membership, and that either frees the address to be invited again.
func TestDeclinedAndRevokedInvitationsGrantNothing(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "end-admin@e.com")
	orgID := orgOf(t, admin)
	invitee, _ := signInCoach(t, "end-invitee@e.com")

	first := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "end-invitee@e.com", "roles": []string{"coach"}})
	// A second live invitation to the same address is refused while one is pending.
	if dup := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "end-invitee@e.com", "roles": []string{"director"}}); dup.status != http.StatusConflict {
		t.Errorf("a second pending invitation should conflict, got %d %s", dup.status, dup.raw)
	}
	if r := do(t, http.MethodPost, "/api/v1/invitations/"+first.body["id"].(string)+"/decline",
		invitee, nil); r.status != http.StatusOK {
		t.Fatalf("decline: %d %s", r.status, r.raw)
	}

	// Declining frees the address, so the club can try again.
	second := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "end-invitee@e.com", "roles": []string{"coach"}})
	if second.status != http.StatusCreated {
		t.Fatalf("re-inviting after a decline: %d %s", second.status, second.raw)
	}
	secondID := second.body["id"].(string)
	if r := do(t, http.MethodDelete,
		"/api/v1/organizations/"+orgID+"/invitations/"+secondID, admin, nil); r.status != http.StatusOK {
		t.Fatalf("revoke: %d %s", r.status, r.raw)
	}
	// A revoked invitation cannot be accepted.
	if r := do(t, http.MethodPost, "/api/v1/invitations/"+secondID+"/accept", invitee, nil); r.status != http.StatusConflict {
		t.Errorf("a revoked invitation must not be acceptable, got %d %s", r.status, r.raw)
	}
	members := do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", admin, nil)
	if len(members.arr()) != 1 {
		t.Errorf("nobody should have joined: %s", members.raw)
	}
}

// TestExpiredInvitationCannotBeAccepted — the expiry test is in the UPDATE alongside the
// status test, so a row that ages out between the read and the write cannot slip through.
func TestExpiredInvitationCannotBeAccepted(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "exp-admin@e.com")
	orgID := orgOf(t, admin)
	invitee, _ := signInCoach(t, "exp-invitee@e.com")

	created := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "exp-invitee@e.com", "roles": []string{"coach"}})
	inviteID := created.body["id"].(string)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE invitations SET expires_at = now() - interval '1 day' WHERE id = $1`, inviteID); err != nil {
		t.Fatalf("age the invitation: %v", err)
	}

	if seen := invitationsFor(t, invitee); len(seen) != 0 {
		t.Errorf("an expired invitation should not be listed, got %v", seen)
	}
	r := do(t, http.MethodPost, "/api/v1/invitations/"+inviteID+"/accept", invitee, nil)
	if r.status != http.StatusConflict {
		t.Errorf("an expired invitation must not be acceptable, got %d %s", r.status, r.raw)
	}
}

// TestInvitationWaitsForAnAccountThatDoesNotExistYet is the property that makes a
// tokenless flow work without a mail path: the offer can be written before its recipient
// has ever signed in, and is there when they do.
func TestInvitationWaitsForAnAccountThatDoesNotExistYet(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "wait-admin@e.com")
	orgID := orgOf(t, admin)

	created := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "wait-newcomer@e.com", "roles": []string{"coach", "parent"}})
	if created.status != http.StatusCreated {
		t.Fatalf("invite a stranger: %d %s", created.status, created.raw)
	}

	// They sign up afterwards, and the invitation is waiting.
	newcomer, _ := signInCoach(t, "wait-newcomer@e.com")
	mine := invitationsFor(t, newcomer)
	if len(mine) != 1 {
		t.Fatalf("the invitation should be waiting, got %v", mine)
	}
	if r := do(t, http.MethodPost, "/api/v1/invitations/"+created.body["id"].(string)+"/accept",
		newcomer, nil); r.status != http.StatusOK {
		t.Fatalf("accept: %d %s", r.status, r.raw)
	}
	members := do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", admin, nil)
	for _, m := range members.arr() {
		row := m.(map[string]any)
		if row["email"] == "wait-newcomer@e.com" {
			if len(row["roles"].([]any)) != 2 {
				t.Errorf("both invited roles should be granted, got %v", row["roles"])
			}
			return
		}
	}
	t.Errorf("the newcomer did not join: %s", members.raw)
}

// TestInvitedAddressIsCaseInsensitive — appleEmail lower-cases what it writes to
// user_accounts, so an invitation typed with capitals has to be folded the same way or
// it is invisible to the account that owns it.
func TestInvitedAddressIsCaseInsensitive(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "case-admin@e.com")
	orgID := orgOf(t, admin)
	invitee, _ := signInCoach(t, "case-invitee@e.com")

	created := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "  Case-Invitee@E.com  ", "roles": []string{"coach"}})
	if created.status != http.StatusCreated {
		t.Fatalf("invite: %d %s", created.status, created.raw)
	}
	if mine := invitationsFor(t, invitee); len(mine) != 1 {
		t.Fatalf("a differently-cased address must still reach its owner, got %v", mine)
	}
}
