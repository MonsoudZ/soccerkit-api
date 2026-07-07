-- name: CreateTeam :one
INSERT INTO teams (name, crest_url, owner_id)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetTeam :one
SELECT * FROM teams WHERE id = $1;

-- name: ListTeams :many
SELECT t.*,
    (SELECT count(*) FROM team_members m WHERE m.team_id = t.id)::bigint AS member_count
FROM teams t
WHERE (sqlc.narg('q')::text IS NULL OR t.name ILIKE '%' || sqlc.narg('q') || '%')
ORDER BY t.name ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: UpdateTeam :one
UPDATE teams
SET name       = COALESCE(sqlc.narg('name'), name),
    crest_url  = CASE WHEN sqlc.arg('set_crest_url')::bool THEN sqlc.narg('crest_url') ELSE crest_url END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteTeam :exec
DELETE FROM teams WHERE id = $1;

-- name: CountTeamMembers :one
SELECT count(*)::bigint FROM team_members WHERE team_id = $1;

-- name: ListTeamMembers :many
SELECT m.id, m.role, m.jersey_number, m.joined_at,
    u.id AS user_id, u.email, u.display_name, u.position, u.skill_level, u.bio, u.avatar_url, u.created_at AS user_created_at
FROM team_members m
JOIN users u ON u.id = m.user_id
WHERE m.team_id = $1
ORDER BY m.joined_at ASC;

-- name: GetTeamMember :one
SELECT * FROM team_members WHERE team_id = $1 AND user_id = $2;

-- name: AddTeamMember :one
INSERT INTO team_members (team_id, user_id, role, jersey_number)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: DeleteTeamMember :execrows
DELETE FROM team_members WHERE team_id = $1 AND user_id = $2;
