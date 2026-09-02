-- Clear the content still held by rows that were tombstoned before the tombstone
-- statements started clearing it.
--
-- The seven projected tables kept everything a row had -- names, medical notes,
-- emergency contacts, the full payload -- for as long as the tombstone existed, and
-- nothing ever revisited them. sync_documents cleared its payload on write; these did
-- not. The queries now clear on delete (see docs/AUDIT-5.md L1), but that only helps
-- deletes made from here on: every row already tombstoned still holds its athlete's
-- medical notes. This is the backfill, and it is the half that actually removes data
-- somebody has already asked to be rid of.
--
-- It clears exactly what the matching tombstone statement clears, so a database that
-- runs this is in the state it would have been in had the statements always been right.
-- Columns the sync upsert never writes (persons.email, phone, birthdate, given_name,
-- family_name) are left alone here for the same reason they are left alone there: REST
-- owns them, and nothing would put them back.
--
-- *** seq is deliberately not touched, and this is the one thing to not get clever
-- about. *** Every one of these rows has already been delivered to every device as a
-- tombstone. Bumping seq would re-deliver all of them: a fleet-wide burst of redundant
-- deletes, which for an account with a long history is the largest pull it has ever
-- made, for no change any client can observe. updated_at is left alone for the same
-- reason -- it is not a cursor, but there is no honest claim that these rows were
-- updated now; what changed is what this service is willing to keep.
--
-- Idempotent: the WHERE clauses match only rows that still hold something, so re-running
-- it does nothing and a database that never held such a row is untouched.

UPDATE teams SET name = '', age_group = NULL, season = NULL, payload = NULL
WHERE deleted = true
  AND (name <> '' OR age_group IS NOT NULL OR season IS NOT NULL OR payload IS NOT NULL);

UPDATE drills SET name = '', description = NULL, payload = NULL
WHERE deleted = true
  AND (name <> '' OR description IS NOT NULL OR payload IS NOT NULL);

UPDATE sessions SET title = '', notes = NULL, payload = NULL
WHERE deleted = true
  AND (title <> '' OR notes IS NOT NULL OR payload IS NOT NULL);

UPDATE persons SET display_name = '', emergency_contact_name = NULL,
    emergency_contact_phone = NULL, medical_notes = NULL, payload = NULL
WHERE deleted = true
  AND (display_name <> '' OR emergency_contact_name IS NOT NULL
    OR emergency_contact_phone IS NOT NULL OR medical_notes IS NOT NULL
    OR payload IS NOT NULL);

UPDATE players SET name = NULL, number = NULL, position = NULL, payload = NULL
WHERE deleted = true
  AND (name IS NOT NULL OR number IS NOT NULL OR position IS NOT NULL
    OR payload IS NOT NULL);

UPDATE events SET title = NULL, kind = NULL, payload = NULL
WHERE deleted = true
  AND (title IS NOT NULL OR kind IS NOT NULL OR payload IS NOT NULL);

UPDATE diagrams SET title = NULL, payload = NULL
WHERE deleted = true
  AND (title IS NOT NULL OR payload IS NOT NULL);
