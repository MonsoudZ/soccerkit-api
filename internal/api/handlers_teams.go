package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/authz"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

type createTeamRequest struct {
	Name     string  `json:"name"`
	AgeGroup *string `json:"ageGroup"`
	Season   *string `json:"season"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapTeamCreate, "you cannot create teams in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	var req createTeamRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" {
		writeError(w, errValidation("name is required"))
		return
	}
	team, err := s.store.CreateTeam(r.Context(), store.CreateTeamParams{
		OrganizationID: oc.orgID, Name: req.Name, AgeGroup: req.AgeGroup, Season: req.Season,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, teamDTO(team, 0))
}

// handleListTeams answers with the teams this caller may see, which is not the same
// list for everyone: staff get the organization's teams, a parent or player gets the
// teams their own household is actually rostered on. Same endpoint, same capability,
// different reach — see authz.DataScope.
func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapTeamRead, "you cannot see teams in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	var out []Team
	if oc.scope() == authz.ScopeOrg {
		rows, lerr := s.store.ListTeamsInOrg(r.Context(), oc.orgID)
		if lerr != nil {
			writeError(w, lerr)
			return
		}
		out = make([]Team, len(rows))
		for i, t := range rows {
			out[i] = teamDTO(store.Team{
				ID: t.ID, OrganizationID: t.OrganizationID, Name: t.Name, AgeGroup: t.AgeGroup,
				Season: t.Season, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			}, t.ActiveRosterCount)
		}
	} else {
		rows, lerr := s.store.ListTeamsForHouseholdInOrg(r.Context(), store.ListTeamsForHouseholdInOrgParams{
			OrganizationID: oc.orgID, PersonID: personIDFrom(r.Context()),
		})
		if lerr != nil {
			writeError(w, lerr)
			return
		}
		out = make([]Team, len(rows))
		for i, t := range rows {
			out[i] = teamDTO(store.Team{
				ID: t.ID, OrganizationID: t.OrganizationID, Name: t.Name, AgeGroup: t.AgeGroup,
				Season: t.Season, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			}, t.ActiveRosterCount)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapTeamRead, "you cannot see teams in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	roster, err := s.store.ListActiveRoster(r.Context(), team.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	// The roster carries every teammate's name, address and birthdate. A coach is
	// meant to have it; a parent is not — their child being on the team is not a
	// reason to hand them the other families' details — so a scoped caller sees the
	// team, its size, and their own household's entries in it.
	visible, err := s.rosterVisibleTo(r.Context(), oc)
	if err != nil {
		writeError(w, err)
		return
	}
	entries := make([]RosterEntry, 0, len(roster))
	for _, row := range roster {
		if visible == nil || visible[row.PersonID] {
			entries = append(entries, rosterRowDTO(row))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"team":   teamDTO(team, int64(len(roster))),
		"roster": entries,
	})
}

type addRosterRequest struct {
	PersonID     string  `json:"personId"`
	JerseyNumber *int32  `json:"jerseyNumber"`
	Position     *string `json:"position"`
	JoinedOn     *string `json:"joinedOn"`
}

func (s *Server) handleAddRoster(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapRosterManage, "you cannot change a roster in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	var req addRosterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	personID, err := parseUUIDParam(req.PersonID, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	// Existence is not authorization: without this the roster was a way to attach any
	// Person id in the database to your own team and then read their name, email and
	// birthdate straight back out of GET /teams/{id}.
	if err := s.personVisibleTo(r.Context(), oc, personID); err != nil {
		writeError(w, err)
		return
	}
	joinedOn, err := parseDate(req.JoinedOn)
	if err != nil {
		writeError(w, err)
		return
	}

	membership, err := s.store.AddRosterMembership(r.Context(), store.AddRosterMembershipParams{
		PersonID: personID, TeamID: team.ID, JerseyNumber: req.JerseyNumber,
		Position: req.Position, JoinedOn: joinedOn,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, errConflict("that person already has an active roster spot on this team"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": membership.ID, "personId": membership.PersonID, "teamId": membership.TeamID,
		"jerseyNumber": membership.JerseyNumber, "position": membership.Position,
		"joinedOn": dateStr(membership.JoinedOn), "status": membership.Status,
	})
}

// handleEndRoster closes a player's active membership (they left the team, or
// are being moved — the caller opens a new membership elsewhere).
func (s *Server) handleEndRoster(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapRosterManage, "you cannot change a roster in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	personID, err := pathUUID(r, "personId")
	if err != nil {
		writeError(w, err)
		return
	}
	membership, err := s.store.GetActiveRosterMembership(r.Context(), store.GetActiveRosterMembershipParams{
		PersonID: personID, TeamID: team.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("that person has no active roster spot on this team"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.EndRosterMembership(r.Context(), store.EndRosterMembershipParams{ID: membership.ID}); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCapability(r, authz.CapTeamDelete, "you cannot delete teams in this organization")
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteTeam(r.Context(), team.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- shared authorization helpers -----------------------------------------

// teamInOrg loads the team named in the path and verifies it belongs to the
// caller's active organization.
func (s *Server) teamInOrg(r *http.Request, oc orgContext) (store.Team, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return store.Team{}, err
	}
	return s.teamByIDInOrg(r.Context(), oc, id)
}

// teamByIDInOrg is teamInOrg for a team id that came from somewhere other than the
// path — a request body, or a form instance's subject.
func (s *Server) teamByIDInOrg(ctx context.Context, oc orgContext, id uuid.UUID) (store.Team, error) {
	team, err := s.store.GetTeam(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Team{}, errNotFound("team not found")
	} else if err != nil {
		return store.Team{}, err
	}
	if team.OrganizationID != oc.orgID {
		return store.Team{}, errForbidden("that team is not in your organization")
	}
	// Being in the org is the staff test. For a parent or player, the team must also be
	// one their household is on: without this, every team id in the club is readable by
	// anyone who holds the humblest membership in it.
	if oc.scope() == authz.ScopeOwn {
		ok, verr := s.store.TeamVisibleToHousehold(ctx, store.TeamVisibleToHouseholdParams{
			TeamID: team.ID, PersonID: personIDFrom(ctx),
		})
		if verr != nil {
			return store.Team{}, verr
		}
		if !ok {
			return store.Team{}, errNotFound("team not found")
		}
	}
	return team, nil
}

// personVisibleTo reports whether the caller's organization may see a Person, and is
// the check the person, roster and evaluation endpoints share.
//
// A Person is visible when they hold a membership in the caller's org, are rostered on
// one of its teams, or are the caller themselves. Everything else is a 404 rather than
// a 403: these ids are not enumerable, and answering "forbidden" would confirm that a
// given id exists, which is itself a disclosure about someone in another club.
func (s *Server) personVisibleTo(ctx context.Context, oc orgContext, personID uuid.UUID) error {
	if personID == personIDFrom(ctx) {
		return nil
	}
	var visible bool
	var err error
	switch oc.scope() {
	case authz.ScopeOrg:
		visible, err = s.store.PersonVisibleInOrg(ctx, store.PersonVisibleInOrgParams{
			PersonID: personID, OrganizationID: oc.orgID,
		})
	case authz.ScopeOwn:
		// A parent reaches their own children and stops there. Membership of the org is
		// what a coach's reach is built on and is exactly the wrong test here: every
		// parent in a club passes it, and these reads return other people's minors'
		// birthdates, contact details and medical notes.
		visible, err = s.store.PersonVisibleToGuardian(ctx, store.PersonVisibleToGuardianParams{
			PersonID: personID, GuardianPersonID: personIDFrom(ctx), OrganizationID: oc.orgID,
		})
	default:
		return errNotFound("person not found")
	}
	if err != nil {
		return err
	}
	if !visible {
		return errNotFound("person not found")
	}
	return nil
}

// rosterVisibleTo returns the set of person ids a caller may see inside a list they are
// already allowed to open, or nil when they may see all of it. Filtering a list in one
// pass beats a visibility query per row, and returning nil for staff keeps the common
// case free.
func (s *Server) rosterVisibleTo(ctx context.Context, oc orgContext) (map[uuid.UUID]bool, error) {
	if oc.scope() != authz.ScopeOwn {
		return nil, nil
	}
	self := personIDFrom(ctx)
	children, err := s.store.ListChildPersonIDs(ctx, self)
	if err != nil {
		return nil, err
	}
	allowed := make(map[uuid.UUID]bool, len(children)+1)
	allowed[self] = true
	for _, id := range children {
		allowed[id] = true
	}
	return allowed, nil
}
