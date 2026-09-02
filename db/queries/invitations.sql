-- name: CreateInvitation :one
INSERT INTO invitations (organization_id, token_hash, role, email, note, invited_by_person_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: AddInvitationChild :exec
INSERT INTO invitation_children (invitation_id, child_person_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: GetInvitationByTokenHash :one
-- Redemption looks an invitation up by the hash of the token presented, never by id: the
-- token is the only thing that proves the caller was invited to anything.
SELECT * FROM invitations WHERE token_hash = $1;

-- name: GetInvitation :one
SELECT * FROM invitations WHERE id = $1;

-- name: ListInvitationsInOrg :many
-- Newest first. Accepted and revoked rows are kept and returned: "who did we invite, and
-- what came of it" is the question this list answers, and dropping the answer as soon as
-- it arrives is how a club invites the same parent four times.
SELECT * FROM invitations
WHERE organization_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListInvitationChildRefs :many
-- The children named on one or more invitations, id and name only — enough for "join
-- Riverside FC as a parent of Sam Smith", and no more of a minor's record than that
-- sentence needs. Takes an array so listing a page of invitations is one query rather
-- than one per row.
SELECT ic.invitation_id, p.id AS person_id, p.display_name
FROM invitation_children ic
JOIN persons p ON p.id = ic.child_person_id
WHERE ic.invitation_id = ANY(@invitation_ids::uuid[]) AND p.deleted = false
ORDER BY p.display_name ASC;

-- name: AcceptInvitation :execrows
-- The write is the arbiter of whether this invitation was still redeemable.
--
-- Reading the row, deciding it is live, and then updating it is a check-then-act with
-- nothing between the two statements: two devices opening the same link — the ordinary
-- case, not an attack, since people forward these — would both read "pending" and both
-- redeem. Every condition that makes an invitation usable is therefore in the predicate,
-- and zero rows affected means somebody (possibly the same person twice) got there first.
UPDATE invitations
SET accepted_at = now(), accepted_by_person_id = $2
WHERE id = $1
  AND accepted_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > now();

-- name: RevokeInvitation :execrows
-- Revoking an invitation that was already accepted must not un-accept it, so the
-- predicate excludes those rather than the handler reading first and hoping.
UPDATE invitations
SET revoked_at = now()
WHERE id = $1 AND organization_id = $2 AND accepted_at IS NULL AND revoked_at IS NULL;
