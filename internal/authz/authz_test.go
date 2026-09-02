package authz_test

import (
	"testing"

	"github.com/monsoudz/soccerkit-api/internal/authz"
)

// The matrix is the permission model, so these tests are written against the product
// rules it is supposed to encode rather than against the table itself — a test that
// re-lists the table only proves the table was copied twice.

func TestEveryRoleIsDefinedAndOrdered(t *testing.T) {
	for _, r := range authz.All {
		if !r.Valid() {
			t.Errorf("%s is listed in All but has no capabilities", r)
		}
		if r.Label() == "" || r.Description() == "" {
			t.Errorf("%s has no label/description for a client to show", r)
		}
	}
	// The schema's CHECK constraint and this list must agree, or a role that is legal
	// in the database grants nothing at runtime.
	if len(authz.All) != 5 {
		t.Fatalf("expected the five schema roles, got %d", len(authz.All))
	}
	for i := 1; i < len(authz.All); i++ {
		if authz.All[i-1].Rank() <= authz.All[i].Rank() {
			t.Errorf("All must run most-privileged first: %s !> %s", authz.All[i-1], authz.All[i])
		}
	}
	if authz.Role("wizard").Valid() {
		t.Error("an unknown role must not validate")
	}
}

func TestStaffCanRunTheirTeams(t *testing.T) {
	for _, r := range []authz.Role{authz.RoleAdmin, authz.RoleDirector, authz.RoleCoach} {
		s := authz.NewSet(string(r))
		for _, c := range []authz.Capability{
			authz.CapPersonCreate, authz.CapTeamCreate, authz.CapRosterManage,
			authz.CapGameWrite, authz.CapTemplateWrite, authz.CapEvaluationSubmit,
		} {
			if !s.Can(c) {
				t.Errorf("%s should hold %s", r, c)
			}
		}
		if s.Scope() != authz.ScopeOrg {
			t.Errorf("%s should read the whole organization, got %s", r, s.Scope())
		}
	}
}

func TestOnlyTheClubTiersRunTheClub(t *testing.T) {
	coach := authz.NewSet("coach")
	for _, c := range []authz.Capability{
		authz.CapMemberGrant, authz.CapMemberRevoke, authz.CapOrgUpdate,
		authz.CapCoachReview, authz.CapReportRead,
	} {
		if coach.Can(c) {
			t.Errorf("a coach must not hold %s", c)
		}
	}
	if authz.NewSet("director").Can(authz.CapOrgDelete) {
		t.Error("only an admin may delete the organization")
	}
	if !authz.NewSet("admin").Can(authz.CapOrgDelete) {
		t.Error("an admin may delete the organization")
	}
}

func TestParentAndPlayerSeeOnlyTheirOwn(t *testing.T) {
	for _, r := range []authz.Role{authz.RoleParent, authz.RolePlayer} {
		s := authz.NewSet(string(r))
		if s.Scope() != authz.ScopeOwn {
			t.Errorf("%s must be scoped to their own household, got %s", r, s.Scope())
		}
		// They participate in the evaluation loop — that is the whole parent/player
		// product — but they never run the org side of it. Reading templates is part of
		// participating: a form you cannot fetch is a form you cannot answer.
		if !s.Can(authz.CapEvaluationSubmit) || !s.Can(authz.CapEvaluationRead) || !s.Can(authz.CapTemplateRead) {
			t.Errorf("%s should be able to fetch, answer and read their own evaluations", r)
		}
		for _, c := range []authz.Capability{
			authz.CapPersonCreate, authz.CapTeamCreate, authz.CapRosterManage,
			authz.CapGameWrite, authz.CapTemplateWrite, authz.CapMemberRead,
			authz.CapSessionRead, authz.CapDrillRead,
			authz.CapMemberGrant, authz.CapDrillWrite, authz.CapShareManage,
		} {
			if s.Can(c) {
				t.Errorf("%s must not hold %s", r, c)
			}
		}
	}
	// A parent holds the child's medical details; a player does not hold anyone's.
	if !authz.NewSet("parent").Can(authz.CapMedicalRead) {
		t.Error("a parent must be able to read their child's medical notes")
	}
	if authz.NewSet("player").Can(authz.CapMedicalRead) {
		t.Error("a player should not be handed medical notes in this tier")
	}
}

