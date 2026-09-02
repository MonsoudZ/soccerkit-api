-- name: ListSyncChangesSince :many
-- The delta an account hasn't seen: synced rows across every source, ordered by
-- the shared cursor. Projected tables contribute their type; sync_documents
-- carries its own.
--
-- One page of it, not all of it. This had no LIMIT, and a pull accumulated whatever it
-- returned into a single response: pushes are capped at maxSyncBatch records but nothing
-- caps how many pushes an account makes, so an account grows its own delta without bound
-- and then every full pull -- since=0, which is what a reinstall sends -- is an unbounded
-- allocation on the server.
--
-- seq comes from a single sequence and is unique across every source, so ordering by it
-- is total and a page boundary cannot fall inside a group of equal keys. That is what
-- makes "resume from the last seq I returned" safe: no row is skipped and none is sent
-- twice. The client drains by asking again until the cursor stops moving; nothing about
-- the wire format changes.
--
-- A page is bounded twice, by rows and by bytes, and both bounds are applied here.
--
-- The byte bound used to be applied in Go, after every row in the window had been read
-- off the wire and materialized. That bounded the response but not the read, and the two
-- bounds only balance at max_bytes/lim -- ~4.2 KiB per record at 2 MiB and 500. Above
-- that the byte bound binds first and the window is mostly waste: measured at 1 MiB
-- payloads, a pull allocated 513 MiB to return a single record. Cutting here means the
-- discarded rows never cross the wire. See docs/AUDIT-5.md M1.
--
-- rn = 1 keeps the rule that made the Go loop correct: the first row goes in whatever it
-- weighs. A payload above the whole budget would otherwise be skipped by every pull
-- forever, and because the cursor only advances over rows actually delivered the client
-- would ask for it again and again and never get past it. Running weights are
-- non-decreasing, so "the prefix that fits" is exactly what this filter selects.
--
-- Every branch has an index on (sync_account_id, seq), and the planner index-scans each
-- one and then top-N heapsorts into the window.
--
-- The ::bigint on max_bytes is load-bearing and not cosmetic. sqlc infers a parameter's
-- type from the column it is compared against, and it cannot resolve one that comes out
-- of a derived table carrying window functions -- without the cast, generation fails with
-- `table alias "budgeted" does not exist`. The cast tells it the type outright.
--
-- What this still does not buy, and it is the open half of M1: it does not stop early.
-- There is no merge-append plan available for this shape (checked with enable_sort off;
-- it sorts anyway), so each page reads the account's remaining delta to find the next
-- `lim` rows, and draining k pages costs k scans of a shrinking tail. When the byte bound
-- binds, k is much larger than rows/lim -- 500 pages for 500 one-MiB records -- and the
-- drain is quadratic in the row count. Bounding that needs a cap on a single record's
-- payload at push time, which is a wire change and is written up as M1 (3).
SELECT budgeted.type, budgeted.id, budgeted.payload, budgeted.deleted, budgeted.seq
FROM (
    -- Running weight of the page in wire bytes. octet_length over the rendered text is
    -- what the client actually receives; pg_column_size would be cheaper but reports the
    -- stored, possibly compressed size, and underestimating here means overshooting the
    -- budget -- the wrong direction for a limit whose job is to bound a response.
    SELECT page.type, page.id, page.payload, page.deleted, page.seq,
           (row_number() OVER (ORDER BY page.seq))::bigint                 AS rn,
           (sum(coalesce(octet_length(page.payload::text), 0))
               OVER (ORDER BY page.seq ROWS UNBOUNDED PRECEDING))::bigint   AS running
    FROM (
        -- The row window, and the point where a tombstone stops carrying its payload: a
        -- delete goes on the wire as {type, id}, so selecting the rest of it only spends
        -- the byte budget on bytes nobody receives. sync_documents already nulls a
        -- tombstoned payload on write; the seven projected tables keep theirs (see
        -- docs/AUDIT-5.md L1), so this is where that difference stops mattering.
        SELECT delta.type, delta.id, delta.deleted, delta.seq,
               (CASE WHEN delta.deleted THEN NULL ELSE delta.payload END)::jsonb AS payload
        FROM (
    SELECT 'Team'::text    AS type, t.id::text AS id, t.payload AS payload, t.deleted AS deleted, t.seq AS seq
        FROM teams    t WHERE t.sync_account_id = $1 AND t.seq > $2
    UNION ALL
    SELECT 'Drill'::text,   d.id::text, d.payload, d.deleted, d.seq
        FROM drills   d WHERE d.sync_account_id = $1 AND d.seq > $2
    UNION ALL
    SELECT 'Session'::text, s.id::text, s.payload, s.deleted, s.seq
        FROM sessions s WHERE s.sync_account_id = $1 AND s.seq > $2
    UNION ALL
    SELECT 'Person'::text, pe.id::text, pe.payload, pe.deleted, pe.seq
        FROM persons pe WHERE pe.sync_account_id = $1 AND pe.seq > $2
    UNION ALL
    SELECT 'Player'::text, pl.id::text, pl.payload, pl.deleted, pl.seq
        FROM players pl WHERE pl.sync_account_id = $1 AND pl.seq > $2
    UNION ALL
    SELECT 'Event'::text, ev.id::text, ev.payload, ev.deleted, ev.seq
        FROM events ev WHERE ev.sync_account_id = $1 AND ev.seq > $2
    UNION ALL
    SELECT 'Diagram'::text, dg.id::text, dg.payload, dg.deleted, dg.seq
        FROM diagrams dg WHERE dg.sync_account_id = $1 AND dg.seq > $2
    UNION ALL
    SELECT sd.type, sd.id, sd.payload, sd.deleted, sd.seq
        FROM sync_documents sd WHERE sd.sync_account_id = $1 AND sd.seq > $2
        ) delta
        ORDER BY delta.seq ASC
        LIMIT sqlc.arg('lim')
    ) page
) budgeted
WHERE budgeted.rn = 1 OR budgeted.running <= sqlc.arg('max_bytes')::bigint
ORDER BY budgeted.seq ASC;

