package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

type createTeamRequest struct {
	Name     string  `json:"name"`
	AgeGroup *string `json:"ageGroup"`
	Season   *string `json:"season"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
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
	// The id is minted here rather than by the column default, because the payload has
	// to carry it and the app requires it. See CreateTeam in db/queries/teams.sql.
	teamID := uuid.New()
	account := personIDFrom(r.Context())
	team, err := s.store.CreateTeam(r.Context(), store.CreateTeamParams{
		ID: teamID, OrganizationID: oc.orgID, SyncAccountID: &account,
		Name: req.Name, AgeGroup: req.AgeGroup, Season: req.Season,
		Payload: newTeamPayload(teamID, req.Name, req.AgeGroup, req.Season),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, teamDTO(team, 0))
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListTeamsInOrg(r.Context(), oc.orgID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Team, len(rows))
	for i, t := range rows {
		out[i] = teamDTO(store.Team{
			ID: t.ID, OrganizationID: t.OrganizationID, Name: t.Name, AgeGroup: t.AgeGroup,
			Season: t.Season, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		}, t.ActiveRosterCount)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.resolveOrg(r)
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
	entries, err := s.visibleRoster(r.Context(), oc, roster)
	if err != nil {
		writeError(w, err)
		return
	}
	// The count is the squad's, not the caller's slice of it: how many players a team
	// carries is not a disclosure about any of them, and a parent seeing "1" for a team
	// of fifteen would be wrong rather than private.
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
	oc, err := s.requireCoach(r)
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
	oc, err := s.requireCoach(r)
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
	oc, err := s.requireCoach(r)
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

func (s *Server) requireCoach(r *http.Request) (orgContext, error) {
	oc, err := s.resolveOrg(r)
	if err != nil {
		return orgContext{}, err
	}
	if !oc.hasAnyRole("admin", "director", "coach") {
		return orgContext{}, errForbidden("only coaches can do that")
	}
	return oc, nil
}

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
	return team, nil
}

// personVisibleTo reports whether the caller may see a Person, and is the check the
// person, roster and evaluation endpoints share. These endpoints return birthdate,
// contact details and medical notes, so an ungated read is a disclosure of a minor's
// PII.
//
// It used to ask one question -- is the subject in the caller's organization -- and
// never what the caller was. Every member therefore saw every athlete: a parent could
// read another family's medical notes, and a player their whole squad's contact
// details. Nothing exploited it because the roles that would have suffered it could not
// be granted to anyone able to log in; member management is what makes it reachable, so
// the gate lands in the same change.
//
// The rules, in the order they are asked:
//
//   - Yourself, always.
//   - Rows you pushed through sync, whatever role you hold. That is per-account and not
//     per-org, so it is a statement about your own data rather than about the club.
//   - Staff (admin, director, coach) see the organization they run: anyone holding a
//     membership in it, or rostered on one of its teams.
//   - A parent sees the children they are a recorded guardian of, and no others.
//   - A player sees themselves, which the first rule already answered.
//
// Everything else is 404 rather than 403: these ids are not enumerable, and answering
// "forbidden" would confirm that a given id exists, which is itself a disclosure about
// someone in another club.
func (s *Server) personVisibleTo(ctx context.Context, oc orgContext, personID uuid.UUID) error {
	caller := personIDFrom(ctx)
	if personID == caller {
		return nil
	}

	owned, err := s.store.PersonOwnedBySyncAccount(ctx, store.PersonOwnedBySyncAccountParams{
		PersonID: personID, ViewerPersonID: &caller,
	})
	if err != nil {
		return err
	}
	if owned {
		return nil
	}

	if oc.isStaff() {
		visible, err := s.store.PersonVisibleInOrg(ctx, store.PersonVisibleInOrgParams{
			PersonID: personID, OrganizationID: oc.orgID,
		})
		if err != nil {
			return err
		}
		if !visible {
			return errNotFound("person not found")
		}
		return nil
	}

	if oc.roles[roleParent] {
		isGuardian, err := s.store.IsGuardianOf(ctx, store.IsGuardianOfParams{
			GuardianPersonID: caller, ChildPersonID: personID,
		})
		if err != nil {
			return err
		}
		if isGuardian {
			return nil
		}
	}
	return errNotFound("person not found")
}

// updatableTeamFields is the set PATCH /teams/{id} accepts.
var updatableTeamFields = map[string]bool{"name": true, "ageGroup": true, "season": true}

// handleUpdateTeam renames a team and edits its age group and season. Teams could be
// created and deleted over REST but never edited, so a coach who mistyped a team name
// had to delete the team -- and its roster, games and sessions -- to fix it.
//
// All three fields are in the sync contract: SyncUpsertTeam projects exactly these out
// of the app's payload. So each one is written twice, into the column this API reads and
// into the payload a pull returns, which is what carries the edit to the phone. See
// UpdateTeam in db/queries/teams.sql for what a column-only write would lose.
func (s *Server) handleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	oc, err := s.requireCoach(r)
	if err != nil {
		writeError(w, err)
		return
	}
	team, err := s.teamInOrg(r, oc)
	if err != nil {
		writeError(w, err)
		return
	}
	raw, err := decodePatch(r, updatableTeamFields)
	if err != nil {
		writeError(w, err)
		return
	}

	params := store.UpdateTeamParams{ID: team.ID}
	patch := syncPatch{}
	if v, ok := raw["name"]; ok {
		name, verr := requiredString(v, "name")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.Name = &name
		patch.set("name", name)
	}
	if v, ok := raw["ageGroup"]; ok {
		ageGroup, verr := optionalString(v, "ageGroup")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetAgeGroup, params.AgeGroup = true, ageGroup
		patch.set("ageGroup", syncString(ageGroup))
	}
	if v, ok := raw["season"]; ok {
		season, verr := optionalString(v, "season")
		if verr != nil {
			writeError(w, verr)
			return
		}
		params.SetSeason, params.Season = true, season
		patch.set("season", syncString(season))
	}
	params.PayloadPatch, params.PatchPayload = patch.marshal()

	updated, err := s.store.UpdateTeam(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	roster, err := s.store.ListActiveRoster(r.Context(), updated.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, teamDTO(updated, int64(len(roster))))
}

// defaultTeamAccent is what the app itself gives a new team (AppStore+Entities.swift and
// TeamFormViewModel both settle on "Teal"), so a team created over REST looks like one
// created on the phone rather than announcing where it came from. accentName is a
// required key on the app's side with no default in its decoder, which is why the server
// has to have an answer at all.
const defaultTeamAccent = "Teal"

// newTeamPayload builds the record the app will decode for a team created over REST.
//
// It writes exactly the keys Team's decoder requires -- id, name, ageGroup, season,
// accentName -- because Codable throws on a missing one and loses the whole team. The
// two nullable columns are written as "" rather than null for the reason syncString
// gives: "" is the app's spelling of empty, and null would fail to decode into a
// non-optional String. An empty ageGroup is understood on the far side, which resolves
// an unrecognised value to the nearest band it knows rather than throwing.
func newTeamPayload(id uuid.UUID, name string, ageGroup, season *string) []byte {
	payload, err := json.Marshal(map[string]any{
		"id":         id.String(),
		"name":       name,
		"ageGroup":   syncString(ageGroup),
		"season":     syncString(season),
		"accentName": defaultTeamAccent,
	})
	if err != nil {
		// Four strings and a UUID cannot fail to marshal. A nil payload would still
		// insert; it would just pull as a record the client discards.
		return nil
	}
	return payload
}

// visibleRoster narrows a team's roster to the entries the caller may see.
//
// Staff see the squad. Everyone else sees themselves and their own children, because the
// roster carries each athlete's name, email and birthdate -- handing the whole list to a
// parent is the same disclosure personVisibleTo refuses one id at a time, and it was the
// easier of the two to reach: one GET returned the lot.
//
// The children are loaded once rather than asked per row. personVisibleTo would answer
// each entry correctly, but at a query per athlete on a page that has already read the
// whole squad.
func (s *Server) visibleRoster(ctx context.Context, oc orgContext, rows []store.ListActiveRosterRow) ([]RosterEntry, error) {
	entries := make([]RosterEntry, 0, len(rows))
	if oc.isStaff() {
		for _, row := range rows {
			entries = append(entries, rosterRowDTO(row))
		}
		return entries, nil
	}

	caller := personIDFrom(ctx)
	mine := map[uuid.UUID]bool{caller: true}
	if oc.roles[roleParent] {
		children, err := s.store.ListChildren(ctx, caller)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			mine[child.ID] = true
		}
	}
	for _, row := range rows {
		if mine[row.PersonID] {
			entries = append(entries, rosterRowDTO(row))
		}
	}
	return entries, nil
}
