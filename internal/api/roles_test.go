package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// doIn is `do` for a caller who belongs to more than one organization: it sets
// X-Organization-ID so the request acts in the club rather than in the personal org the
// caller got at sign-in, which is what resolveOrg would otherwise pick.
func doIn(t *testing.T, method, path, token, orgID string, payload any) resp {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, testServer.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Organization-ID", orgID)
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// orgOf returns the organization the token's owner was provisioned with at sign-in.
func orgOf(t *testing.T, token string) string {
	t.Helper()
	me := do(t, http.MethodGet, "/api/v1/me", token, nil)
	if me.status != http.StatusOK {
		t.Fatalf("me: %d %s", me.status, me.raw)
	}
	memberships := me.body["memberships"].([]any)
	if len(memberships) == 0 {
		t.Fatal("no memberships")
	}
	return memberships[0].(map[string]any)["organizationId"].(string)
}

// joinOrg is the only way into an organization now: an invitation the invitee answers.
// Tests that just need someone in a club use this rather than restating the two calls.
func joinOrg(t *testing.T, inviter, orgID, invitee, email string, roles ...string) {
	t.Helper()
	inv := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", inviter,
		map[string]any{"email": email, "roles": roles})
	if inv.status != http.StatusCreated {
		t.Fatalf("invite %s: %d %s", email, inv.status, inv.raw)
	}
	accept := do(t, http.MethodPost, "/api/v1/invitations/"+inv.body["id"].(string)+"/accept", invitee, nil)
	if accept.status != http.StatusOK {
		t.Fatalf("accept for %s: %d %s", email, accept.status, accept.raw)
	}
}

// club builds the fixture every test below needs: a coach with an organization, an
// account-holding parent added to it, and two athletes with one of them linked to the
// parent. Until member management existed this arrangement could not be made at all --
// parent and player were grantable only to people with no login -- which is why nothing
// had ever exercised what a non-staff member can see.
type club struct {
	coach, parent    string
	orgID            string
	childID, otherID string
	parentID, teamID string
}

func newClub(t *testing.T, prefix string) club {
	t.Helper()
	coach, _ := signInCoach(t, prefix+"coach@e.com")
	parent, parentID := signInCoach(t, prefix+"parent@e.com")
	orgID := orgOf(t, coach)

	joinOrg(t, coach, orgID, parent, prefix+"parent@e.com", "parent")

	mk := func(name, notes string) string {
		p := do(t, http.MethodPost, "/api/v1/persons", coach, map[string]any{
			"displayName": name, "role": "player", "medicalNotes": notes,
			"birthdate": "2014-03-01",
		})
		if p.status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, p.status, p.raw)
		}
		return p.body["id"].(string)
	}
	childID := mk("My Child", "peanut allergy")
	otherID := mk("Another Child", "asthma")

	link := do(t, http.MethodPost, "/api/v1/persons/"+childID+"/guardians", coach,
		map[string]any{"personId": parentID})
	if link.status != http.StatusCreated {
		t.Fatalf("link guardian: %d %s", link.status, link.raw)
	}

	team := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U12"})
	teamID := team.body["id"].(string)
	for _, id := range []string{childID, otherID} {
		if r := do(t, http.MethodPost, "/api/v1/teams/"+teamID+"/roster", coach,
			map[string]any{"personId": id}); r.status != http.StatusCreated {
			t.Fatalf("roster %s: %d %s", id, r.status, r.raw)
		}
	}
	return club{coach: coach, parent: parent, orgID: orgID,
		childID: childID, otherID: otherID, parentID: parentID, teamID: teamID}
}

