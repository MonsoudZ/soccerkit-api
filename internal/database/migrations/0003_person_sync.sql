-- Graduate Person into a projected sync type, so the coach's account Person
-- (created by /auth/apple) and the app's synced Person are one row.
--
-- The client and server both derive the coach's person id deterministically as
-- UUIDv5(namespace, apple_sub), so a pushed Person upserts the same persons row
-- the account links to — one identity, no id round-tripping, no migration.
--
-- persons has no NOT NULL foreign key to another synced entity, so it projects
-- cleanly (unlike games/form_instances/roster_memberships).

ALTER TABLE persons
    ADD COLUMN sync_account_id uuid REFERENCES persons (id) ON DELETE CASCADE,
    ADD COLUMN payload jsonb,
    ADD COLUMN deleted boolean NOT NULL DEFAULT false,
    ADD COLUMN seq bigint;

CREATE INDEX idx_persons_sync ON persons (sync_account_id, seq);
