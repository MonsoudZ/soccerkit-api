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
-- Writes the record twice, for the reason UpdateTeam spells out: a pull returns
-- `payload`, so a column-only edit never reaches the phone and is overwritten by its
-- next push.
--
-- Only four of these columns are in the sync contract -- display_name, the two emergency
-- contact fields and medical_notes are what SyncUpsertPerson projects, so they are what
-- payload_patch may carry. given_name, family_name, birthdate, email and phone are REST's
-- alone: no push writes them and SyncTombstonePerson deliberately spares them, so they
-- need no propagation and must not be invented into a payload the app would not
-- recognise.
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
    payload                 = CASE WHEN sqlc.arg('patch_payload')::bool
                                   THEN COALESCE(payload, '{}'::jsonb) || sqlc.arg('payload_patch')::jsonb
                                   ELSE payload END,
    seq                     = nextval('sync_seq'),
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

-- name: ListOrganizationMembers :many
-- Everyone in an organization, one row per person with their roles gathered into an
-- array. Roles are rows -- UNIQUE (person_id, organization_id, role) -- so a person
-- holding both coach and parent is two memberships, and listing them unaggregated would
-- show that person twice with no way to tell it was one human.
--
-- Deliberately not a person read: display_name and email are what a member list needs,
-- and birthdate, phone and medical notes are not. Someone who may manage members is not
-- thereby entitled to every athlete's medical history.
SELECT p.id AS person_id, p.display_name, p.email,
       array_agg(m.role ORDER BY m.role)::text[] AS roles,
       bool_or(o.owner_person_id = p.id) AS is_owner
FROM memberships m
JOIN persons p ON p.id = m.person_id
JOIN organizations o ON o.id = m.organization_id
WHERE m.organization_id = $1 AND p.deleted = false
GROUP BY p.id, p.display_name, p.email
ORDER BY p.display_name ASC, p.id ASC;

-- name: ListRolesForPersonInOrg :many
-- One person's roles in one organization. Used before a role change, so the handler can
-- say what it is replacing and refuse to strip the last admin.
SELECT role FROM memberships
WHERE person_id = $1 AND organization_id = $2
ORDER BY role;

-- name: DeleteMembershipsForPersonInOrg :exec
-- Removes a person from an organization outright, every role at once. Roles are rows, so
-- "remove this member" is a delete of the set rather than of one of them.
DELETE FROM memberships WHERE person_id = $1 AND organization_id = $2;

-- name: CountOtherAdminsInOrg :one
-- How many admins an organization would still have without this person. The guard against
-- the one mistake that cannot be undone through the API: an org whose last admin demotes
-- or removes themselves has nobody left who can manage members, and no endpoint that can
-- put one back.
SELECT count(*) FROM memberships
WHERE organization_id = $1 AND role = 'admin' AND person_id <> $2;

-- name: GetPersonIDByAccountEmail :one
-- Resolves the address a member is invited by to the Person it belongs to.
--
-- Addressing by email rather than by person id is the authorization: a person id is an
-- opaque handle that says nothing about whether the caller has any business adding its
-- owner, whereas knowing someone's sign-in address is the ordinary evidence that you
-- know them. It also keeps this endpoint from becoming a way to sweep person ids and
-- pull each one into an org to read it.
--
-- user_accounts.email, not persons.email: the former is the verified Apple claim and is
-- UNIQUE, the latter is editable contact information and neither.
SELECT ua.person_id FROM user_accounts ua WHERE ua.email = $1;

-- name: IsGuardianOf :one
-- Whether one person is a recorded guardian of another. This is what makes the parent
-- role mean something narrower than "member of the org": a parent sees their own
-- children and nobody else's.
SELECT EXISTS (
    SELECT 1 FROM guardianships
     WHERE guardian_person_id = $1 AND child_person_id = $2
);

-- name: ListGuardiansForChild :many
SELECT p.* FROM guardianships g
JOIN persons p ON p.id = g.guardian_person_id
WHERE g.child_person_id = $1 AND p.deleted = false
ORDER BY p.display_name ASC;

-- name: DeleteGuardianship :exec
DELETE FROM guardianships
WHERE guardian_person_id = $1 AND child_person_id = $2;

-- name: UpdateOrganization :one
-- Only the name. `kind` is deliberately not editable here: handleDeleteMe deletes the
-- caller's *personal* orgs and orphans their clubs, so flipping kind would quietly
-- change what account deletion destroys. That is a product decision with a data-loss
-- edge, not a field on a PATCH body.
UPDATE organizations
SET name = COALESCE(sqlc.narg('name'), name),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: PersonHasUserAccount :one
-- Whether this Person can sign in. PATCH /persons/{id} uses it to keep a coach's edit
-- rights to the loginless athletes they manage, the same population POST /persons can
-- create -- someone with an account edits their own row.
SELECT EXISTS (
    SELECT 1 FROM user_accounts WHERE person_id = $1
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
-- Whether the caller may see a Person at all: they are not tombstoned, and one of three
-- things is true -- they hold a membership in the caller's org, they are rostered on one
-- of its live teams, or the caller pushed them through sync themselves. The roster arm
-- matters because an athlete can be added to a team without a membership row of their
-- own.
--
-- This is the *organization* arm only. Sync ownership used to be a third disjunct here;
-- it is PersonOwnedBySyncAccount now, because the two answer different questions and the
-- role rules ask them of different callers. Staff get this one -- they run the club, so
-- they see everyone in it. A parent does not, and would have been handed the whole club
-- by the membership arm.
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

-- name: PersonOwnedBySyncAccount :one
-- Whether this Person is a row the caller pushed through sync themselves.
--
-- Separate from the org arms and available to every role, because it is not a claim
-- about the organization: sync streams are per-account, so this matches only rows the
-- caller wrote and already holds on their own device. An athlete the app creates arrives
-- carrying no membership and no roster row, so without this the person endpoints answer
-- 404 for the athletes a coach actually has.
SELECT EXISTS (
    SELECT 1 FROM persons p
     WHERE p.id = sqlc.arg('person_id')
       AND p.deleted = false
       AND p.sync_account_id = sqlc.arg('viewer_person_id')
);
