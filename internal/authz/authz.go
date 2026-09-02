// Package authz is the role model: who can exist in an organization, and what each of
// them is allowed to do.
//
// The schema has carried the five role names since 0001_init, but "what a director may
// do that a coach may not" lived nowhere — every handler answered it inline with a
// literal list like hasAnyRole("admin", "director", "coach"). That works while there is
// one real role (the solo coach, who holds the top three) and stops working the moment
// parents and players sign in: a rule spread over twenty call sites cannot be read,
// tested, or shown to a client, and the first handler that forgets a name is a silent
// privilege bug.
//
// So permission is named here, once, as a capability — a verb the product has, not an
// endpoint the API happens to expose. Roles are sets of capabilities; handlers ask
// "can you do this?" instead of "who are you?". Adding a role means adding a row to the
// matrix, and adding a feature to a role means adding it to that role's list, with
// nothing to hunt for.
//
// This package is deliberately pure: no database, no HTTP. That is what makes the whole
// matrix testable in microseconds and printable to the client at GET /roles, so the iOS
// app can gate its UI on the same table the server enforces rather than a second copy
// that drifts.
package authz

import "sort"

// --- roles ----------------------------------------------------------------

// Role is a person's standing in ONE organization. It is never a property of a person:
// the same human is a director at their club and a parent at their kid's, and both are
// true at once. See memberships in 0001_init.sql.
type Role string

const (
	// RoleAdmin owns the organization — billing, settings, and who else is an admin.
	// A personal org's owner holds this over themselves.
	RoleAdmin Role = "admin"
	// RoleDirector runs the club's sporting side: staffing, teams across age groups,
	// club-wide reporting, and evaluating the coaches themselves.
	RoleDirector Role = "director"
	// RoleCoach runs their teams: roster, sessions, game day, and athlete evaluation.
	RoleCoach Role = "coach"
	// RoleParent sees their own children and nobody else's, and answers the forms
	// asked of a parent (availability, pre-game readiness, medical updates).
	RoleParent Role = "parent"
	// RolePlayer is the athlete's own login — self-assessment and their own history.
	// Young athletes have no login at all; they are a Person, not an account.
	RolePlayer Role = "player"
)

// All is every role, ordered most privileged first. It is the source for the /roles
// catalogue and for validating a role name off the wire.
var All = []Role{RoleAdmin, RoleDirector, RoleCoach, RoleParent, RolePlayer}

// Valid reports whether r is a role this system defines. The memberships CHECK
// constraint says the same thing in SQL; this is so a bad role name comes back as a
// 400 naming the field rather than a 500 from the database.
func (r Role) Valid() bool { return len(capabilities[r]) > 0 }

// Rank orders roles by authority. It answers exactly one question — may this caller
// hand out that role? — and is not a substitute for a capability check: a parent
// outranks a player and neither can touch a team.
func (r Role) Rank() int {
	switch r {
	case RoleAdmin:
		return 40
	case RoleDirector:
		return 30
	case RoleCoach:
		return 20
	case RoleParent:
		return 10
	case RolePlayer:
		return 5
	}
	return 0
}

// Label is the human name for a role, for a client that would otherwise capitalize the
// wire value and get it wrong in another language.
func (r Role) Label() string {
	switch r {
	case RoleAdmin:
		return "Admin"
	case RoleDirector:
		return "Director"
	case RoleCoach:
		return "Coach"
	case RoleParent:
		return "Parent"
	case RolePlayer:
		return "Player"
	}
	return string(r)
}

// Description is the one-line explanation the app shows next to a role in a picker.
func (r Role) Description() string {
	switch r {
	case RoleAdmin:
		return "Owns the organization: settings, billing, and who holds which role."
	case RoleDirector:
		return "Runs the club: staff, teams across age groups, club-wide reporting, and coach reviews."
	case RoleCoach:
		return "Runs their teams: roster, training sessions, game day, and athlete evaluations."
	case RoleParent:
		return "Sees their own children only: schedule, evaluations, and the forms asked of a parent."
	case RolePlayer:
		return "The athlete's own login: their schedule, their self-assessments, their history."
	}
	return ""
}

// --- capabilities ---------------------------------------------------------

