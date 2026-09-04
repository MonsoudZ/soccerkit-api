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

-- name: ListReachablePeopleForTeam :many
-- Who to tell when a team's fixture is scheduled, moved or called off: the squad, and the
-- parents of the squad. It is the RSVP's audience -- the people the register is asking --
-- so it is the roster and its guardians rather than everyone who can see the team.
--
-- Filtered to people who have a device registered, which is a departure from how an
-- invitation notifies: that one tells a single named person and asks nothing about their
-- phone. A fixture fans out to a whole squad plus their families, and the delivery queue
-- is bounded and drained by one worker (see internal/push), so a coach entering a
-- season's fixtures in one sitting would otherwise push hundreds of deliveries for people
-- with nowhere to deliver to, and the drops would land on whoever was behind them.
--
-- The actor is excluded. A coach who just moved a kickoff does not need their own phone
-- to tell them they moved it.
WITH people AS (
    SELECT r.person_id FROM roster_memberships r
    WHERE r.team_id = @team_id AND r.left_on IS NULL
    UNION
    SELECT g.guardian_person_id AS person_id FROM guardianships g
    WHERE g.child_person_id IN (
        SELECT r.person_id FROM roster_memberships r
        WHERE r.team_id = @team_id AND r.left_on IS NULL
    )
)
SELECT p.id FROM people pe
JOIN persons p ON p.id = pe.person_id AND p.deleted = false
WHERE p.id <> @actor_person_id
  AND EXISTS (SELECT 1 FROM device_tokens d WHERE d.person_id = p.id);
