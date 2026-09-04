-- name: UpsertDeviceToken :one
-- Registering is idempotent and re-points the token at whoever is signed in now. See
-- 0012_device_tokens.sql for why the token owns the row rather than the person.
INSERT INTO device_tokens (token, person_id, platform)
VALUES ($1, $2, $3)
ON CONFLICT (token) DO UPDATE
SET person_id = EXCLUDED.person_id, platform = EXCLUDED.platform, last_seen_at = now()
RETURNING *;

-- name: DeleteDeviceToken :execrows
-- Scoped to the owner, so holding a token is not enough to unregister someone else's
-- device. Sign-out is the ordinary caller.
DELETE FROM device_tokens WHERE token = $1 AND person_id = $2;

-- name: DeleteDeviceTokenAnyOwner :exec
-- Pruning after Apple rejects a token, which is not scoped to a person on purpose: the
-- rejection says this token is dead everywhere, and the row may well belong to whoever
-- registered it before the device changed hands.
DELETE FROM device_tokens WHERE token = $1;

-- name: ListDeviceTokensForPerson :many
SELECT token FROM device_tokens WHERE person_id = $1 ORDER BY last_seen_at DESC;