// TestParentSeesOnlyTheirOwnChild is the reason the role gate exists.
//
// personVisibleTo used to ask only whether the subject was in the caller's organization,
// never what the caller was, so every member could read every athlete: birthdate,
// contact details and medical notes. It was unreachable only because parent could not be
// granted to anyone who could log in -- which member management has just changed.
func TestParentSeesOnlyTheirOwnChild(t *testing.T) {
	resetDB(t)
	c := newClub(t, "pv")

	mine := doIn(t, http.MethodGet, "/api/v1/persons/"+c.childID, c.parent, c.orgID, nil)
	if mine.status != http.StatusOK {
		t.Fatalf("a parent must see their own child: %d %s", mine.status, mine.raw)
	}
	if mine.body["medicalNotes"] != "peanut allergy" {
		t.Errorf("expected the child's record, got %s", mine.raw)
	}

	other := doIn(t, http.MethodGet, "/api/v1/persons/"+c.otherID, c.parent, c.orgID, nil)
	if other.status != http.StatusNotFound {
		t.Fatalf("a parent must not read another family's child: %d %s", other.status, other.raw)
	}
	if bytes.Contains(other.raw, []byte("asthma")) {
		t.Errorf("the response leaked another child's medical notes: %s", other.raw)
	}

	// The same gate on the other person-keyed reads.
	for _, path := range []string{"/instances", "/aggregate", "/guardians"} {
		r := doIn(t, http.MethodGet, "/api/v1/persons/"+c.otherID+path, c.parent, c.orgID, nil)
		if r.status != http.StatusNotFound {
			t.Errorf("%s should be 404 for another family's child, got %d %s", path, r.status, r.raw)
		}
	}
}

// TestParentSeesOnlyTheirOwnChildOnARoster covers the easier half of the same
// disclosure: the roster carries every athlete's name, email and birthdate, so one GET
// returned what personVisibleTo refuses one id at a time.
func TestParentSeesOnlyTheirOwnChildOnARoster(t *testing.T) {
	resetDB(t)
	c := newClub(t, "rv")

	staff := do(t, http.MethodGet, "/api/v1/teams/"+c.teamID, c.coach, nil)
	if roster := staff.body["roster"].([]any); len(roster) != 2 {
		t.Fatalf("staff should see the squad, got %d", len(roster))
	}

	seen := doIn(t, http.MethodGet, "/api/v1/teams/"+c.teamID, c.parent, c.orgID, nil)
	if seen.status != http.StatusOK {
		t.Fatalf("a parent may open their child's team: %d %s", seen.status, seen.raw)
	}
	roster := seen.body["roster"].([]any)
	if len(roster) != 1 {
		t.Fatalf("a parent should see one entry, their own child; got %d: %s", len(roster), seen.raw)
	}
	if roster[0].(map[string]any)["personId"] != c.childID {
		t.Errorf("wrong entry: %v", roster[0])
	}
	// The squad size is not a disclosure about anyone in it.
	if count := seen.body["team"].(map[string]any)["activeRosterCount"].(float64); count != 2 {
		t.Errorf("the count should be the squad's, got %v", count)
	}
}

