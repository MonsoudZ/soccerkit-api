-- Invitations: how somebody who already has an account joins an organization that is
-- not their own.
--
-- Until now there was no way. POST /members refuses a person with no linkage to the org
-- — an id is not consent, and accepting one would make "grant a role" a way to read a
-- stranger's record — and POST /persons creates a Person with no login, which is the
-- right thing for a U9 athlete and useless for a coach who already signed in with Apple.
-- So the club tiers were unreachable: a director could not add a coach, and a parent
-- could not be connected to their own child.
--
-- An invitation is the consent that was missing. Staff issue one, the person who holds
-- it redeems it with their own authenticated account, and the membership is created for
-- the account that redeemed it — never for an id somebody typed.
--
-- The token is the credential, and it is treated like one. Only its SHA-256 is stored,
-- for the same reason refresh_tokens stores only a hash (see 0005): a copy of this
-- database is then not a set of working invitations. 32 bytes of entropy is what stands
-- between a link and a membership, so the token is never returned again after creation
-- and never logged.
--
-- Single use is enforced by the write, not by a read that precedes it: acceptance is a
-- conditional UPDATE on accepted_at IS NULL, so two devices redeeming the same link at
-- the same time produce one membership and one loser, rather than two of everything.

CREATE TABLE invitations (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id       uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- SHA-256 of the token, base64url. The token itself exists in exactly one place:
    -- the response that created it.
    token_hash            text NOT NULL UNIQUE,
    role                  text NOT NULL CHECK (role IN ('admin', 'director', 'coach', 'parent', 'player')),
    -- Optional binding to an address. When set, only an account whose Apple-verified
    -- address matches may redeem, which turns a leaked link into a dead one. It is
    -- optional because Hide My Email exists: an invitee who hides their address signs in
    -- with a private relay that will never match what the club typed, and a bound invite
    -- would lock out exactly the person it was meant for.
    email                 text,
    note                  text,
    invited_by_person_id  uuid REFERENCES persons (id) ON DELETE SET NULL,
    expires_at            timestamptz NOT NULL,
    accepted_at           timestamptz,
    accepted_by_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,
    revoked_at            timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_invitations_org ON invitations (organization_id, created_at DESC);

-- The children a parent invitation links its redeemer to.
--
-- This is what makes the parent tier work at all: a parent's reads are scoped to the
-- children they are the guardian of, so a parent membership with no guardianship sees
-- nothing. The link cannot be made after the fact by the parent themselves — that would
-- be a parent claiming somebody's child — so the coach who knows which child this is
-- names them when the invitation is issued, and redemption writes the guardianship.
CREATE TABLE invitation_children (
    invitation_id   uuid NOT NULL REFERENCES invitations (id) ON DELETE CASCADE,
    child_person_id uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    PRIMARY KEY (invitation_id, child_person_id)
);
