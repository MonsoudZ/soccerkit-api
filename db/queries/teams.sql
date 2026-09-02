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
UPDATE teams
SET name       = COALESCE(sqlc.narg('name'), name),
    age_group  = CASE WHEN sqlc.arg('set_age_group')::bool THEN sqlc.narg('age_group') ELSE age_group END,
    season     = CASE WHEN sqlc.arg('set_season')::bool THEN sqlc.narg('season') ELSE season END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteTeam :exec
-- A REST delete tombstones rather than dropping the row, so the deletion reaches sync
-- clients. A hard DELETE produced no row for ListSyncChangesSince to return, so a device
-- holding the team was never told it was gone and re-created it on its next push.
UPDATE teams SET deleted = true, seq = nextval('sync_seq'), updated_at = now()
WHERE id = $1;

-- Roster (time-bounded memberships) ----------------------------------------

-- name: AddRosterMembership :one
INSERT INTO roster_memberships (person_id, team_id, jersey_number, position, joined_on)
VALUES ($1, $2, $3, $4, COALESCE(sqlc.narg('joined_on'), CURRENT_DATE))
RETURNING *;

-- name: GetActiveRosterMembership :one
SELECT * FROM roster_memberships
WHERE person_id = $1 AND team_id = $2 AND left_on IS NULL;

-- name: GetRosterMembership :one
SELECT * FROM roster_memberships WHERE id = $1;

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

-- name: ListTeamsForHouseholdInOrg :many
-- The teams a parent or player may see: those in this organization that they, or a
-- child they are the guardian of, are actively rostered on. It is GET /teams for a
-- scoped caller — the org-wide ListTeamsInOrg is a staff query, and answering it for a
-- parent would hand them the club's whole team list.
SELECT t.*,
    (SELECT count(*) FROM roster_memberships r WHERE r.team_id = t.id AND r.left_on IS NULL)::bigint AS active_roster_count
FROM teams t
WHERE t.organization_id = @organization_id
  AND t.deleted = false
  AND EXISTS (
      SELECT 1 FROM roster_memberships r
       WHERE r.team_id = t.id
         AND r.left_on IS NULL
         AND (r.person_id = @person_id
              OR r.person_id IN (SELECT g.child_person_id FROM guardianships g
                                  WHERE g.guardian_person_id = @person_id))
  )
ORDER BY t.name ASC;

-- name: TeamVisibleToHousehold :one
-- The single-team form of ListTeamsForHouseholdInOrg, for reads keyed on a team id.
SELECT EXISTS (
    SELECT 1 FROM roster_memberships r
     WHERE r.team_id = @team_id
       AND r.left_on IS NULL
       AND (r.person_id = @person_id
            OR r.person_id IN (SELECT g.child_person_id FROM guardianships g
                                WHERE g.guardian_person_id = @person_id))
);
