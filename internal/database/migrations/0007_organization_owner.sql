-- Give an organization an explicit owner.
--
-- Nothing recorded who owned an org, so account deletion inferred it from membership:
-- ListPersonalOrgIDsForPerson selected every personal org the caller was *a member of*,
-- on this reasoning —
--
--   "A personal org is created with its owner as sole member (see handleRegister), so
--    'member of a personal org' == 'owns it'."
--
-- — which is true of the code that creates orgs and enforced nowhere. The moment a
-- second account can join an org (clubs, directors, assistant coaches: seam 1 of the
-- schema, and the next feature), a plain member deleting their own account takes the
-- whole organization with them: its teams, drills, sessions, templates, games, rosters
-- and orphaned athletes, irreversibly, with no indication to anyone that it happened.
--
-- Doing this now is a column and a backfill. Doing it after the invite endpoint ships is
-- the same column plus a data-loss incident.
--
-- ON DELETE SET NULL, not CASCADE: whether deleting a club owner's account should
-- destroy the club is a product question, and a cascade would answer it silently.
-- handleDeleteMe keeps its explicit, documented deletion of the caller's *personal*
-- orgs; a club org owned by the caller is orphaned rather than dropped, which is the
-- same thing that happens today and is visible in the column when it comes time to
-- decide.

ALTER TABLE organizations
    ADD COLUMN owner_person_id uuid REFERENCES persons (id) ON DELETE SET NULL;

-- Backfill from the admin membership, which is what handleRegister and
-- provisionAppleIdentity give the creator. Every existing personal org has exactly one
-- member, so this is unambiguous; the ORDER BY only makes it deterministic if that ever
-- stopped being true.
UPDATE organizations o
SET owner_person_id = (
    SELECT m.person_id
    FROM memberships m
    WHERE m.organization_id = o.id AND m.role = 'admin'
    ORDER BY m.created_at ASC, m.person_id ASC
    LIMIT 1
)
WHERE o.owner_person_id IS NULL;

CREATE INDEX idx_organizations_owner ON organizations (owner_person_id);
