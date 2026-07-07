package api

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/monsoudz/soccerkit-api/internal/store"
)

type createLeagueRequest struct {
	Name   string `json:"name"`
	Season string `json:"season"`
}

func (s *Server) handleCreateLeague(w http.ResponseWriter, r *http.Request) {
	var req createLeagueRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" || req.Season == "" {
		writeError(w, errValidation("name and season are required"))
		return
	}
	league, err := s.store.CreateLeague(r.Context(), store.CreateLeagueParams{Name: req.Name, Season: req.Season})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, League{
		ID: league.ID, Name: league.Name, Season: league.Season, TeamCount: 0,
		CreatedAt: rfc3339(league.CreatedAt),
	})
}

func (s *Server) handleListLeagues(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListLeagues(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]League, len(rows))
	for i, l := range rows {
		out[i] = League{ID: l.ID, Name: l.Name, Season: l.Season, TeamCount: l.TeamCount, CreatedAt: rfc3339(l.CreatedAt)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetLeague(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	l, err := s.getLeagueDTO(r, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) getLeagueDTO(r *http.Request, id uuid.UUID) (League, error) {
	l, err := s.store.GetLeague(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return League{}, errNotFound("league not found")
	} else if err != nil {
		return League{}, err
	}
	return League{ID: l.ID, Name: l.Name, Season: l.Season, TeamCount: l.TeamCount, CreatedAt: rfc3339(l.CreatedAt)}, nil
}

type addLeagueTeamRequest struct {
	TeamID string `json:"teamId"`
}

func (s *Server) handleAddLeagueTeam(w http.ResponseWriter, r *http.Request) {
	leagueID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req addLeagueTeamRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	teamID, err := parseUUIDParam(req.TeamID, "teamId")
	if err != nil {
		writeError(w, err)
		return
	}

	if _, err := s.store.GetLeague(r.Context(), leagueID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("league not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetTeam(r.Context(), teamID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errBadRequest("teamId does not reference an existing team"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetLeagueTeam(r.Context(), store.GetLeagueTeamParams{LeagueID: leagueID, TeamID: teamID}); err == nil {
		writeError(w, errConflict("that team is already in the league"))
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, err)
		return
	}
	if err := s.store.AddLeagueTeam(r.Context(), store.AddLeagueTeamParams{LeagueID: leagueID, TeamID: teamID}); err != nil {
		writeError(w, err)
		return
	}
	l, err := s.getLeagueDTO(r, leagueID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

type createFixtureRequest struct {
	HomeTeamID string `json:"homeTeamId"`
	AwayTeamID string `json:"awayTeamId"`
	KickoffAt  string `json:"kickoffAt"`
}

func (s *Server) handleCreateFixture(w http.ResponseWriter, r *http.Request) {
	leagueID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req createFixtureRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	homeID, err := parseUUIDParam(req.HomeTeamID, "homeTeamId")
	if err != nil {
		writeError(w, err)
		return
	}
	awayID, err := parseUUIDParam(req.AwayTeamID, "awayTeamId")
	if err != nil {
		writeError(w, err)
		return
	}
	if homeID == awayID {
		writeError(w, errBadRequest("a team cannot play itself"))
		return
	}
	kickoff, err := time.Parse(time.RFC3339, req.KickoffAt)
	if err != nil {
		writeError(w, errValidation("kickoffAt must be an RFC3339 timestamp"))
		return
	}

	if _, err := s.store.GetLeague(r.Context(), leagueID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("league not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	for _, tid := range []uuid.UUID{homeID, awayID} {
		if _, err := s.store.GetLeagueTeam(r.Context(), store.GetLeagueTeamParams{LeagueID: leagueID, TeamID: tid}); errors.Is(err, pgx.ErrNoRows) {
			writeError(w, errBadRequest("team "+tid.String()+" is not registered in this league"))
			return
		} else if err != nil {
			writeError(w, err)
			return
		}
	}

	fixture, err := s.store.CreateFixture(r.Context(), store.CreateFixtureParams{
		LeagueID: leagueID, HomeTeamID: homeID, AwayTeamID: awayID, KickoffAt: timestamptz(kickoff),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	full, err := s.store.GetFixtureWithTeams(r.Context(), fixture.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fixtureRowDTO(full))
}

func (s *Server) handleListFixtures(w http.ResponseWriter, r *http.Request) {
	leagueID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetLeague(r.Context(), leagueID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("league not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.store.ListFixtures(r.Context(), leagueID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]Fixture, len(rows))
	for i, f := range rows {
		out[i] = Fixture{
			ID: f.ID, LeagueID: f.LeagueID, HomeTeamID: f.HomeTeamID, AwayTeamID: f.AwayTeamID,
			HomeTeamName: f.HomeTeamName, AwayTeamName: f.AwayTeamName, KickoffAt: rfc3339(f.KickoffAt),
			HomeScore: f.HomeScore, AwayScore: f.AwayScore, Status: f.Status,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type recordResultRequest struct {
	HomeScore *int32 `json:"homeScore"`
	AwayScore *int32 `json:"awayScore"`
}

func (s *Server) handleRecordResult(w http.ResponseWriter, r *http.Request) {
	fixtureID, err := pathUUID(r, "fixtureId")
	if err != nil {
		writeError(w, err)
		return
	}
	var req recordResultRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.HomeScore == nil || req.AwayScore == nil || *req.HomeScore < 0 || *req.AwayScore < 0 {
		writeError(w, errValidation("homeScore and awayScore must be non-negative integers"))
		return
	}
	if _, err := s.store.GetFixture(r.Context(), fixtureID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("fixture not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.RecordFixtureResult(r.Context(), store.RecordFixtureResultParams{
		ID: fixtureID, HomeScore: req.HomeScore, AwayScore: req.AwayScore,
	}); err != nil {
		writeError(w, err)
		return
	}
	full, err := s.store.GetFixtureWithTeams(r.Context(), fixtureID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fixtureRowDTO(full))
}

// handleStandings computes the league table from completed fixtures.
func (s *Server) handleStandings(w http.ResponseWriter, r *http.Request) {
	leagueID, err := pathUUID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.store.GetLeague(r.Context(), leagueID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, errNotFound("league not found"))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}

	teams, err := s.store.ListLeagueTeams(r.Context(), leagueID)
	if err != nil {
		writeError(w, err)
		return
	}
	table := make(map[uuid.UUID]*StandingRow, len(teams))
	order := make([]uuid.UUID, 0, len(teams))
	for _, t := range teams {
		table[t.TeamID] = &StandingRow{TeamID: t.TeamID, TeamName: t.TeamName}
		order = append(order, t.TeamID)
	}

	fixtures, err := s.store.ListCompletedFixtures(r.Context(), leagueID)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, f := range fixtures {
		home, away := table[f.HomeTeamID], table[f.AwayTeamID]
		if home == nil || away == nil || f.HomeScore == nil || f.AwayScore == nil {
			continue
		}
		hs, as := int(*f.HomeScore), int(*f.AwayScore)
		home.Played++
		away.Played++
		home.GoalsFor += hs
		home.GoalsAgainst += as
		away.GoalsFor += as
		away.GoalsAgainst += hs
		switch {
		case hs > as:
			home.Won++
			home.Points += 3
			away.Lost++
		case hs < as:
			away.Won++
			away.Points += 3
			home.Lost++
		default:
			home.Drawn++
			away.Drawn++
			home.Points++
			away.Points++
		}
	}

	rows := make([]StandingRow, 0, len(order))
	for _, id := range order {
		row := table[id]
		row.GoalDifference = row.GoalsFor - row.GoalsAgainst
		rows = append(rows, *row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Points != b.Points {
			return a.Points > b.Points
		}
		if a.GoalDifference != b.GoalDifference {
			return a.GoalDifference > b.GoalDifference
		}
		if a.GoalsFor != b.GoalsFor {
			return a.GoalsFor > b.GoalsFor
		}
		return a.TeamName < b.TeamName
	})
	writeJSON(w, http.StatusOK, rows)
}

func fixtureRowDTO(f store.GetFixtureWithTeamsRow) Fixture {
	return Fixture{
		ID: f.ID, LeagueID: f.LeagueID, HomeTeamID: f.HomeTeamID, AwayTeamID: f.AwayTeamID,
		HomeTeamName: f.HomeTeamName, AwayTeamName: f.AwayTeamName, KickoffAt: rfc3339(f.KickoffAt),
		HomeScore: f.HomeScore, AwayScore: f.AwayScore, Status: f.Status,
	}
}
