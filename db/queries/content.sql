-- Drills --------------------------------------------------------------------

-- name: CreateDrill :one
INSERT INTO drills (organization_id, author_person_id, name, description)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetDrill :one
SELECT * FROM drills WHERE id = $1;

-- name: ListDrillsInOrg :many
SELECT * FROM drills WHERE organization_id = $1 ORDER BY name ASC;

-- Sessions ------------------------------------------------------------------

-- name: CreateSession :one
INSERT INTO sessions (organization_id, author_person_id, team_id, title, scheduled_at, notes)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1;

-- name: ListSessionsInOrg :many
SELECT * FROM sessions
WHERE organization_id = $1
  AND (sqlc.narg('team_id')::uuid IS NULL OR team_id = sqlc.narg('team_id'))
ORDER BY scheduled_at DESC NULLS LAST, created_at DESC;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = $1;

-- name: CreateSessionBlock :one
INSERT INTO session_blocks (session_id, drill_id, title, duration_min, position, notes)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListSessionBlocks :many
SELECT sb.*, d.name AS drill_name
FROM session_blocks sb
LEFT JOIN drills d ON d.id = sb.drill_id
WHERE sb.session_id = $1
ORDER BY sb.position, sb.title;

-- Games (game day) ----------------------------------------------------------

-- name: CreateGame :one
INSERT INTO games (organization_id, team_id, opponent, kickoff_at, home_away)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetGame :one
SELECT * FROM games WHERE id = $1;

-- name: ListGamesForTeam :many
SELECT * FROM games WHERE team_id = $1 ORDER BY kickoff_at DESC NULLS LAST, created_at DESC;

-- name: UpdateGame :one
UPDATE games
SET opponent       = CASE WHEN sqlc.arg('set_opponent')::bool THEN sqlc.narg('opponent') ELSE opponent END,
    kickoff_at     = COALESCE(sqlc.narg('kickoff_at'), kickoff_at),
    home_away      = CASE WHEN sqlc.arg('set_home_away')::bool THEN sqlc.narg('home_away') ELSE home_away END,
    our_score      = CASE WHEN sqlc.arg('set_scores')::bool THEN sqlc.narg('our_score') ELSE our_score END,
    opponent_score = CASE WHEN sqlc.arg('set_scores')::bool THEN sqlc.narg('opponent_score') ELSE opponent_score END,
    status         = COALESCE(sqlc.narg('status'), status),
    updated_at     = now()
WHERE id = sqlc.arg('id')
RETURNING *;