// TestMemberManagementGuards covers the rules that keep granting from being an
// escalation and the org from being stranded.
func TestMemberManagementGuards(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "mgadmin@e.com")
	orgID := orgOf(t, admin)
	director, directorID := signInCoach(t, "mgdirector@e.com")
	outsider, _ := signInCoach(t, "mgoutsider@e.com")

	// The admin appoints a director.
	joinOrg(t, admin, orgID, director, "mgdirector@e.com", "director")

	// manageOrg in the permission matrix is admin alone. A director standardizes
	// templates and sees every team; deciding who is in the club is not theirs.
	t.Run("a director cannot manage members at all", func(t *testing.T) {
		me := do(t, http.MethodGet, "/api/v1/me", admin, nil)
		adminID := me.body["person"].(map[string]any)["id"].(string)

		invite := doIn(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", director, orgID,
			map[string]any{"email": "mgoutsider@e.com", "roles": []string{"coach"}})
		if invite.status != http.StatusForbidden {
			t.Errorf("inviting: expected 403, got %d %s", invite.status, invite.raw)
		}
		demote := doIn(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+adminID,
			director, orgID, map[string]any{"roles": []string{"coach"}})
		if demote.status != http.StatusForbidden {
			t.Errorf("demoting: expected 403, got %d %s", demote.status, demote.raw)
		}
		remove := doIn(t, http.MethodDelete, "/api/v1/organizations/"+orgID+"/members/"+adminID,
			director, orgID, nil)
		if remove.status != http.StatusForbidden {
			t.Errorf("removing: expected 403, got %d %s", remove.status, remove.raw)
		}
	})

	t.Run("the last admin cannot be demoted", func(t *testing.T) {
		me := do(t, http.MethodGet, "/api/v1/me", admin, nil)
		adminID := me.body["person"].(map[string]any)["id"].(string)
		r := do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+adminID,
			admin, map[string]any{"roles": []string{"coach"}})
		if r.status != http.StatusConflict {
			t.Errorf("stranding the org must be refused, got %d %s", r.status, r.raw)
		}
	})

	t.Run("a non-member cannot be patched", func(t *testing.T) {
		r := do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+directorID+"x",
			admin, map[string]any{"roles": []string{"coach"}})
		if r.status != http.StatusBadRequest && r.status != http.StatusNotFound {
			t.Errorf("expected 400/404, got %d %s", r.status, r.raw)
		}
	})

	t.Run("an address with no account can still be invited", func(t *testing.T) {
		// The old direct-add answered 404 here, which made it a probe for who had signed
		// up. An invitation does not need the account to exist yet -- it waits.
		r := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
			map[string]any{"email": "nobody-yet@e.com", "roles": []string{"coach"}})
		if r.status != http.StatusCreated {
			t.Errorf("expected 201, got %d %s", r.status, r.raw)
		}
	})

	t.Run("inviting an existing member is a conflict", func(t *testing.T) {
		r := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
			map[string]any{"email": "mgdirector@e.com", "roles": []string{"coach"}})
		if r.status != http.StatusConflict {
			t.Errorf("expected 409, got %d %s", r.status, r.raw)
		}
	})

	t.Run("an unknown role is rejected", func(t *testing.T) {
		r := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
			map[string]any{"email": "mgoutsider@e.com", "roles": []string{"superuser"}})
		if r.status != http.StatusBadRequest {
			t.Errorf("expected 400, got %d %s", r.status, r.raw)
		}
	})

	t.Run("an outsider cannot list or manage members", func(t *testing.T) {
		if r := do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", outsider, nil); r.status == http.StatusOK {
			t.Errorf("an outsider listed a club's members: %s", r.raw)
		}
	})
}

// TestCoachCannotManageMembers pins the narrower gate. A coach runs training; appointing
// another coach, or removing the director who appointed them, is a different authority.
func TestCoachCannotManageMembers(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "ccadmin@e.com")
	orgID := orgOf(t, admin)
	coach, _ := signInCoach(t, "ccplain@e.com")
	signInCoach(t, "ccspare@e.com")

	joinOrg(t, admin, orgID, coach, "ccplain@e.com", "coach")
	// Staff, so they may read the directory.
	if r := doIn(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", coach, orgID, nil); r.status != http.StatusOK {
		t.Errorf("a coach may list members: %d %s", r.status, r.raw)
	}
	// But not change it.
	r := doIn(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", coach, orgID,
		map[string]any{"email": "ccspare@e.com", "roles": []string{"coach"}})
	if r.status != http.StatusForbidden {
		t.Errorf("a coach must not invite members, got %d %s", r.status, r.raw)
	}
}

