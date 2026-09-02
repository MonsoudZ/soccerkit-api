-- name: CreateOrganization :one
-- owner_person_id is not optional in practice: it is what account deletion selects on,
-- and an org created without one can never be deleted by the person who made it.
INSERT INTO organizations (name, kind, owner_person_id) VALUES ($1, $2, $3) RETURNING *;

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
-- Sign in with Apple is the only path that creates an account, so an account is born
-- with its Apple subject and there is no credential of ours to store alongside it.
INSERT INTO user_accounts (person_id, email, apple_sub)
VALUES ($1, $2, $3)
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
-- Tie-broken on id: orgs created inside one transaction share a now() timestamp, and
-- resolveOrg takes the first row as the caller's default org when no X-Organization-ID
-- is sent. Without the tie-break that default is whatever Postgres happens to return.
ORDER BY o.created_at ASC, o.id ASC;

-- name: ListRolesInOrg :many
SELECT role FROM memberships WHERE person_id = $1 AND organization_id = $2;

-- name: HasMembership :one
SELECT EXISTS (
    SELECT 1 FROM memberships WHERE person_id = $1 AND organization_id = $2
);

-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (token_hash, user_account_id, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRefreshToken :one
SELECT * FROM refresh_tokens WHERE token_hash = $1;

-- name: RevokeRefreshToken :execrows
-- Rotation's single-use guard, not a follow-up to one. handleRefresh used to read the
-- row, decide it was live, and then revoke it unconditionally — a check-then-act with
-- nothing between the two statements, so concurrent presentations of one token all read
-- it before any of them wrote and every one of them minted a fresh family (measured: 6
-- live chains from 32 simultaneous redemptions of a single token). That is the exact
-- invariant reuse detection rests on: one token, one use, and a second use is evidence.
--
-- The predicate makes the write itself the arbiter. Zero rows means somebody else
-- redeemed this token between our read and our write, which the caller treats the same
-- way it treats a replay inside the grace window — microseconds apart is the retry case,
-- not the theft case.
UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: DeleteRefreshTokenByToken :exec
-- Logout removes the row outright rather than revoking it. A revoked row is the signal
-- that a token was rotated away, and presenting one again is treated as a replay; a
-- logged-out token must not look like that, or signing out on one device would cascade
-- every other device off too.
DELETE FROM refresh_tokens WHERE token_hash = $1;

-- name: ReapExpiredRefreshTokens :execrows
-- Delete refresh tokens that expired long enough ago to be of no further use.
--
-- Nothing ever deleted. RevokeRefreshToken and RevokeRefreshTokensForAccount stamp
-- revoked_at; logout removes its own row; everything else accumulated, one row per
-- sign-in and one per refresh, forever. Since password authentication was removed a
-- refresh token is the only credential this service stores, so the table is both the
-- fastest-growing thing here and the most sensitive. See docs/AUDIT-2.md L4.
--
-- The predicate is expires_at, not revoked_at, and that is the whole subtlety. A revoked
-- row is what replay detection reads: presenting an already-rotated token is evidence
-- the chain leaked, and the response is to revoke the family. Delete revoked rows
-- eagerly and a replay stops looking like a replay -- it looks like an unknown token,
-- answered with a plain 401, and the theft goes unremarked. Keying on expiry keeps every
-- revoked row for as long as the token it describes could still be presented, plus the
-- grace the caller passes.
DELETE FROM refresh_tokens
WHERE expires_at < now() - sqlc.arg('grace')::interval;

-- name: RevokeRefreshTokensForAccount :exec
-- Revoke every live token for one account. Used when a already-rotated token is
-- presented again: that is a replay, and the only safe reading is that the chain has
-- leaked, so the whole family goes rather than just the one token.
UPDATE refresh_tokens SET revoked_at = now()
WHERE user_account_id = $1 AND revoked_at IS NULL;

-- name: CreateGuardianship :one
INSERT INTO guardianships (guardian_person_id, child_person_id)
VALUES ($1, $2)
ON CONFLICT (guardian_person_id, child_person_id) DO NOTHING
RETURNING *;

-- name: ListChildren :many
-- p.deleted = false for the same reason every other person read has it: a tombstoned
-- Person is gone, and since a tombstone now clears its display columns it would list as
-- a blank row rather than a name. Nothing calls this yet -- guardianships are not
-- exposed -- which is exactly why it is worth fixing before something does.
SELECT p.* FROM guardianships g
JOIN persons p ON p.id = g.child_person_id
WHERE g.guardian_person_id = $1 AND p.deleted = false
ORDER BY p.display_name ASC;

-- name: ListOwnedPersonalOrgIDsForPerson :many
-- The personal org(s) this person owns, selected on organizations.owner_person_id.
--
-- This used to select on membership and argue that the two were the same thing —
-- "a personal org is created with its owner as sole member, so member == owner". That
-- held only because nothing could add a second member to an org. Once something can, a
-- plain member deleting their own account deletes the org out from under its owner. The
-- name said "ForPerson" and meant "belonging to"; it now means "owned by", which is why
-- it is renamed rather than quietly reworded.
--
-- Club orgs stay excluded even when the caller owns one: whether deleting a club
-- owner's account should destroy the club is a product decision that has not been made,
-- and the conservative answer — orphan it, leave the data — matches today's behaviour.
SELECT o.id
FROM organizations o
WHERE o.owner_person_id = $1 AND o.kind = 'personal';

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

-- name: CreatePersonWithID :one
-- Create a Person at an explicit id — the coach's deterministic account Person, so it
-- matches the id the app derives locally.
--
-- DO NOTHING, not DO UPDATE. This used to adopt whatever row was already at that id,
-- and the id is UUIDv5(a namespace constant published in this repo, apple_sub), so it is
-- computable by anyone who knows the subject. POST /sync lets any authenticated account
-- insert a persons row at an id of its choosing — SyncUpsertPerson's ownership guard
-- governs conflicts, and there is no conflict when the row does not exist yet — so an
-- attacker could pre-create the row and have the victim's first Apple sign-in adopt it,
-- sync_account_id and all. From then on the victim's own account Person was the
-- attacker's to rewrite, tombstone and pull.
--
-- Returning zero rows means the id is taken by a row we did not create. There is no
-- legitimate way for that to happen: authenticating as this coach requires /auth/apple,
-- so their own sync push cannot precede their own provisioning, and provisioning is one
-- transaction that commits whole or not at all. The caller treats it as a refusal.
INSERT INTO persons (id, display_name, email)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: PersonVisibleInOrg :one
-- Whether an organization may see a Person at all: they are not tombstoned, and they
-- either hold a membership in the org or are rostered on one of its live teams. The
-- roster arm matters because an athlete can be added to a team without a membership
-- row of their own.
SELECT EXISTS (
    SELECT 1 FROM persons p
     WHERE p.id = sqlc.arg('person_id')
       AND p.deleted = false
       AND (EXISTS (SELECT 1 FROM memberships m
                     WHERE m.person_id = p.id
                       AND m.organization_id = sqlc.arg('organization_id'))
         OR EXISTS (SELECT 1 FROM roster_memberships rm
                      JOIN teams t ON t.id = rm.team_id
                     WHERE rm.person_id = p.id
                       AND t.organization_id = sqlc.arg('organization_id')
                       AND t.deleted = false))
);