// Capability is one thing a person may do, named for the product verb rather than the
// endpoint. Endpoints come and go; "may this person put an athlete on a roster" does not.
type Capability string

const (
	// Organization
	CapOrgRead   Capability = "org.read"   // see the org exists, its name and kind
	CapOrgUpdate Capability = "org.update" // rename it, change its settings
	CapOrgDelete Capability = "org.delete" // destroy it and everything in it

	// Membership & roles
	CapMemberRead   Capability = "member.read"   // see who belongs to the org and as what
	CapMemberGrant  Capability = "member.grant"  // give someone a role (bounded by Rank)
	CapMemberRevoke Capability = "member.revoke" // take a role away (bounded by Rank)
	CapInviteSend   Capability = "invite.send"   // invite someone into the org (bounded by CanInvite)

	// People
	CapPersonCreate Capability = "person.create"       // add an athlete/parent record
	CapPersonRead   Capability = "person.read"         // read a person, at DataScope's width
	CapPersonUpdate Capability = "person.update"       // edit contact/identity details
	CapPersonDelete Capability = "person.delete"       // remove a person from the org
	CapMedicalRead  Capability = "person.medical.read" // allergies, conditions, emergency contacts
	CapGuardianLink Capability = "guardian.link"       // link a parent to a child

	// Teams & roster
	CapTeamRead     Capability = "team.read"     // see teams, at DataScope's width
	CapTeamCreate   Capability = "team.create"   //
	CapTeamUpdate   Capability = "team.update"   //
	CapTeamDelete   Capability = "team.delete"   //
	CapRosterManage Capability = "roster.manage" // add/remove athletes on a team

	// Training content
	CapDrillRead    Capability = "drill.read"
	CapDrillWrite   Capability = "drill.write"
	CapSessionRead  Capability = "session.read"
	CapSessionWrite Capability = "session.write"

	// Game day
	CapGameRead  Capability = "game.read"  // the schedule and results
	CapGameWrite Capability = "game.write" // create a fixture, record a result

	// The evaluation engine (the product's core loop)
	CapTemplateRead     Capability = "template.read"
	CapTemplateWrite    Capability = "template.write"
	CapEvaluationSubmit Capability = "evaluation.submit" // fill in a form instance
	CapEvaluationRead   Capability = "evaluation.read"   // read instances/aggregates, at DataScope's width
	CapCoachReview      Capability = "coach.review"      // evaluate a COACH, not an athlete
	CapReportRead       Capability = "report.read"       // club-wide aggregates across teams

	// Sharing (seam 5 — share_grants)
	CapShareManage Capability = "share.manage"
)