// TestParentCannotClaimAnotherChild closes the escalation the guardianship rule invites:
// if a parent could record themselves as a guardian, the parent gate would be one call
// away from seeing anyone.
func TestParentCannotClaimAnotherChild(t *testing.T) {
	resetDB(t)
	c := newClub(t, "pc")

	r := doIn(t, http.MethodPost, "/api/v1/persons/"+c.otherID+"/guardians", c.parent, c.orgID,
		map[string]any{"personId": c.parentID})
	if r.status == http.StatusCreated {
		t.Fatalf("a parent enrolled themselves as guardian of another child: %s", r.raw)
	}
	// And the child stays unreadable.
	after := doIn(t, http.MethodGet, "/api/v1/persons/"+c.otherID, c.parent, c.orgID, nil)
	if after.status != http.StatusNotFound {
		t.Errorf("expected 404 after the failed claim, got %d %s", after.status, after.raw)
	}
}

// TestRemovingAGuardianRevokesTheirAccess — the gate reads live rows, so unlinking a
// guardian takes their access with it rather than leaving a stale grant.
func TestRemovingAGuardianRevokesTheirAccess(t *testing.T) {
	resetDB(t)
	c := newClub(t, "rg")

	if r := doIn(t, http.MethodGet, "/api/v1/persons/"+c.childID, c.parent, c.orgID, nil); r.status != http.StatusOK {
		t.Fatalf("precondition: parent sees their child, got %d", r.status)
	}
	if r := do(t, http.MethodDelete,
		"/api/v1/persons/"+c.childID+"/guardians/"+c.parentID, c.coach, nil); r.status != http.StatusOK {
		t.Fatalf("unlink: %d %s", r.status, r.raw)
	}
	after := doIn(t, http.MethodGet, "/api/v1/persons/"+c.childID, c.parent, c.orgID, nil)
	if after.status != http.StatusNotFound {
		t.Errorf("access should be gone with the guardianship, got %d %s", after.status, after.raw)
	}
}

// TestReLinkingAGuardianIsIdempotent — CreateGuardianship is ON CONFLICT DO NOTHING with
// RETURNING, so a repeat gives back no row, which pgx reports as ErrNoRows. Recording the
// same guardianship twice is what a client retrying a dropped response does, and a 500
// would tell it to keep retrying something that already worked.
func TestReLinkingAGuardianIsIdempotent(t *testing.T) {
	resetDB(t)
	c := newClub(t, "idem")

	again := do(t, http.MethodPost, "/api/v1/persons/"+c.childID+"/guardians", c.coach,
		map[string]any{"personId": c.parentID})
	if again.status != http.StatusCreated {
		t.Fatalf("re-linking an existing guardian should succeed, got %d %s", again.status, again.raw)
	}
}

// TestLastAdminCannotBeRemovedFromAnOrphanedClub reaches the guard the owner rule
// normally hides.
//
// While an organization has an owner, the owner rule answers first and keeps their admin
// role, so there is always one admin and the last-admin check never runs. It becomes the
// only protection once owner_person_id is NULL -- which migration 0007 deliberately
// allows, since deleting an owner's account sets it null and orphans the club rather
// than destroying it. An orphaned club whose remaining admin steps down has nobody who
// can manage members and no endpoint that can appoint one.
func TestLastAdminCannotBeRemovedFromAnOrphanedClub(t *testing.T) {
	resetDB(t)
	admin, adminID := signInCoach(t, "orphan-admin@e.com")
	orgID := orgOf(t, admin)

	// Orphan the club, the way losing its owner would.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE organizations SET owner_person_id = NULL WHERE id = $1`, orgID); err != nil {
		t.Fatalf("orphan org: %v", err)
	}

	demote := do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+adminID,
		admin, map[string]any{"roles": []string{"coach"}})
	if demote.status != http.StatusConflict {
		t.Fatalf("the last admin of an orphaned club must not step down, got %d %s",
			demote.status, demote.raw)
	}
	remove := do(t, http.MethodDelete, "/api/v1/organizations/"+orgID+"/members/"+adminID, admin, nil)
	if remove.status != http.StatusConflict {
		t.Fatalf("nor be removed, got %d %s", remove.status, remove.raw)
	}

	// With a second admin in place, stepping down is allowed.
	second, _ := signInCoach(t, "orphan-second@e.com")
	joinOrg(t, admin, orgID, second, "orphan-second@e.com", "admin")
	if r := do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+adminID,
		admin, map[string]any{"roles": []string{"coach"}}); r.status != http.StatusOK {
		t.Fatalf("with another admin in place this is allowed, got %d %s", r.status, r.raw)
	}
}