func TestHoldingSeveralRolesUnionsThem(t *testing.T) {
	// The solo coach: one sign-in, three memberships. And the parent who also coaches,
	// which is why permissions are additive rather than "highest role wins".
	solo := authz.NewSet("admin", "director", "coach")
	if !solo.Can(authz.CapOrgDelete) || !solo.Can(authz.CapRosterManage) {
		t.Error("the solo coach should hold the union of their three roles")
	}
	if got := solo.Roles()[0]; got != authz.RoleAdmin {
		t.Errorf("Roles() must run most privileged first, got %s", got)
	}

	parentCoach := authz.NewSet("parent", "coach")
	if parentCoach.Scope() != authz.ScopeOrg {
		t.Error("a parent who also coaches reads as a coach")
	}
	if !parentCoach.Can(authz.CapTeamCreate) {
		t.Error("the coach half must still work")
	}
}

func TestUnknownRolesAreIgnoredNotHonoured(t *testing.T) {
	s := authz.NewSet("wizard", "")
	if !s.Empty() || s.Scope() != authz.ScopeNone {
		t.Error("an unrecognized role must grant nothing")
	}
	if s.Can(authz.CapOrgRead) {
		t.Error("an unrecognized role must not carry capabilities")
	}
	// A newer service writing a role this build does not know must not lock the caller
	// out of the roles it does know.
	mixed := authz.NewSet("wizard", "coach")
	if !mixed.Can(authz.CapTeamCreate) {
		t.Error("a known role alongside an unknown one must still work")
	}
}

func TestGrantingIsBoundedByRank(t *testing.T) {
	admin := authz.NewSet("admin")
	director := authz.NewSet("director")
	coach := authz.NewSet("coach")

	if !admin.CanGrant(authz.RoleAdmin) {
		t.Error("an admin may appoint another admin")
	}
	// The escalation this rule exists to stop: member.grant alone would let a director
	// make themselves the owner.
	if director.CanGrant(authz.RoleAdmin) {
		t.Error("a director must not be able to mint an admin")
	}
	for _, r := range []authz.Role{authz.RoleDirector, authz.RoleCoach, authz.RoleParent, authz.RolePlayer} {
		if !director.CanGrant(r) {
			t.Errorf("a director may grant %s", r)
		}
	}
	// Rank is a ceiling, not a licence: a coach outranks a parent and still may not
	// hand out roles at all.
	if coach.CanGrant(authz.RolePlayer) {
		t.Error("a coach holds no member.grant, so rank is irrelevant")
	}
	if admin.CanGrant("wizard") {
		t.Error("an unknown role is never grantable")
	}
}

func TestCapabilitiesAreStableAndDeduplicated(t *testing.T) {
	caps := authz.NewSet("admin", "coach").Capabilities()
	for i := 1; i < len(caps); i++ {
		if caps[i-1] >= caps[i] {
			t.Fatalf("capabilities must be sorted and unique: %s then %s", caps[i-1], caps[i])
		}
	}
	if len(caps) != len(authz.RoleAdmin.Capabilities()) {
		t.Error("admin+coach should be exactly admin's set, since admin is a superset")
	}
	if len(authz.NewSet().Capabilities()) != 0 {
		t.Error("no roles means no capabilities")
	}
}

func TestGrantableRolesIsWhatTheRankAllows(t *testing.T) {
	// The picker an app renders. A director sees everything but the role that would
	// make them the owner; a coach sees nothing, because they staff nobody.
	if got := len(authz.NewSet("admin").GrantableRoles()); got != len(authz.All) {
		t.Errorf("an admin may grant every role, got %d", got)
	}
	director := authz.NewSet("director").GrantableRoles()
	if len(director) != 4 || director[0] != authz.RoleDirector {
		t.Errorf("a director may grant director and below, got %v", director)
	}
	if got := authz.NewSet("coach").GrantableRoles(); len(got) != 0 {
		t.Errorf("a coach grants no roles, got %v", got)
	}
}

func TestRevokingHasTheSameCeilingAsGranting(t *testing.T) {
	// Otherwise the ceiling is decorative: a director who cannot appoint an admin but
	// can remove one still decides who owns the club.
	if authz.NewSet("director").CanRevoke(authz.RoleAdmin) {
		t.Error("a director must not be able to strip an admin")
	}
	if !authz.NewSet("admin").CanRevoke(authz.RoleAdmin) {
		t.Error("an admin may remove another admin")
	}
	if authz.NewSet("coach").CanRevoke(authz.RolePlayer) {
		t.Error("a coach holds no member.revoke")
	}
}
