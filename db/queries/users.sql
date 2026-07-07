-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name, position, skill_level)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: UpdateUserProfile :one
UPDATE users
SET display_name = COALESCE(sqlc.narg('display_name'), display_name),
    position     = CASE WHEN sqlc.arg('set_position')::bool THEN sqlc.narg('position') ELSE position END,
    skill_level  = COALESCE(sqlc.narg('skill_level'), skill_level),
    bio          = CASE WHEN sqlc.arg('set_bio')::bool THEN sqlc.narg('bio') ELSE bio END,
    avatar_url   = CASE WHEN sqlc.arg('set_avatar_url')::bool THEN sqlc.narg('avatar_url') ELSE avatar_url END,
    updated_at   = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ListPlayers :many
SELECT * FROM users
WHERE (sqlc.narg('q')::text IS NULL OR display_name ILIKE '%' || sqlc.narg('q') || '%')
  AND (sqlc.narg('position')::text IS NULL OR position = sqlc.narg('position'))
  AND (sqlc.narg('min_skill')::int IS NULL OR skill_level >= sqlc.narg('min_skill'))
ORDER BY display_name ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountPlayers :one
SELECT count(*) FROM users
WHERE (sqlc.narg('q')::text IS NULL OR display_name ILIKE '%' || sqlc.narg('q') || '%')
  AND (sqlc.narg('position')::text IS NULL OR position = sqlc.narg('position'))
  AND (sqlc.narg('min_skill')::int IS NULL OR skill_level >= sqlc.narg('min_skill'));

-- name: CareerStats :one
SELECT
    count(*)                              AS appearances,
    COALESCE(sum(goals), 0)::bigint       AS goals,
    COALESCE(sum(assists), 0)::bigint     AS assists,
    COALESCE(sum(yellow_cards), 0)::bigint AS yellow_cards,
    COALESCE(sum(red_cards), 0)::bigint   AS red_cards,
    COALESCE(sum(minutes_played), 0)::bigint AS minutes_played
FROM player_match_stats
WHERE user_id = $1;