// TestParentCannotLinkGuardiansEvenForTheirOwnChild isolates the staff gate on
// guardianship writes.
//
// TestParentCannotClaimAnotherChild does not reach it: a parent cannot see another
// family's child, so the visibility check refuses first and the staff check is never
// consulted. The reachable case is a parent acting on people they *can* see -- their own
// children -- where only the staff gate stands between them and rewriting who counts as
// a guardian.
func TestParentCannotLinkGuardiansEvenForTheirOwnChild(t *testing.T) {
	resetDB(t)
	c := newClub(t, "plg")

	// A second child of the same parent, so both ends are visible to them.
	second := do(t, http.MethodPost, "/api/v1/persons", c.coach, map[string]any{
		"displayName": "Second Child", "role": "player",
	})
	secondID := second.body["id"].(string)
	if r := do(t, http.MethodPost, "/api/v1/persons/"+secondID+"/guardians", c.coach,
		map[string]any{"personId": c.parentID}); r.status != http.StatusCreated {
		t.Fatalf("link second child: %d %s", r.status, r.raw)
	}
	if r := doIn(t, http.MethodGet, "/api/v1/persons/"+secondID, c.parent, c.orgID, nil); r.status != http.StatusOK {
		t.Fatalf("precondition: the parent can see both children, got %d", r.status)
	}

	r := doIn(t, http.MethodPost, "/api/v1/persons/"+c.childID+"/guardians", c.parent, c.orgID,
		map[string]any{"personId": secondID})
	if r.status != http.StatusForbidden {
		t.Fatalf("only staff may change a child's guardians, got %d %s", r.status, r.raw)
	}
}

// TestTheGrantCeilingHoldsOnTheOwnerPath is what keeps checkGrantableRoles load-bearing
// now that member management is admin-only.
//
// An owner is let through requireMemberManager whatever membership they hold, so that a
// role change cannot lock someone out of the club they own. That is the only way a
// caller below admin reaches the granting code, and the rank ceiling is the only thing
// between them and appointing themselves one.
//
// No endpoint produces that state: wouldStrandOrg refuses to strip an owner's admin role
// or remove them, on purpose. The state is still reachable in the database — nothing
// about owner_person_id requires the owner to be an admin, and an ownership transfer is
// the obvious next endpoint that would create it — so the setup here is SQL rather than
// an API call. A guard for a state the schema permits is worth keeping and worth
// testing; a guard that has never been executed is neither.
func TestTheGrantCeilingHoldsOnTheOwnerPath(t *testing.T) {
	resetDB(t)
	owner, ownerID := signInCoach(t, "ceiling-owner@e.com")
	orgID := orgOf(t, owner)
	signInCoach(t, "ceiling-outsider@e.com")

	// The owner holds only coach. Reachable in the schema, not through the API.
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM memberships WHERE person_id = $1 AND organization_id = $2 AND role <> 'coach'`,
		ownerID, orgID); err != nil {
		t.Fatalf("demote the owner: %v", err)
	}

	// Still the owner, so still past requireMemberManager — but a coach's ceiling.
	invite := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", owner,
		map[string]any{"email": "ceiling-outsider@e.com", "roles": []string{"admin"}})
	if invite.status != http.StatusForbidden {
		t.Fatalf("an owner holding only coach must not grant admin, got %d %s", invite.status, invite.raw)
	}
	ok := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", owner,
		map[string]any{"email": "ceiling-outsider@e.com", "roles": []string{"coach"}})
	if ok.status != http.StatusCreated {
		t.Errorf("but may still grant at their own level, got %d %s", ok.status, ok.raw)
	}
}
