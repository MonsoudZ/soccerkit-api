-- name: CreateTeam :one
INSERT INTO teams (organization_id, name, age_group, season)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetTeam :one
SELECT * FROM teams WHERE id = $1 AND deleted = false;

-- name: ListTeamsInOrg :many
SELECT t.*,
    (SELECT count(*) FROM roster_memberships r WHERE r.team_id = t.id AND r.left_on IS NULL)::bigint AS active_roster_count
FROM teams t
WHERE t.organization_id = $1 AND t.deleted = false
ORDER BY t.name ASC;

-- name: UpdateTeam :one
-- A REST edit has to reach the app, which is why this writes the record twice.
--
-- A sync pull returns `payload`, not these columns, so updating the columns alone would
-- leave the edit invisible on the phone -- and the app's next push, built from its own
-- unchanged copy, would write the old values straight back over it. The change would
-- disappear with nothing logged and nobody told.
--
-- So the caller passes `payload_patch`: the same fields, keyed the way the app's own
-- record keys them, merged over whatever payload is already there. `||` merges only the
-- keys present, so an untouched field keeps its value and a field the server does not
-- project is preserved rather than dropped -- which is what keeps a newer app's extra
-- fields alive across an edit made by an older server.
--
-- seq is bumped unconditionally. A row with a NULL sync_account_id is invisible to sync
-- and the new seq is simply unread; for a synced row, an edit that did not move the
-- cursor is an edit no device would ever ask for.
UPDATE teams
SET name       = COALESCE(sqlc.narg('name'), name),
    age_group  = CASE WHEN sqlc.arg('set_age_group')::bool THEN sqlc.narg('age_group') ELSE age_group END,
    season     = CASE WHEN sqlc.arg('set_season')::bool THEN sqlc.narg('season') ELSE season END,
    payload    = CASE WHEN sqlc.arg('patch_payload')::bool
                      THEN COALESCE(payload, '{}'::jsonb) || sqlc.arg('payload_patch')::jsonb
                      ELSE payload END,
    seq        = nextval('sync_seq'),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteTeam :exec
-- A REST delete tombstones rather than dropping the row, so the deletion reaches sync
-- clients. A hard DELETE produced no row for ListSyncChangesSince to return, so a device
-- holding the team was never told it was gone and re-created it on its next push.
-- Clears the row's content for the same reason SyncTombstoneTeam does: a tombstone
-- needs the id, the flag and a seq, and nothing else. See docs/AUDIT-5.md L1.
UPDATE teams SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    name = '', age_group = NULL, season = NULL, payload = NULL
WHERE id = $1;

-- Roster (time-bounded memberships) ----------------------------------------

-- name: AddRosterMembership :one
INSERT INTO roster_memberships (person_id, team_id, jersey_number, position, joined_on)
VALUES ($1, $2, $3, $4, COALESCE(sqlc.narg('joined_on'), CURRENT_DATE))
RETURNING *;

-- name: GetActiveRosterMembership :one
SELECT * FROM roster_memberships
WHERE person_id = $1 AND team_id = $2 AND left_on IS NULL;

-- name: ListActiveRoster :many
SELECT r.id, r.jersey_number, r.position, r.joined_on, r.status,
    p.id AS person_id, p.display_name, p.email, p.birthdate
FROM roster_memberships r
JOIN persons p ON p.id = r.person_id
WHERE r.team_id = $1 AND r.left_on IS NULL AND p.deleted = false
ORDER BY r.jersey_number NULLS LAST, p.display_name ASC;

-- name: EndRosterMembership :one
UPDATE roster_memberships
SET left_on = COALESCE(sqlc.narg('left_on'), CURRENT_DATE), status = 'inactive', updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ListTeamsForPerson :many
SELECT t.id AS team_id, t.name AS team_name, r.jersey_number, r.position, r.joined_on, r.left_on
FROM roster_memberships r
JOIN teams t ON t.id = r.team_id
WHERE r.person_id = $1
ORDER BY r.joined_on DESC;
