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

// The role system end to end: the catalogue a client builds its UI from, the grant and
// revoke pair that changes who may do what, and — the part that matters most — what a
// parent can actually see once they hold a role in someone else's club.

// doAs is `do` with an explicit organization. Everything below acts in a club the
// caller does not own, which is the whole point of the tier: X-Organization-ID is how a
// person who belongs to several orgs says which one this request is about.
func doAs(t *testing.T, method, path, token, orgID string, payload any) resp {
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
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// orgOwnedBy returns the personal org a coach's sign-in created for them.
func orgOwnedBy(t *testing.T, personID string) string {
	t.Helper()
	var org string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id::text FROM organizations WHERE owner_person_id = $1`, personID).Scan(&org); err != nil {
		t.Fatalf("find org owned by %s: %v", personID, err)
	}
	return org
}

// joinOrgAs puts an existing account into somebody else's organization with the given
// roles, by writing the memberships directly.
//
// It is written this way on purpose. POST /members deliberately refuses to enroll a
// person who has no linkage to the org yet — an id is not consent, and accepting one
// would make "grant a role" a way to read a stranger's record. Connecting an account
// that signed in on its own to a club it was invited to is an invitation flow, and
// there isn't one yet; when there is, this helper becomes a call to it.
func joinOrgAs(t *testing.T, personID, orgID string, roles ...string) {
	t.Helper()
	for _, role := range roles {
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO memberships (person_id, organization_id, role) VALUES ($1, $2, $3)
			 ON CONFLICT DO NOTHING`, personID, orgID, role); err != nil {
			t.Fatalf("join %s to %s as %s: %v", personID, orgID, role, err)
		}
	}
}

func TestRoleCatalogueIsPublished(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "catalogue@e.com")

	r := do(t, http.MethodGet, "/api/v1/roles", coach, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /roles: %d %s", r.status, r.raw)
	}
	roles := r.arr()
	if len(roles) != 5 {
		t.Fatalf("expected the five roles, got %d: %s", len(roles), r.raw)
	}
	seen := map[string]int{}
	for _, entry := range roles {
		e := entry.(map[string]any)
		name := e["role"].(string)
		caps, _ := e["capabilities"].([]any)
		if e["label"] == "" || e["description"] == "" || len(caps) == 0 {
			t.Errorf("role %q is published without something a client needs: %v", name, e)
		}
		seen[name] = len(caps)
	}
	for _, want := range []string{"admin", "director", "coach", "parent", "player"} {
		if seen[want] == 0 {
			t.Errorf("role %q is missing from the catalogue", want)
		}
	}
	// The shape of the model, asserted where a client can see it: authority descends,
	// and a parent is not a slimmer coach but a much smaller set.
	if !(seen["admin"] > seen["director"] && seen["director"] > seen["coach"] && seen["coach"] > seen["parent"]) {
		t.Errorf("capability counts should descend with authority: %v", seen)
	}
}

func TestAccessDescribesTheSoloCoach(t *testing.T) {
	resetDB(t)
	coach, personID := signInCoach(t, "access@e.com")

	r := do(t, http.MethodGet, "/api/v1/me/access", coach, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /me/access: %d %s", r.status, r.raw)
	}
	if got := r.body["organizationId"]; got != orgOwnedBy(t, personID) {
		t.Errorf("access should describe the caller's own org, got %v", got)
	}
	roles := r.body["roles"].([]any)
	if len(roles) != 3 || roles[0] != "admin" {
		t.Errorf("a solo coach holds admin, director and coach, most privileged first: %v", roles)
	}
	if r.body["scope"] != "org" {
		t.Errorf("staff read the whole organization, got %v", r.body["scope"])
	}
	// Everything an admin may hand out, which is everything.
	if grantable := r.body["grantableRoles"].([]any); len(grantable) != 5 {
		t.Errorf("an admin may grant every role, got %v", grantable)
	}
	caps := r.body["capabilities"].([]any)
	if len(caps) == 0 {
		t.Fatal("capabilities must be published; the client gates its UI on them")
	}
}