// capabilities is the matrix: the whole permission model of the product, in one place
// you can read top to bottom. Everything else in this package is a lookup into it.
//
// Read a row as "this role may". A capability absent from a row is denied — there is no
// inheritance between roles, deliberately: a director is not "a coach plus extras", and
// writing each row out means a change to one role cannot quietly move another.
var capabilities = map[Role][]Capability{
	// The org's owner. Everything a director may do, plus the two things that end an
	// organization or change who runs it.
	RoleAdmin: {
		CapOrgRead, CapOrgUpdate, CapOrgDelete,
		CapMemberRead, CapMemberGrant, CapMemberRevoke, CapInviteSend,
		CapPersonCreate, CapPersonRead, CapPersonUpdate, CapPersonDelete, CapMedicalRead, CapGuardianLink,
		CapTeamRead, CapTeamCreate, CapTeamUpdate, CapTeamDelete, CapRosterManage,
		CapDrillRead, CapDrillWrite, CapSessionRead, CapSessionWrite,
		CapGameRead, CapGameWrite,
		CapTemplateRead, CapTemplateWrite, CapEvaluationSubmit, CapEvaluationRead,
		CapCoachReview, CapReportRead,
		CapShareManage,
	},
	// Runs the sporting side. Staffs the club and reviews its coaches, but does not own
	// it: no org.delete, and Rank keeps them from minting an admin.
	RoleDirector: {
		CapOrgRead, CapOrgUpdate,
		CapMemberRead, CapMemberGrant, CapMemberRevoke, CapInviteSend,
		CapPersonCreate, CapPersonRead, CapPersonUpdate, CapPersonDelete, CapMedicalRead, CapGuardianLink,
		CapTeamRead, CapTeamCreate, CapTeamUpdate, CapTeamDelete, CapRosterManage,
		CapDrillRead, CapDrillWrite, CapSessionRead, CapSessionWrite,
		CapGameRead, CapGameWrite,
		CapTemplateRead, CapTemplateWrite, CapEvaluationSubmit, CapEvaluationRead,
		CapCoachReview, CapReportRead,
		CapShareManage,
	},
	// The shipped product's main character. Everything about their athletes and their
	// teams; nothing about staffing, and no reviewing of other coaches.
	RoleCoach: {
		CapOrgRead,
		// A coach invites, but does not staff: CanInvite caps them at roles below their
		// own, which is the parents and players of their own athletes — the invitations
		// they are the only person in a position to send.
		CapMemberRead, CapInviteSend,
		CapPersonCreate, CapPersonRead, CapPersonUpdate, CapMedicalRead, CapGuardianLink,
		CapTeamRead, CapTeamCreate, CapTeamUpdate, CapTeamDelete, CapRosterManage,
		CapDrillRead, CapDrillWrite, CapSessionRead, CapSessionWrite,
		CapGameRead, CapGameWrite,
		CapTemplateRead, CapTemplateWrite, CapEvaluationSubmit, CapEvaluationRead,
		CapShareManage,
	},
	// Reads and writes about their own children and nobody else's. The capability says
	// what they may do; DataScope says how far it reaches — the pair is what makes a
	// parent's person.read safe next to a coach's.
	RoleParent: {
		CapOrgRead,
		CapPersonRead, CapMedicalRead,
		CapTeamRead, CapGameRead,
		// template.read is here because a form you cannot fetch is a form you cannot
		// answer: without it, evaluation.submit is a capability with no way to use it.
		// It is the broadest thing in this row — it exposes the club's rubrics, not
		// just the forms addressed to a parent — and narrowing it is the job of an
		// audience on the template (or a share_grant), not of removing it here.
		CapTemplateRead, CapEvaluationSubmit, CapEvaluationRead,
	},
	// The athlete's own login: themselves only.
	RolePlayer: {
		CapOrgRead,
		CapPersonRead,
		CapTeamRead, CapGameRead,
		CapTemplateRead, CapEvaluationSubmit, CapEvaluationRead,
	},
}

// Capabilities lists what a single role may do, sorted for a stable wire order.
func (r Role) Capabilities() []Capability {
	caps := append([]Capability(nil), capabilities[r]...)
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return caps
}

// Can reports whether a single role carries a capability.
func (r Role) Can(c Capability) bool {
	for _, have := range capabilities[r] {
		if have == c {
			return true
		}
	}
	return false
}

// --- scope ----------------------------------------------------------------

// DataScope is how far a read reaches once a capability has allowed it at all.
//
// It is the other half of the model, and the half that is easy to forget: a parent and
// a coach both hold person.read, and the difference between them is not what they may
// do but whose rows they may do it to. Capability alone would hand a parent the whole
// club's medical notes.
type DataScope string

const (
	// ScopeNone: no reach at all.
	ScopeNone DataScope = "none"
	// ScopeOwn: themselves, plus the children they are the guardian of.
	ScopeOwn DataScope = "own"
	// ScopeOrg: every person and team in the organization.
	ScopeOrg DataScope = "org"
)

// --- role sets ------------------------------------------------------------

// Set is the roles one person holds in ONE organization. A person routinely holds
// several — the solo coach is admin, director and coach at once, and a parent who
// coaches is both — so every question below is asked of the union.
type Set struct {
	roles map[Role]bool
}

// NewSet builds a Set from role names as they are stored in memberships.role. Unknown
// names are dropped rather than rejected: a row written by a newer version of this
// service must not lock an older one out of the org, and an unknown name grants
// nothing anyway.
func NewSet(names ...string) Set {
	s := Set{roles: make(map[Role]bool, len(names))}
	for _, n := range names {
		if r := Role(n); r.Valid() {
			s.roles[r] = true
		}
	}
	return s
}

// Empty reports whether the person holds no recognized role here.
func (s Set) Empty() bool { return len(s.roles) == 0 }

// Has reports whether the person holds exactly this role.
func (s Set) Has(r Role) bool { return s.roles[r] }

