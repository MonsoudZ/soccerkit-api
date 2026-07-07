-- name: CreateMatch :one
INSERT INTO matches (host_id, venue_id, title, description, format, max_players, kickoff_at, duration_min)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetMatch :one
SELECT * FROM matches WHERE id = $1;

-- name: ListMatches :many
SELECT m.*,
    (SELECT count(*) FROM match_rsvps r WHERE r.match_id = m.id AND r.status = 'GOING')::bigint AS going_count
FROM matches m
WHERE (sqlc.narg('status')::text IS NULL OR m.status = sqlc.narg('status'))
  AND (sqlc.narg('venue_id')::uuid IS NULL OR m.venue_id = sqlc.narg('venue_id'))
  AND (sqlc.narg('host_id')::uuid IS NULL OR m.host_id = sqlc.narg('host_id'))
  AND (NOT sqlc.arg('upcoming_only')::bool OR m.kickoff_at >= now())
ORDER BY m.kickoff_at ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountMatchGoing :one
SELECT count(*)::bigint FROM match_rsvps WHERE match_id = $1 AND status = 'GOING';

-- name: UpdateMatch :one
UPDATE matches
SET title        = COALESCE(sqlc.narg('title'), title),
    description  = CASE WHEN sqlc.arg('set_description')::bool THEN sqlc.narg('description') ELSE description END,
    format       = COALESCE(sqlc.narg('format'), format),
    max_players  = COALESCE(sqlc.narg('max_players'), max_players),
    kickoff_at   = COALESCE(sqlc.narg('kickoff_at'), kickoff_at),
    duration_min = COALESCE(sqlc.narg('duration_min'), duration_min),
    venue_id     = CASE WHEN sqlc.arg('set_venue_id')::bool THEN sqlc.narg('venue_id') ELSE venue_id END,
    status       = COALESCE(sqlc.narg('status'), status),
    updated_at   = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetMatchStatus :exec
UPDATE matches SET status = $2, updated_at = now() WHERE id = $1;

-- name: ListMatchRsvps :many
SELECT r.id, r.status, r.created_at,
    u.id AS user_id, u.email, u.display_name, u.position, u.skill_level, u.bio, u.avatar_url, u.created_at AS user_created_at
FROM match_rsvps r
JOIN users u ON u.id = r.user_id
WHERE r.match_id = $1 AND r.status <> 'DECLINED'
ORDER BY r.created_at ASC;

-- name: GetRsvp :one
SELECT * FROM match_rsvps WHERE match_id = $1 AND user_id = $2;

-- name: CountGoingExcludingUser :one
SELECT count(*)::bigint FROM match_rsvps
WHERE match_id = $1 AND status = 'GOING' AND user_id <> $2;

-- name: UpsertRsvp :one
INSERT INTO match_rsvps (match_id, user_id, status)
VALUES ($1, $2, $3)
ON CONFLICT (match_id, user_id)
DO UPDATE SET status = EXCLUDED.status, updated_at = now()
RETURNING *;

-- name: GetOldestWaitlisted :one
SELECT * FROM match_rsvps
WHERE match_id = $1 AND status = 'WAITLIST'
ORDER BY created_at ASC
LIMIT 1;

-- name: SetRsvpStatus :exec
UPDATE match_rsvps SET status = $2, updated_at = now() WHERE id = $1;
