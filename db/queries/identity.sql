-- name: CreateOrganization :one
INSERT INTO organizations (name, kind) VALUES ($1, $2) RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations WHERE id = $1;

-- name: CreatePerson :one
INSERT INTO persons (display_name, given_name, family_name, birthdate, email, phone,
    emergency_contact_name, emergency_contact_phone, medical_notes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetPerson :one
SELECT * FROM persons WHERE id = $1;

-- name: UpdatePerson :one
UPDATE persons
SET display_name            = COALESCE(sqlc.narg('display_name'), display_name),
    given_name              = CASE WHEN sqlc.arg('set_given_name')::bool THEN sqlc.narg('given_name') ELSE given_name END,
    family_name             = CASE WHEN sqlc.arg('set_family_name')::bool THEN sqlc.narg('family_name') ELSE family_name END,
    birthdate               = CASE WHEN sqlc.arg('set_birthdate')::bool THEN sqlc.narg('birthdate') ELSE birthdate END,
    email                   = CASE WHEN sqlc.arg('set_email')::bool THEN sqlc.narg('email') ELSE email END,
    phone                   = CASE WHEN sqlc.arg('set_phone')::bool THEN sqlc.narg('phone') ELSE phone END,
    emergency_contact_name  = CASE WHEN sqlc.arg('set_ec_name')::bool THEN sqlc.narg('ec_name') ELSE emergency_contact_name END,
    emergency_contact_phone = CASE WHEN sqlc.arg('set_ec_phone')::bool THEN sqlc.narg('ec_phone') ELSE emergency_contact_phone END,
    medical_notes           = CASE WHEN sqlc.arg('set_medical')::bool THEN sqlc.narg('medical') ELSE medical_notes END,
    updated_at              = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: CreateUserAccount :one
INSERT INTO user_accounts (person_id, email, password_hash, apple_sub)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUserAccountByEmail :one
SELECT * FROM user_accounts WHERE email = $1;

-- name: GetUserAccountByID :one
SELECT * FROM user_accounts WHERE id = $1;

-- name: CreateMembership :one
INSERT INTO memberships (person_id, organization_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (person_id, organization_id, role) DO NOTHING
RETURNING *;

-- name: ListMembershipsForPerson :many
SELECT m.id, m.role, m.organization_id, o.name AS organization_name, o.kind AS organization_kind
FROM memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.person_id = $1
ORDER BY o.created_at ASC;

-- name: ListRolesInOrg :many
SELECT role FROM memberships WHERE person_id = $1 AND organization_id = $2;

-- name: HasMembership :one
SELECT EXISTS (
    SELECT 1 FROM memberships WHERE person_id = $1 AND organization_id = $2
);

-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (token, user_account_id, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRefreshToken :one
SELECT * FROM refresh_tokens WHERE token = $1;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1;

-- name: RevokeRefreshTokenByToken :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE token = $1 AND revoked_at IS NULL;

-- name: CreateGuardianship :one
INSERT INTO guardianships (guardian_person_id, child_person_id)
VALUES ($1, $2)
ON CONFLICT (guardian_person_id, child_person_id) DO NOTHING
RETURNING *;

-- name: ListChildren :many
SELECT p.* FROM guardianships g
JOIN persons p ON p.id = g.child_person_id
WHERE g.guardian_person_id = $1
ORDER BY p.display_name ASC;

-- name: ListPersonalOrgIDsForPerson :many
-- The personal org(s) this person owns. A personal org is created with its owner
-- as sole member (see handleRegister), so "member of a personal org" == "owns
-- it". Club orgs the caller merely belongs to are intentionally excluded: account
-- deletion removes the caller from the club (via their membership), not the club.
SELECT DISTINCT o.id
FROM memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.person_id = $1 AND o.kind = 'personal';

-- name: SelectOrphanedAthletePersonIDs :many
-- Athletes (Persons) whose ONLY organizational linkage is via the org(s) being
-- deleted. Deleting those orgs strips their membership/roster rows but leaves the
-- Person itself — name, birthdate, medical notes: minors' PII we are legally
-- required to erase (COPPA/GDPR). ON DELETE CASCADE never reaches these, so we
-- delete them explicitly. A person still linked to any org OUTSIDE the delete-set
-- survives (the shared-athlete / multi-org case). Excludes the caller's own
-- Person (deleted separately) and anyone synced by a different account.
WITH linked_in AS (
    SELECT m.person_id FROM memberships m
    WHERE m.organization_id = ANY(@org_ids::uuid[])
    UNION
    SELECT rm.person_id FROM roster_memberships rm
    JOIN teams t ON t.id = rm.team_id
    WHERE t.organization_id = ANY(@org_ids::uuid[])
),
linked_out AS (
    SELECT m.person_id FROM memberships m
    WHERE m.organization_id <> ALL(@org_ids::uuid[])
    UNION
    SELECT rm.person_id FROM roster_memberships rm
    JOIN teams t ON t.id = rm.team_id
    WHERE t.organization_id <> ALL(@org_ids::uuid[])
)
SELECT p.id
FROM persons p
WHERE p.id IN (SELECT person_id FROM linked_in)
  AND p.id NOT IN (SELECT person_id FROM linked_out)
  AND p.id <> @caller_person_id
  AND (p.sync_account_id IS NULL OR p.sync_account_id = @caller_person_id);

-- name: DeletePersonsByIDs :exec
DELETE FROM persons WHERE id = ANY(@ids::uuid[]);

-- name: DeleteOrganizationsByIDs :exec
DELETE FROM organizations WHERE id = ANY(@ids::uuid[]);

-- name: DeletePersonByID :exec
DELETE FROM persons WHERE id = $1;

-- name: GetUserAccountByAppleSub :one
SELECT * FROM user_accounts WHERE apple_sub = $1;

-- name: LinkAppleSub :exec
UPDATE user_accounts SET apple_sub = $2, updated_at = now() WHERE id = $1;

-- name: CreatePersonWithID :one
-- Create (or adopt) a Person with an explicit id — used for the coach's
-- deterministic account Person so it matches the app's synced Person.
INSERT INTO persons (id, display_name, email)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET display_name = EXCLUDED.display_name, updated_at = now()
RETURNING *;

-- PersonVisibleInOrg reports whether a Person is reachable from an org. persons
-- has no organization_id: a Person is tied to an org only by a membership (an
-- athlete enrolled via POST /persons, or the coach themselves) or by the roster
-- of one of that org's teams. Those two edges are the whole visibility rule.
-- name: PersonVisibleInOrg :one
SELECT (
    EXISTS (
        SELECT 1 FROM memberships m
        WHERE m.person_id = sqlc.arg(person_id)
          AND m.organization_id = sqlc.arg(organization_id)
    )
    OR EXISTS (
        SELECT 1 FROM roster_memberships rm
        JOIN teams t ON t.id = rm.team_id
        WHERE rm.person_id = sqlc.arg(person_id)
          AND t.organization_id = sqlc.arg(organization_id)
    )
) AS visible;