// HasAny reports whether the person holds at least one of these roles. Prefer Can:
// asking by name is what this package exists to replace, and it is right only where
// the rule really is about identity rather than permission.
func (s Set) HasAny(rs ...Role) bool {
	for _, r := range rs {
		if s.roles[r] {
			return true
		}
	}
	return false
}

// Can reports whether ANY held role carries the capability. Permissions are additive:
// a parent who also coaches gets the coach's reach, which is the point of holding both.
func (s Set) Can(c Capability) bool {
	for r := range s.roles {
		if r.Can(c) {
			return true
		}
	}
	return false
}

// Roles lists the held roles, most privileged first.
func (s Set) Roles() []Role {
	out := make([]Role, 0, len(s.roles))
	for r := range s.roles {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank() != out[j].Rank() {
			return out[i].Rank() > out[j].Rank()
		}
		return out[i] < out[j]
	})
	return out
}

// Capabilities is the union of every held role's capabilities, sorted. This is what
// GET /me/access returns, so a client can gray out a button for the same reason
// the server would refuse the request behind it.
func (s Set) Capabilities() []Capability {
	seen := map[Capability]bool{}
	for r := range s.roles {
		for _, c := range capabilities[r] {
			seen[c] = true
		}
	}
	out := make([]Capability, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// MaxRank is the authority of the highest role held.
func (s Set) MaxRank() int {
	max := 0
	for r := range s.roles {
		if r.Rank() > max {
			max = r.Rank()
		}
	}
	return max
}

// CanAssign is the rank ceiling on its own: may this caller act on a role of this
// authority at all? A director must not be able to promote themselves to admin, and no
// one may hand out — or strip — authority above their own.
//
// It is deliberately separate from the capability check, because granting and revoking
// are separate capabilities over the same ceiling.
func (s Set) CanAssign(target Role) bool {
	return target.Valid() && target.Rank() <= s.MaxRank()
}

// CanGrant reports whether this caller may hand out the role target. Holding
// member.grant is necessary and not sufficient: without the ceiling, the capability is
// a ladder to the top of the organization.
func (s Set) CanGrant(target Role) bool {
	return s.Can(CapMemberGrant) && s.CanAssign(target)
}

// CanRevoke reports whether this caller may take the role target away from someone.
func (s Set) CanRevoke(target Role) bool {
	return s.Can(CapMemberRevoke) && s.CanAssign(target)
}

// GrantableRoles is what this caller may hand out, most privileged first — the list an
// app should populate its role picker from, rather than showing every role and learning
// from a 403 which ones were real.
func (s Set) GrantableRoles() []Role {
	out := []Role{}
	for _, r := range All {
		if s.CanGrant(r) {
			out = append(out, r)
		}
	}
	return out
}

// CanInvite reports whether this caller may invite somebody into the organization as
// the role target.
//
// The ceiling is deliberately not the same for everyone who may send an invitation.
// Staff who can already grant a role directly (admin, director) can invite at exactly
// the same reach — an invitation must never be a way around the grant ceiling, or
// "invite yourself as admin, then accept" is a one-step takeover. A coach holds no
// member.grant at all and is capped strictly below their own rank: the parents and
// players of their own athletes, which are the invitations only they are in a position
// to send, and not a peer coach or a director, which is the club's decision to make.
func (s Set) CanInvite(target Role) bool {
	if !target.Valid() || !s.Can(CapInviteSend) {
		return false
	}
	if s.Can(CapMemberGrant) {
		return s.CanAssign(target)
	}
	return target.Rank() < s.MaxRank()
}

// InvitableRoles is what this caller may invite somebody as, most privileged first.
func (s Set) InvitableRoles() []Role {
	out := []Role{}
	for _, r := range All {
		if s.CanInvite(r) {
			out = append(out, r)
		}
	}
	return out
}

// Scope is how wide this person's reads over people, teams and evaluations run.
// Staff see the organization; a parent or player sees their own household.
func (s Set) Scope() DataScope {
	switch {
	case s.HasAny(RoleAdmin, RoleDirector, RoleCoach):
		return ScopeOrg
	case s.HasAny(RoleParent, RolePlayer):
		return ScopeOwn
	default:
		return ScopeNone
	}
}