func TestGrantAndRevokeRoles(t *testing.T) {
	resetDB(t)
	admin, adminPerson := signInCoach(t, "grant@e.com")
	person := createAthlete(t, admin, "Jamie Assistant")

	r := do(t, http.MethodPost, "/api/v1/members", admin, map[string]any{
		"personId": person, "role": "coach",
	})
	if r.status != http.StatusCreated {
		t.Fatalf("grant coach: %d %s", r.status, r.raw)
	}
	roles := r.body["roles"].([]any)
	if len(roles) != 2 || roles[0] != "coach" {
		t.Errorf("the grant should answer with the roles as they now stand: %v", roles)
	}

	// Idempotent: a client retrying a request it is unsure landed must not be told the
	// grant failed.
	if again := do(t, http.MethodPost, "/api/v1/members", admin, map[string]any{
		"personId": person, "role": "coach",
	}); again.status != http.StatusCreated {
		t.Errorf("re-granting a held role should succeed, got %d %s", again.status, again.raw)
	}

	list := do(t, http.MethodGet, "/api/v1/members", admin, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list members: %d %s", list.status, list.raw)
	}
	members := list.arr()
	if len(members) != 2 {
		t.Fatalf("expected the admin and the new coach, got %d: %s", len(members), list.raw)
	}
	for _, m := range members {
		e := m.(map[string]any)
		// The solo coach holds three roles and is ONE member, not three rows.
		if e["personId"] == adminPerson && len(e["roles"].([]any)) != 3 {
			t.Errorf("the owner should appear once with all their roles: %v", e)
		}
	}

	rev := do(t, http.MethodDelete, "/api/v1/members/"+person+"/roles/coach", admin, nil)
	if rev.status != http.StatusOK {
		t.Fatalf("revoke coach: %d %s", rev.status, rev.raw)
	}
	if roles := rev.body["roles"].([]any); len(roles) != 1 || roles[0] != "player" {
		t.Errorf("revoking one role must leave the others alone: %v", roles)
	}
	// Revoking what nobody holds is a 404, not a silent success.
	if again := do(t, http.MethodDelete, "/api/v1/members/"+person+"/roles/coach", admin, nil); again.status != http.StatusNotFound {
		t.Errorf("re-revoking should 404, got %d %s", again.status, again.raw)
	}
	if bad := do(t, http.MethodPost, "/api/v1/members", admin, map[string]any{
		"personId": person, "role": "wizard",
	}); bad.status != http.StatusBadRequest {
		t.Errorf("an unknown role is a 400, got %d %s", bad.status, bad.raw)
	}
}

func TestRankCeilingStopsSelfPromotion(t *testing.T) {
	resetDB(t)
	_, ownerPerson := signInCoach(t, "club@e.com")
	org := orgOwnedBy(t, ownerPerson)

	director, directorPerson := signInCoach(t, "director@e.com")
	joinOrgAs(t, directorPerson, org, "director")
	coach, coachPerson := signInCoach(t, "assistant@e.com")
	joinOrgAs(t, coachPerson, org, "coach")

	// A director staffs the club...
	if r := doAs(t, http.MethodPost, "/api/v1/members", director, org, map[string]any{
		"personId": coachPerson, "role": "parent",
	}); r.status != http.StatusCreated {
		t.Fatalf("a director may grant a role below their own: %d %s", r.status, r.raw)
	}
	// ...but cannot make themselves the owner of it, which is the escalation the whole
	// rank ceiling exists to stop.
	if r := doAs(t, http.MethodPost, "/api/v1/members", director, org, map[string]any{
		"personId": directorPerson, "role": "admin",
	}); r.status != http.StatusForbidden {
		t.Errorf("a director must not be able to mint an admin: %d %s", r.status, r.raw)
	}
	// Nor strip the admin who outranks them — a ceiling that applied only to granting
	// would still let them decide who runs the club.
	if r := doAs(t, http.MethodDelete, "/api/v1/members/"+ownerPerson+"/roles/admin", director, org, nil); r.status != http.StatusForbidden {
		t.Errorf("a director must not be able to strip an admin: %d %s", r.status, r.raw)
	}
	// A coach staffs nobody at all.
	if r := doAs(t, http.MethodPost, "/api/v1/members", coach, org, map[string]any{
		"personId": coachPerson, "role": "player",
	}); r.status != http.StatusForbidden {
		t.Errorf("a coach holds no member.grant: %d %s", r.status, r.raw)
	}
	if r := doAs(t, http.MethodGet, "/api/v1/members", coach, org, nil); r.status != http.StatusOK {
		t.Errorf("a coach may still see who is in the club: %d %s", r.status, r.raw)
	}
}

func TestAnOrganizationKeepsItsLastAdmin(t *testing.T) {
	resetDB(t)
	admin, adminPerson := signInCoach(t, "lastadmin@e.com")
	org := orgOwnedBy(t, adminPerson)

	// Demoting the only admin would leave an org nobody can administer: member.grant is
	// an admin/director capability and the rank ceiling means nobody left could hand the
	// role back. There is no undo, so the write is refused.
	r := do(t, http.MethodDelete, "/api/v1/members/"+adminPerson+"/roles/admin", admin, nil)
	if r.status != http.StatusConflict {
		t.Fatalf("expected 409 for the last admin, got %d %s", r.status, r.raw)
	}

	// With a second admin in place the same call is fine.
	_, secondPerson := signInCoach(t, "coadmin@e.com")
	joinOrgAs(t, secondPerson, org, "admin")
	if again := do(t, http.MethodDelete, "/api/v1/members/"+adminPerson+"/roles/admin", admin, nil); again.status != http.StatusOK {
		t.Errorf("with a co-admin, stepping down should work: %d %s", again.status, again.raw)
	}
}

