package api

// The five roles an organization membership may carry.
//
// They are stored as rows -- UNIQUE (person_id, organization_id, role) -- so one person
// may hold several in the same organization, and a coach who is also a parent is two
// memberships and one human. orgContext.roles is the set of them for the org the caller
// is acting in.
//
// The split that matters is staff against the rest. admin, director and coach run a
// club and see all of it. parent and player belong to one and see almost none of it: a
// parent sees the children they are recorded as guardian of, a player sees themselves.
// That distinction had nowhere to live until now -- every read keyed on a person id
// asked only whether the subject was in the organization, never what the caller was.
const (
	roleAdmin    = "admin"
	roleDirector = "director"
	roleCoach    = "coach"
	roleParent   = "parent"
	rolePlayer   = "player"
)

// roleRank orders the roles for the one question granting has to answer: may the caller
// hand out this role?
//
// A caller may grant no role above their own highest. Without that ceiling "may manage
// members" and "may make myself an admin" are the same permission one call apart, and a
// director could appoint themselves over the person who appointed them. parent and
// player share a rank because neither confers authority over the other -- the ordering
// is about what may be granted, not about seniority between families.
var roleRank = map[string]int{
	roleAdmin:    4,
	roleDirector: 3,
	roleCoach:    2,
	roleParent:   1,
	rolePlayer:   1,
}

// validRole reports whether a string is one of the five. The column has the same CHECK,
// so this is about answering with a 400 that names the problem rather than a 500 from
// the constraint.
func validRole(role string) bool {
	_, ok := roleRank[role]
	return ok
}

// allRoles is the vocabulary, in rank order, for error messages.
var allRoles = []string{roleAdmin, roleDirector, roleCoach, roleParent, rolePlayer}

// highestRank is the caller's own level in the organization they are acting in, and the
// ceiling on what they may grant.
func (o orgContext) highestRank() int {
	best := 0
	for role := range o.roles {
		if r := roleRank[role]; r > best {
			best = r
		}
	}
	return best
}

// isStaff reports whether the caller runs the organization rather than belonging to it.
func (o orgContext) isStaff() bool {
	return o.hasAnyRole(roleAdmin, roleDirector, roleCoach)
}

// canManageMembers reports whether the caller may change who is in the organization and
// with what role. This is the matrix's manageOrg, and it is admin alone: a coach runs
// training and a director standardizes it, but deciding who is in the club at all -- and
// therefore who can see whose children -- is the one authority that does not spread.
func (o orgContext) canManageMembers() bool {
	return o.hasAnyRole(roleAdmin)
}

// highestRankOf is highestRank for someone else's role set, so a change can be refused
// when its target outranks the caller.
func highestRankOf(roles []string) int {
	best := 0
	for _, role := range roles {
		if r := roleRank[role]; r > best {
			best = r
		}
	}
	return best
}
