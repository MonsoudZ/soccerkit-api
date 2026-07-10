-- Graduate three more client types out of the opaque sync_documents fallback
-- into dedicated, queryable tables: Player, Event (TeamEvent), Diagram
-- (TacticsDiagram). All three are homeless on the server (no prior domain table)
-- and reference other entities only softly (ids in the payload), so they project
-- cleanly with no foreign-key ordering constraints.
--
-- Each carries the standard sync spine (payload + seq + deleted + sync_account_id)
-- plus a few columns projected out of the payload for querying.

CREATE TABLE players (
    id              uuid PRIMARY KEY,
    sync_account_id uuid REFERENCES persons (id) ON DELETE CASCADE,
    person_id       uuid,           -- soft ref to a Person (syncs separately; no FK)
    name            text,
    number          int,
    position        text,
    payload         jsonb,
    deleted         boolean NOT NULL DEFAULT false,
    seq             bigint,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_players_sync ON players (sync_account_id, seq);

CREATE TABLE events (
    id              uuid PRIMARY KEY,
    sync_account_id uuid REFERENCES persons (id) ON DELETE CASCADE,
    team_id         uuid,           -- soft ref to a Team
    title           text,
    kind            text,
    payload         jsonb,
    deleted         boolean NOT NULL DEFAULT false,
    seq             bigint,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_events_sync ON events (sync_account_id, seq);

CREATE TABLE diagrams (
    id              uuid PRIMARY KEY,
    sync_account_id uuid REFERENCES persons (id) ON DELETE CASCADE,
    team_id         uuid,           -- soft ref to a Team
    title           text,
    payload         jsonb,
    deleted         boolean NOT NULL DEFAULT false,
    seq             bigint,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_diagrams_sync ON diagrams (sync_account_id, seq);