// TestParentSeesOnlyTheirOwnChild is the test the parent tier stands on. A parent holds
// a role in a club full of other people's children; every read they make has to stop at
// their own household, because these endpoints return minors' birthdates, contact
// details and medical notes.
func TestParentSeesOnlyTheirOwnChild(t *testing.T) {
	resetDB(t)
	coach, coachPerson := signInCoach(t, "clubcoach@e.com")
	org := orgOwnedBy(t, coachPerson)

	mine := createAthlete(t, coach, "My Child")
	theirs := createAthlete(t, coach, "Another Family's Child")

	teamR := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U10 Blue"})
	if teamR.status != http.StatusCreated {
		t.Fatalf("create team: %d %s", teamR.status, teamR.raw)
	}
	team := teamR.body["id"].(string)
	otherR := do(t, http.MethodPost, "/api/v1/teams", coach, map[string]any{"name": "U14 Red"})
	otherTeam := otherR.body["id"].(string)
	for _, p := range []string{mine, theirs} {
		if r := do(t, http.MethodPost, "/api/v1/teams/"+team+"/roster", coach, map[string]any{"personId": p}); r.status != http.StatusCreated {
			t.Fatalf("roster %s: %d %s", p, r.status, r.raw)
		}
	}

	parent, parentPerson := signInCoach(t, "parent@e.com")
	joinOrgAs(t, parentPerson, org, "parent")
	// The link is what a parent's whole reach is built on, and only staff may write it.
	if r := do(t, http.MethodPost, "/api/v1/persons/"+mine+"/guardians", coach, map[string]any{
		"guardianPersonId": parentPerson,
	}); r.status != http.StatusCreated {
		t.Fatalf("link guardian: %d %s", r.status, r.raw)
	}

	if r := doAs(t, http.MethodGet, "/api/v1/me/access", parent, org, nil); r.status != http.StatusOK {
		t.Fatalf("parent access: %d %s", r.status, r.raw)
	} else if r.body["scope"] != "own" {
		t.Errorf("a parent's reads are scoped to their own household, got %v", r.body["scope"])
	}

	// Their own child: readable.
	if r := doAs(t, http.MethodGet, "/api/v1/persons/"+mine, parent, org, nil); r.status != http.StatusOK {
		t.Errorf("a parent must be able to read their own child: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodGet, "/api/v1/me/children", parent, nil); r.status != http.StatusOK {
		t.Errorf("GET /me/children: %d %s", r.status, r.raw)
	} else if kids := r.arr(); len(kids) != 1 || kids[0].(map[string]any)["id"] != mine {
		t.Errorf("expected exactly their own child, got %s", r.raw)
	}

	// Another family's child, in the same club, on the same team: not readable, by any
	// route into that Person's data.
	for _, path := range []string{
		"/api/v1/persons/" + theirs,
		"/api/v1/persons/" + theirs + "/instances",
		"/api/v1/persons/" + theirs + "/aggregate",
	} {
		if r := doAs(t, http.MethodGet, path, parent, org, nil); r.status != http.StatusNotFound {
			t.Errorf("%s must not be readable by a parent: %d %s", path, r.status, r.raw)
		}
	}

	// Teams: the one their child is on, and its roster narrowed to their household.
	list := doAs(t, http.MethodGet, "/api/v1/teams", parent, org, nil)
	if list.status != http.StatusOK {
		t.Fatalf("parent team list: %d %s", list.status, list.raw)
	}
	teams := list.arr()
	if len(teams) != 1 || teams[0].(map[string]any)["id"] != team {
		t.Errorf("a parent sees only the teams their household is on, got %s", list.raw)
	}
	if r := doAs(t, http.MethodGet, "/api/v1/teams/"+otherTeam, parent, org, nil); r.status != http.StatusNotFound {
		t.Errorf("a team their child is not on must not be readable: %d %s", r.status, r.raw)
	}
	detail := doAs(t, http.MethodGet, "/api/v1/teams/"+team, parent, org, nil)
	if detail.status != http.StatusOK {
		t.Fatalf("parent team detail: %d %s", detail.status, detail.raw)
	}
	roster := detail.body["roster"].([]any)
	if len(roster) != 1 || roster[0].(map[string]any)["personId"] != mine {
		t.Errorf("the roster must be narrowed to their own child, got %s", detail.raw)
	}
	// The team's real size is not a secret; the other families' details are.
	if size := detail.body["team"].(map[string]any)["activeRosterCount"].(float64); size != 2 {
		t.Errorf("the team's size should still be honest, got %v", size)
	}

	// A parent must be able to fetch the forms they are asked to fill in — a form you
	// cannot read is a form you cannot answer — so template reads stay open to them.
	if r := doAs(t, http.MethodGet, "/api/v1/templates", parent, org, nil); r.status != http.StatusOK {
		t.Errorf("a parent must be able to read the forms they answer: %d %s", r.status, r.raw)
	}

	// And none of the staff surface.
	for _, c := range []struct {
		method, path string
		payload      any
	}{
		{http.MethodPost, "/api/v1/teams", map[string]any{"name": "Parent's Team"}},
		{http.MethodPost, "/api/v1/persons", map[string]any{"displayName": "Someone"}},
		{http.MethodPost, "/api/v1/teams/" + team + "/roster", map[string]any{"personId": mine}},
		{http.MethodGet, "/api/v1/members", nil},
		{http.MethodGet, "/api/v1/drills", nil},
		{http.MethodGet, "/api/v1/sessions", nil},
		{http.MethodPost, "/api/v1/templates", map[string]any{
			"context": "pre_game", "name": "Mine",
			"fields": []any{map[string]any{"key": "k", "label": "L", "kind": "scale"}},
		}},
		{http.MethodDelete, "/api/v1/teams/" + team, nil},
	} {
		if r := doAs(t, c.method, c.path, parent, org, c.payload); r.status != http.StatusForbidden {
			t.Errorf("%s %s should be forbidden to a parent, got %d %s", c.method, c.path, r.status, r.raw)
		}
	}
}

// A player holds the smallest role there is, and it still has to work: their own record,
// their own team, and nobody else's.
func TestPlayerSeesOnlyThemselves(t *testing.T) {
	resetDB(t)
	coach, coachPerson := signInCoach(t, "playercoach@e.com")
	org := orgOwnedBy(t, coachPerson)
	teammate := createAthlete(t, coach, "A Teammate")

	player, playerPerson := signInCoach(t, "player@e.com")
	joinOrgAs(t, playerPerson, org, "player")

	if r := doAs(t, http.MethodGet, "/api/v1/persons/"+playerPerson, player, org, nil); r.status != http.StatusOK {
		t.Errorf("a player may always read themselves: %d %s", r.status, r.raw)
	}
	if r := doAs(t, http.MethodGet, "/api/v1/persons/"+teammate, player, org, nil); r.status != http.StatusNotFound {
		t.Errorf("a player must not read a teammate's record: %d %s", r.status, r.raw)
	}
	if r := doAs(t, http.MethodGet, "/api/v1/teams", player, org, nil); r.status != http.StatusOK {
		t.Errorf("a player may list their teams: %d %s", r.status, r.raw)
	} else if len(r.arr()) != 0 {
		t.Errorf("this one is on no team yet, so the list is empty: %s", r.raw)
	}
	if r := doAs(t, http.MethodPost, "/api/v1/teams", player, org, map[string]any{"name": "Nope"}); r.status != http.StatusForbidden {
		t.Errorf("a player creates nothing: %d %s", r.status, r.raw)
	}
}

// Somebody with a membership but no recognized role must be able to do nothing at all,
// rather than falling through to whatever a handler forgot to check.
func TestAnUnknownRoleGrantsNothing(t *testing.T) {
	resetDB(t)
	_, ownerPerson := signInCoach(t, "unknownrole@e.com")
	org := orgOwnedBy(t, ownerPerson)
	stranger, strangerPerson := signInCoach(t, "stranger@e.com")

	// 'player' is legal in the schema; the point is a membership whose role carries no
	// capability in this build — the same position a role added by a newer service
	// would put an older one in.
	joinOrgAs(t, strangerPerson, org, "player")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE memberships SET role = 'parent' WHERE person_id = $1 AND organization_id = $2`,
		strangerPerson, org); err != nil {
		t.Fatalf("rewrite role: %v", err)
	}

	// A parent with no children linked: a valid role, an empty reach.
	for _, path := range []string{"/api/v1/teams", "/api/v1/persons/" + ownerPerson} {
		r := doAs(t, http.MethodGet, path, stranger, org, nil)
		if r.status == http.StatusOK && len(r.arr()) > 0 {
			t.Errorf("%s leaked rows to an unlinked parent: %s", path, r.raw)
		}
		if r.status != http.StatusOK && r.status != http.StatusNotFound {
			t.Errorf("%s: expected an empty answer or a 404, got %d %s", path, r.status, r.raw)
		}
	}
}
