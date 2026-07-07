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
