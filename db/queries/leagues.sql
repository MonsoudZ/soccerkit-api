-- name: CreateLeague :one
INSERT INTO leagues (name, season) VALUES ($1, $2) RETURNING *;

-- name: GetLeague :one
SELECT l.*,
    (SELECT count(*) FROM league_teams lt WHERE lt.league_id = l.id)::bigint AS team_count
FROM leagues l WHERE l.id = $1;

-- name: ListLeagues :many
SELECT l.*,
    (SELECT count(*) FROM league_teams lt WHERE lt.league_id = l.id)::bigint AS team_count
FROM leagues l
ORDER BY l.created_at DESC;

-- name: GetLeagueTeam :one
SELECT * FROM league_teams WHERE league_id = $1 AND team_id = $2;

-- name: AddLeagueTeam :exec
INSERT INTO league_teams (league_id, team_id) VALUES ($1, $2);

-- name: ListLeagueTeams :many
SELECT lt.team_id, t.name AS team_name
FROM league_teams lt
JOIN teams t ON t.id = lt.team_id
WHERE lt.league_id = $1
ORDER BY t.name ASC;

-- name: CreateFixture :one
INSERT INTO fixtures (league_id, home_team_id, away_team_id, kickoff_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetFixture :one
SELECT * FROM fixtures WHERE id = $1;

-- name: GetFixtureWithTeams :one
SELECT f.*, h.name AS home_team_name, a.name AS away_team_name
FROM fixtures f
JOIN teams h ON h.id = f.home_team_id
JOIN teams a ON a.id = f.away_team_id
WHERE f.id = $1;

-- name: ListFixtures :many
SELECT f.*, h.name AS home_team_name, a.name AS away_team_name
FROM fixtures f
JOIN teams h ON h.id = f.home_team_id
JOIN teams a ON a.id = f.away_team_id
WHERE f.league_id = $1
ORDER BY f.kickoff_at ASC;

-- name: RecordFixtureResult :one
UPDATE fixtures
SET home_score = $2, away_score = $3, status = 'COMPLETED', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListCompletedFixtures :many
SELECT * FROM fixtures
WHERE league_id = $1 AND status = 'COMPLETED'
  AND home_score IS NOT NULL AND away_score IS NOT NULL;
