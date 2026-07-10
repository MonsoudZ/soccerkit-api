-- name: ListSyncChangesSince :many
-- The delta an account hasn't seen: synced rows across every source, ordered by
-- the shared cursor. Projected tables contribute their type; sync_documents
-- carries its own.
SELECT delta.type, delta.id, delta.payload, delta.deleted, delta.seq FROM (
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
    SELECT sd.type, sd.id, sd.payload, sd.deleted, sd.seq
        FROM sync_documents sd WHERE sd.sync_account_id = $1 AND sd.seq > $2
) delta
ORDER BY delta.seq ASC;

-- name: SyncUpsertTeam :exec
INSERT INTO teams (id, organization_id, sync_account_id, name, age_group, season, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name, age_group = EXCLUDED.age_group, season = EXCLUDED.season,
    sync_account_id = EXCLUDED.sync_account_id, payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now();

-- name: SyncUpsertDrill :exec
INSERT INTO drills (id, organization_id, author_person_id, sync_account_id, name, description, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name, description = EXCLUDED.description,
    sync_account_id = EXCLUDED.sync_account_id, payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now();

-- name: SyncUpsertSession :exec
INSERT INTO sessions (id, organization_id, author_person_id, sync_account_id, title, notes, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET title = EXCLUDED.title, notes = EXCLUDED.notes,
    sync_account_id = EXCLUDED.sync_account_id, payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now();

-- Tombstones are per-table: a delete can only affect a row this account owns,
-- so REST-created rows (sync_account_id IS NULL) are never tombstoned.

-- name: SyncTombstoneTeam :exec
UPDATE teams SET deleted = true, seq = nextval('sync_seq'), updated_at = now()
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncTombstoneDrill :exec
UPDATE drills SET deleted = true, seq = nextval('sync_seq'), updated_at = now()
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncTombstoneSession :exec
UPDATE sessions SET deleted = true, seq = nextval('sync_seq'), updated_at = now()
WHERE id = $1 AND sync_account_id = $2;

-- name: SyncUpsertDocument :exec
INSERT INTO sync_documents (sync_account_id, type, id, payload, deleted, seq)
VALUES ($1, $2, $3, $4, false, nextval('sync_seq'))
ON CONFLICT (sync_account_id, type, id) DO UPDATE
SET payload = EXCLUDED.payload, deleted = false, seq = nextval('sync_seq'), updated_at = now();

-- name: SyncTombstoneDocument :exec
INSERT INTO sync_documents (sync_account_id, type, id, payload, deleted, seq)
VALUES ($1, $2, $3, NULL, true, nextval('sync_seq'))
ON CONFLICT (sync_account_id, type, id) DO UPDATE
SET payload = NULL, deleted = true, seq = nextval('sync_seq'), updated_at = now();

-- name: SyncUpsertPerson :exec
INSERT INTO persons (id, sync_account_id, display_name, emergency_contact_name, emergency_contact_phone, medical_notes, payload, deleted, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, nextval('sync_seq'))
ON CONFLICT (id) DO UPDATE
SET display_name = EXCLUDED.display_name,
    emergency_contact_name = EXCLUDED.emergency_contact_name,
    emergency_contact_phone = EXCLUDED.emergency_contact_phone,
    medical_notes = EXCLUDED.medical_notes,
    sync_account_id = EXCLUDED.sync_account_id, payload = EXCLUDED.payload,
    deleted = false, seq = nextval('sync_seq'), updated_at = now();

-- name: SyncTombstonePerson :exec
UPDATE persons SET deleted = true, seq = nextval('sync_seq'), updated_at = now()
WHERE id = $1 AND sync_account_id = $2;