-- Upserts are keyed on the client-supplied primary key, so each conflict clause is
-- guarded on the row's owner: a push naming a row this account does not own affects
-- zero rows, and the handler returns it to the client as a conflict. A NULL owner
-- (a REST-created row) fails the guard too — the separation 0002_sync.sql describes.
-- Ownership never transfers on update, so the SET clauses do not reassign
-- sync_account_id; SyncUpsertPerson is the one exception, see its comment.

-- name: SyncUpsertTeam :execrows
INSERT INTO teams (id, organization_id, sync_account_id, name, age_group, season, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name, age_group = EXCLUDED.age_group, season = EXCLUDED.season,
    payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now()
WHERE teams.sync_account_id = EXCLUDED.sync_account_id;

-- name: SyncUpsertDrill :execrows
INSERT INTO drills (id, organization_id, author_person_id, sync_account_id, name, description, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name, description = EXCLUDED.description,
    payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now()
WHERE drills.sync_account_id = EXCLUDED.sync_account_id;

-- name: SyncUpsertSession :execrows
INSERT INTO sessions (id, organization_id, author_person_id, sync_account_id, title, notes, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET title = EXCLUDED.title, notes = EXCLUDED.notes,
    payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now()
WHERE sessions.sync_account_id = EXCLUDED.sync_account_id;

-- Tombstones are per-table: a delete can only affect a row this account owns,
-- so REST-created rows (sync_account_id IS NULL) are never tombstoned.
--
-- A tombstone clears exactly what its upsert sets, and keeps only what a tombstone needs
-- to do its job: the id, the deleted flag and a fresh seq. It went on holding everything
-- -- names, medical notes, emergency contacts, the whole payload -- for as long as the
-- row existed, and nothing ever revisited them. A coach who deleted an athlete had not
-- deleted that athlete's medical notes. sync_documents already cleared its payload on
-- write; these seven did not (see docs/AUDIT-5.md L1).
--
-- Clearing precisely the upsert's own columns is what keeps this reversible: pushing the
-- record again restores every field that was cleared, because they are the same fields.
-- Columns the sync upsert never writes are left alone for the same reason -- REST owns
-- them, a sync delete has no business dropping them, and nothing would put them back.
-- The NOT NULL display columns take '' rather than NULL; no read reaches a tombstoned
-- row, so the value is unobservable either way, and '' keeps the constraint honest.
--
-- The trade, stated once: the server can no longer reconstruct a record from a delete it
-- has applied. There is no undelete endpoint and no read path that returns a tombstoned
-- row, so nothing loses a capability it had -- but a mistaken delete is now the client's
-- to recover from, not ours.

-- name: SyncTombstoneTeam :execrows
UPDATE teams SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    name = '', age_group = NULL, season = NULL, payload = NULL
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncTombstoneDrill :execrows
UPDATE drills SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    name = '', description = NULL, payload = NULL
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncTombstoneSession :execrows
UPDATE sessions SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    title = '', notes = NULL, payload = NULL
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncUpsertDocument :execrows
INSERT INTO sync_documents (sync_account_id, type, id, payload, deleted, seq)
VALUES ($1, $2, $3, $4, false, nextval('sync_seq'))
ON CONFLICT (sync_account_id, type, id) DO UPDATE
SET payload = EXCLUDED.payload, deleted = false, seq = nextval('sync_seq'), updated_at = now();

-- name: SyncTombstoneDocument :execrows
INSERT INTO sync_documents (sync_account_id, type, id, payload, deleted, seq)
VALUES ($1, $2, $3, NULL, true, nextval('sync_seq'))
ON CONFLICT (sync_account_id, type, id) DO UPDATE
SET payload = NULL, deleted = true, seq = nextval('sync_seq'), updated_at = now();

-- name: SyncUpsertPerson :execrows
-- The one statement that may adopt an unowned row, and only the caller's own Person.
-- /auth/apple provisions the coach's Person through CreatePersonWithID with a NULL
-- sync_account_id, and the app then pushes that same id expecting the two to reconcile
-- into one identity (see 0003_person_sync.sql). The second disjunct permits exactly that:
-- persons.id equals the pushing account's id only for the caller's own row, so it cannot
-- be used to claim anyone else's.
INSERT INTO persons (id, sync_account_id, display_name, emergency_contact_name, emergency_contact_phone, medical_notes, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET display_name = EXCLUDED.display_name,
    emergency_contact_name = EXCLUDED.emergency_contact_name,
    emergency_contact_phone = EXCLUDED.emergency_contact_phone,
    medical_notes = EXCLUDED.medical_notes,
    sync_account_id = EXCLUDED.sync_account_id, payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now()
WHERE persons.sync_account_id = EXCLUDED.sync_account_id
   OR (persons.sync_account_id IS NULL AND persons.id = EXCLUDED.sync_account_id);

-- name: SyncTombstonePerson :execrows
-- The one that motivated the rule. medical_notes and the emergency contact of a deleted
-- athlete are the most sensitive fields this service stores, and they were kept forever.
-- email, phone, birthdate and the given/family names are deliberately not touched:
-- SyncUpsertPerson does not write them, REST does, and clearing what cannot be restored
-- would turn a sync delete into permanent loss of data it does not own.
UPDATE persons SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    display_name = '', emergency_contact_name = NULL, emergency_contact_phone = NULL,
    medical_notes = NULL, payload = NULL
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncUpsertPlayer :execrows
INSERT INTO players (id, sync_account_id, person_id, name, number, position, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET person_id = EXCLUDED.person_id, name = EXCLUDED.name, number = EXCLUDED.number,
    position = EXCLUDED.position,
    payload = EXCLUDED.payload, deleted = false, seq = nextval('sync_seq'), updated_at = now()
WHERE players.sync_account_id = EXCLUDED.sync_account_id;

-- name: SyncTombstonePlayer :execrows
UPDATE players SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    name = NULL, number = NULL, position = NULL, payload = NULL
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncUpsertEvent :execrows
INSERT INTO events (id, sync_account_id, team_id, title, kind, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET team_id = EXCLUDED.team_id, title = EXCLUDED.title, kind = EXCLUDED.kind,
    payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now()
WHERE events.sync_account_id = EXCLUDED.sync_account_id;

-- name: SyncTombstoneEvent :execrows
UPDATE events SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    title = NULL, kind = NULL, payload = NULL
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncUpsertDiagram :execrows
INSERT INTO diagrams (id, sync_account_id, team_id, title, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET team_id = EXCLUDED.team_id, title = EXCLUDED.title,
    payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now()
WHERE diagrams.sync_account_id = EXCLUDED.sync_account_id;

-- name: SyncTombstoneDiagram :execrows
UPDATE diagrams SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    title = NULL, payload = NULL
WHERE id = $1 AND sync_account_id = $2;

-- name: CurrentSyncSeq :one
-- The highest cursor this server could have issued.
--
-- A pull uses it to bounds-check the cursor it was handed. Every cursor the server
-- returns is the seq of a row it actually delivered, so a legitimate cursor is never
-- above this; a cursor that is can only have come from a sequence that moved backwards
-- under a client that had already passed it -- which is what a database restore does,
-- and a restore is the documented way back from 0009. Without the check, such a cursor
-- is answered with an empty page and echoed back unchanged, which is exactly the
-- client's "you are up to date" condition. See docs/AUDIT-5.md M2.
--
-- pg_sequence_last_value reads the sequence's own page rather than consuming a value,
-- so this costs no cursor and no write.
--
-- It is NULL when the sequence has not been called since it was created or reset, and
-- that is deliberately left as NULL rather than folded to 0. A restore taken by pg_dump
-- or PITR brings last_value back with is_called set, so the real case this guards has a
-- number. NULL means something reset the sequence without writing through it, and then
-- 0 is not the highest cursor the server could have issued -- the rows still carry
-- higher seqs, and folding to 0 would make a pull reject the cursor it had itself just
-- returned, resyncing that device from the beginning on every single pull. The caller
-- reads `known = false` as "no bound available" and lets the cursor stand.
WITH s AS (SELECT pg_sequence_last_value('sync_seq'::regclass) AS v)
SELECT (v IS NOT NULL)::boolean AS known, coalesce(v, 0)::bigint AS seq FROM s;
