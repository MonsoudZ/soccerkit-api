-- Sync projection (option b).
--
-- The iOS app syncs an opaque {type, id, payload} stream. Rather than a blob
-- store, we keep the payload ON the domain tables (queryable columns are
-- projected out of it), and fall back to a generic document table for types not
-- yet projected — so the client syncs losslessly today while types graduate into
-- their real tables over time.
--
-- Cursor: a single monotonic sequence stamps every synced write; a pull returns
-- rows with seq greater than the client's cursor, unioned across sources.
-- Scope: sync rows are owned by the pushing account (a Person). Rows created via
-- the REST API have a NULL sync_account_id and are invisible to sync — the two
-- write paths stay cleanly separated until a later reconciliation.

CREATE SEQUENCE sync_seq;

-- The sync spine, added to each projected domain table.
ALTER TABLE teams
    ADD COLUMN sync_account_id uuid REFERENCES persons (id) ON DELETE CASCADE,
    ADD COLUMN payload jsonb,
    ADD COLUMN deleted boolean NOT NULL DEFAULT false,
    ADD COLUMN seq bigint;
CREATE INDEX idx_teams_sync ON teams (sync_account_id, seq);

ALTER TABLE drills
    ADD COLUMN sync_account_id uuid REFERENCES persons (id) ON DELETE CASCADE,
    ADD COLUMN payload jsonb,
    ADD COLUMN deleted boolean NOT NULL DEFAULT false,
    ADD COLUMN seq bigint;
CREATE INDEX idx_drills_sync ON drills (sync_account_id, seq);

ALTER TABLE sessions
    ADD COLUMN sync_account_id uuid REFERENCES persons (id) ON DELETE CASCADE,
    ADD COLUMN payload jsonb,
    ADD COLUMN deleted boolean NOT NULL DEFAULT false,
    ADD COLUMN seq bigint;
CREATE INDEX idx_sessions_sync ON sessions (sync_account_id, seq);

-- Generic fallback for client types without a projected home yet (Player,
-- Diagram, Event, Game, Person, Organization, RosterMembership, ShareGrant,
-- FormTemplate, FormInstance, UserAccount, Prefs, …). Keyed by (account, type,
-- id) where id is the client's string id ("prefs" is not a UUID, so id is text).
CREATE TABLE sync_documents (
    sync_account_id uuid   NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    type            text   NOT NULL,
    id              text   NOT NULL,
    payload         jsonb,
    deleted         boolean NOT NULL DEFAULT false,
    seq             bigint  NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (sync_account_id, type, id)
);
CREATE INDEX idx_sync_documents_seq ON sync_documents (sync_account_id, seq);
