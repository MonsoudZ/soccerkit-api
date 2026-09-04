-- Who is coming, and who actually came.
--
-- Two questions, one row. They are asked by different people at different times -- a
-- player (or their parent) answers the first before the event, a coach records the second
-- after it -- but they are the same fact about the same person at the same fixture, and
-- splitting them into two tables would mean every sheet a coach reads is a join written
-- by hand. Both halves are nullable: an unanswered RSVP and an unrecorded attendance are
-- the normal state of a row, not a missing one.
--
-- The event is a game or a training session, as two nullable foreign keys with a CHECK
-- that exactly one is set. That is the shape form_instances already uses for its subject,
-- and it is chosen here for the reason it was chosen there: a (type, id) pair would be
-- one column instead of two, and would give up the foreign key. This table holds a
-- statement about a named child, so a row that outlives the fixture it belongs to is a
-- record nobody can reach and nobody deletes -- including on DELETE /me, where erasure
-- runs down exactly these cascades.
--
-- No sync spine (payload/seq/sync_account_id). The app has no RSVP record to push yet, and
-- the projected tables carry one because a client type already existed for them; adding
-- columns here for a contract nobody has written would be inventing the client's schema
-- from the server side. It reaches the phone over REST until there is something to project.
CREATE TABLE attendances (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    game_id    uuid REFERENCES games (id) ON DELETE CASCADE,
    session_id uuid REFERENCES sessions (id) ON DELETE CASCADE,
    person_id  uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,

    -- The answer, and who gave it. rsvp_by_person_id is not always person_id: a parent
    -- answers for a child, and a coach who took the call answers for a player who has no
    -- login at all. Knowing which of those happened is the difference between "the family
    -- told us" and "we assumed", so it is recorded rather than inferred.
    rsvp              text CHECK (rsvp IN ('going', 'maybe', 'not_going')),
    rsvp_note         text,
    rsvp_at           timestamptz,
    rsvp_by_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,

    -- What happened, which only staff may write. 'excused' is distinct from 'absent'
    -- because a coach who cannot tell an approved absence from an unexplained one has an
    -- attendance record they cannot act on.
    status                text CHECK (status IN ('present', 'absent', 'late', 'excused')),
    status_note           text,
    recorded_at           timestamptz,
    recorded_by_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CHECK (num_nonnulls(game_id, session_id) = 1)
);

-- One row per person per event. Partial, because a plain UNIQUE over the three columns
-- would let a person hold any number of rows against the same game: (game, NULL, person)
-- is distinct from itself under NULL semantics, so the constraint that matters would not
-- exist. Each index is also what the sheet reads by.
CREATE UNIQUE INDEX idx_attendance_one_per_game
    ON attendances (game_id, person_id) WHERE game_id IS NOT NULL;
CREATE UNIQUE INDEX idx_attendance_one_per_session
    ON attendances (session_id, person_id) WHERE session_id IS NOT NULL;
CREATE INDEX idx_attendance_person ON attendances (person_id);
