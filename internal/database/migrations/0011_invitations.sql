-- Invitations: joining an organization becomes something the invitee agrees to.
--
-- POST /organizations/{id}/members added a person the moment the call succeeded. The
-- address requirement stood in for consent -- you had to know someone's sign-in address
-- to add them -- but that is not consent, it is an obstacle. Being enrolled in a club
-- means that club's staff can read your Person, and it should take a yes.
--
-- No token, and that is deliberate rather than a shortcut around having no mail path. A
-- token is a bearer credential: it has to be generated, transported, stored and expired,
-- and anyone who sees it in a forwarded message is the invitee. Instead an invitation
-- names an address, and the invitee finds it by signing in -- the match is against
-- user_accounts.email, which is a claim Apple verified, so only the real owner of the
-- address can see or accept it. That also means an invitation can be written for someone
-- who has no account yet: it waits, and is there when they first sign in.
--
-- email is stored lower-cased, matching how appleEmail normalises the address it writes
-- to user_accounts, so the lookup is a plain equality rather than a case-folding join.
CREATE TABLE invitations (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id      uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    email                text NOT NULL,
    -- The roles acceptance grants. Held as an array rather than rows because an
    -- invitation is one decision about one set: accepting half of it is not a state
    -- anything here wants to represent.
    roles                text[] NOT NULL,
    invited_by_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,
    status               text NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'accepted', 'declined', 'revoked')),
    expires_at           timestamptz NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    responded_at         timestamptz,
    -- The same vocabulary the memberships CHECK enforces, applied to every element. A
    -- role that reaches this column is one the accept path would hand out.
    CONSTRAINT invitations_roles_valid CHECK (
        array_length(roles, 1) >= 1
        AND roles <@ ARRAY['admin', 'director', 'coach', 'parent', 'player']::text[]
    )
);

-- One live invitation per address per organization. Partial, so a declined or revoked
-- one does not block a fresh attempt -- a coach who mistyped a role should be able to
-- revoke and re-send rather than being told the address is taken forever.
CREATE UNIQUE INDEX idx_invitations_pending_unique
    ON invitations (organization_id, email)
    WHERE status = 'pending';

-- The invitee's lookup: every pending invitation for the address they signed in with.
CREATE INDEX idx_invitations_email ON invitations (email) WHERE status = 'pending';
CREATE INDEX idx_invitations_org ON invitations (organization_id);
