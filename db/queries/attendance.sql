-- Attendance (RSVP + who actually came) ------------------------------------

-- name: EnsureAttendance :exec
-- Open the row for (event, person) if it is not already open.
--
-- Paired with one of the two setters below rather than folded into them as an upsert:
-- the uniqueness that matters lives in two partial indexes (one per event kind, see
-- 0014_attendance.sql), and ON CONFLICT can only infer one target, so an upsert would
-- have to be written twice and each copy would silently cover only half the table. A
-- bare DO NOTHING covers whichever index the row actually hits, and makes this safe to
-- call on every write without asking first.
--
-- The two statements do not need a transaction between them. An insert whose setter
-- never ran leaves a row with no answer and no status, which is exactly the row that
-- would have been read as "has not replied" had the insert not happened at all.
INSERT INTO attendances (game_id, session_id, person_id)
VALUES (sqlc.narg('game_id'), sqlc.narg('session_id'), @person_id)
ON CONFLICT DO NOTHING;

-- name: SetAttendanceRSVP :one
-- Record what a player said, and who said it. rsvp_by_person_id is the caller, which is
-- not always the subject: a parent answers for a child, and a coach answers for a player
-- with no login.
--
-- The event is matched with an OR over the two nullable keys rather than a branch per
-- kind. Exactly one of the arguments is ever non-NULL, and `column = NULL` is NULL and so
-- never matches, which makes the unused half of the condition inert instead of wrong.
--
-- Every field is written, with no set-flags: the endpoint is a PUT of one whole answer,
-- so a reply sent without a note is a reply that has no note. That is the difference from
-- SetAttendanceStatus below, which is a PATCH and has to tell absent from null.
UPDATE attendances
SET rsvp              = @rsvp::text,
    rsvp_note         = sqlc.narg('rsvp_note'),
    rsvp_at           = now(),
    rsvp_by_person_id = @rsvp_by_person_id,
    updated_at        = now()
WHERE person_id = @person_id
  AND (game_id = sqlc.narg('game_id') OR session_id = sqlc.narg('session_id'))
RETURNING *;

-- name: SetAttendanceStatus :one
-- Record what happened, which is staff's to write.
--
-- Clearing the status is a real operation, unlike clearing an RSVP: a coach who ticked
-- the wrong player needs the row to go back to "not recorded", and there is no value in
-- the vocabulary that means that. When the status goes the provenance goes with it --
-- keeping a recorded_at for a status nobody holds would be a timestamp on nothing.
--
-- Which makes each field a set-flag plus a nullable value, the shape UpdateGame settles
-- on and for the same reason: this is a PATCH, so absent and null are different requests,
-- and a single COALESCE has no way to say "clear it". Without the flags, a coach adding a
-- note to a line would silently erase the status they had just recorded.
UPDATE attendances
SET status      = CASE WHEN @set_status::bool THEN sqlc.narg('status') ELSE status END,
    status_note = CASE WHEN @set_status_note::bool THEN sqlc.narg('status_note') ELSE status_note END,
    recorded_at = CASE WHEN @set_status::bool
                       THEN CASE WHEN sqlc.narg('status')::text IS NULL THEN NULL ELSE now() END
                       ELSE recorded_at END,
    recorded_by_person_id = CASE WHEN @set_status::bool
                       THEN CASE WHEN sqlc.narg('status')::text IS NULL
                                 THEN NULL ELSE sqlc.narg('recorded_by_person_id')::uuid END
                       ELSE recorded_by_person_id END,
    updated_at  = now()
WHERE person_id = @person_id
  AND (game_id = sqlc.narg('game_id') OR session_id = sqlc.narg('session_id'))
RETURNING *;

-- name: ListAttendanceForEvent :many
-- The sheet: one line per person, whether or not they have answered.
--
-- The people are the union of two sets, not just the current roster. A player who left
-- the club in March was still at February's match, and driving this off
-- roster_memberships alone would erase them from that game's record -- the attendance
-- row would still exist, unreachable, and the coach's own count would disagree with it.
-- `on_roster` is what tells the two apart on the far side.
--
-- The union is a CTE over two indexed lookups rather than a scan of persons filtered by
-- "on this team or at this event", which is the same set and reads every person in the
-- database to find it.
--
-- `all_people` narrows the result to named people without a second query: the RSVP and
-- attendance writes each return the one line they changed, and re-reading a squad of
-- twenty to answer with one of them is a query nobody asked for. Role scoping is
-- deliberately NOT done here -- see handleGetAttendance, which needs the whole sheet to
-- count it before narrowing what it shows.
WITH people AS (
    SELECT r.person_id FROM roster_memberships r
    WHERE r.team_id = @team_id AND r.left_on IS NULL
    UNION
    SELECT a.person_id FROM attendances a
    WHERE a.game_id = sqlc.narg('game_id') OR a.session_id = sqlc.narg('session_id')
)
SELECT p.id AS person_id, p.display_name,
       r.jersey_number, r.position, (r.id IS NOT NULL)::bool AS on_roster,
       a.rsvp, a.rsvp_note, a.rsvp_at, a.rsvp_by_person_id,
       a.status, a.status_note, a.recorded_at, a.recorded_by_person_id
FROM people pe
JOIN persons p ON p.id = pe.person_id AND p.deleted = false
LEFT JOIN roster_memberships r
       ON r.person_id = pe.person_id AND r.team_id = @team_id AND r.left_on IS NULL
LEFT JOIN attendances a
       ON a.person_id = pe.person_id
      AND (a.game_id = sqlc.narg('game_id') OR a.session_id = sqlc.narg('session_id'))
WHERE @all_people::bool OR pe.person_id = ANY(@person_ids::uuid[])
ORDER BY r.jersey_number NULLS LAST, p.display_name ASC;

-- name: CountAnsweredAttendanceForSession :one
-- Whether a session's register has been started, which is what decides whether it may
-- still be moved to another team.
--
-- Counted on answers rather than rows: EnsureAttendance opens a blank line before either
-- setter runs, so a row on its own means nothing happened. A line with an RSVP or a
-- recorded status is somebody's statement about a specific squad's training, and carrying
-- it over to a different squad would attribute it to people who were never asked.
SELECT count(*) FROM attendances
WHERE session_id = $1 AND (rsvp IS NOT NULL OR status IS NOT NULL);

-- name: AggregateAttendanceForTeam :many
-- The register read down the season instead of across one fixture.
--
-- A coach's real question is not "who is coming on Saturday" but "who keeps missing
-- training", and until now the only way to ask it was to open every sheet in turn and
-- count by hand. This is that count, one line per player.
--
-- The universe is the active squad times the team's events in the window, not the
-- attendance rows: a player who was never marked at all has to appear as somebody who
-- missed six sessions, and driving this off `attendances` would show them as having a
-- clean record because silence leaves no row. That is what `not_recorded` separates out —
-- a squad nobody registered and a squad that turned up are the same numbers otherwise.
--
-- A cancelled game drops out. Nobody attends a match that was called off, and counting it
-- as an absence would punish a squad for a coach's decision.
--
-- Rows with no date are in the window only when no window was asked for: `starts_at >=
-- NULL` is NULL and so fails the filter, which is the wanted answer — a fixture with no
-- time cannot be placed in March.
WITH events AS (
    SELECT g.id AS event_id, 'game'::text AS kind, g.kickoff_at AS starts_at
    FROM games g
    WHERE g.team_id = @team_id AND g.status <> 'cancelled'
    UNION ALL
    SELECT s.id AS event_id, 'session'::text AS kind, s.scheduled_at AS starts_at
    FROM sessions s
    WHERE s.team_id = @team_id AND s.deleted = false
), scoped AS (
    SELECT event_id, kind, starts_at FROM events
    WHERE (@include_games::bool OR kind <> 'game')
      AND (@include_sessions::bool OR kind <> 'session')
      AND (sqlc.narg('starting_from')::timestamptz IS NULL OR starts_at >= sqlc.narg('starting_from'))
      AND (sqlc.narg('starting_to')::timestamptz IS NULL OR starts_at <= sqlc.narg('starting_to'))
)
SELECT p.id AS person_id, p.display_name, r.jersey_number,
       count(*) AS events,
       count(*) FILTER (WHERE a.status = 'present') AS present,
       count(*) FILTER (WHERE a.status = 'absent')  AS absent,
       count(*) FILTER (WHERE a.status = 'late')    AS late,
       count(*) FILTER (WHERE a.status = 'excused') AS excused,
       count(*) FILTER (WHERE a.status IS NULL)     AS not_recorded,
       -- Said they were coming and did not turn up. The single most actionable number
       -- here after the rate, and one no per-fixture sheet can show: it only exists once
       -- the two halves of a line are read together across a season.
       count(*) FILTER (WHERE a.rsvp = 'going' AND a.status = 'absent') AS no_shows
FROM roster_memberships r
JOIN persons p ON p.id = r.person_id AND p.deleted = false
CROSS JOIN scoped e
LEFT JOIN attendances a
       ON a.person_id = r.person_id
      AND ((e.kind = 'game' AND a.game_id = e.event_id)
        OR (e.kind = 'session' AND a.session_id = e.event_id))
WHERE r.team_id = @team_id AND r.left_on IS NULL
GROUP BY p.id, p.display_name, r.jersey_number
ORDER BY r.jersey_number NULLS LAST, p.display_name ASC;

-- name: AggregateAttendanceForPerson :many
-- One athlete's record, across the teams they have played for, a line per team.
--
-- The team aggregate asks "how is this squad turning up" and can take the current roster
-- as its universe. This asks the mirrored question about a person, and the current roster
-- is the wrong answer to it: an athlete who moved from U12 to U14 in January did not stop
-- having attended U12's autumn, and a player who left the club still has the season they
-- played. So the universe is every event of every team they were rostered on *while it
-- happened* -- which is what roster_memberships being time-bounded is for, and the first
-- query in this service to actually read those bounds rather than just the open ones.
--
-- The overlap is inclusive at both ends: an event on the day someone joined, or on the day
-- they left, is one they were there for. Compared as dates rather than instants, which
-- means UTC -- the same limitation the attendance window carries, and it can only move an
-- event across a boundary on the exact day of a transfer.
--
-- An event with no date is counted for any membership on that team. It cannot be placed in
-- a window, and dropping it would hide a fixture the athlete was actually marked at; the
-- EXISTS is what keeps a rejoined player from counting it twice.
--
-- Scoped to one organization. A person may hold roster spots in two clubs, and the caller
-- was cleared to see them in exactly one -- returning the other club's teams here would be
-- a disclosure that personVisibleTo never authorized.
WITH memberships AS (
    SELECT r.team_id, r.joined_on, r.left_on
    FROM roster_memberships r
    WHERE r.person_id = @person_id
), events AS (
    SELECT g.id AS event_id, 'game'::text AS kind, g.team_id, g.kickoff_at AS starts_at
    FROM games g
    WHERE g.team_id IN (SELECT team_id FROM memberships) AND g.status <> 'cancelled'
    UNION ALL
    SELECT s.id AS event_id, 'session'::text AS kind, s.team_id, s.scheduled_at AS starts_at
    FROM sessions s
    WHERE s.team_id IN (SELECT team_id FROM memberships) AND s.deleted = false
), theirs AS (
    SELECT e.event_id, e.kind, e.team_id FROM events e
    WHERE (@include_games::bool OR e.kind <> 'game')
      AND (@include_sessions::bool OR e.kind <> 'session')
      AND (sqlc.narg('starting_from')::timestamptz IS NULL OR e.starts_at >= sqlc.narg('starting_from'))
      AND (sqlc.narg('starting_to')::timestamptz IS NULL OR e.starts_at <= sqlc.narg('starting_to'))
      AND (sqlc.narg('only_team_id')::uuid IS NULL OR e.team_id = sqlc.narg('only_team_id'))
      AND (
        EXISTS (
          SELECT 1 FROM memberships m
          WHERE m.team_id = e.team_id
            AND (e.starts_at IS NULL
              OR (e.starts_at::date >= m.joined_on
                  AND (m.left_on IS NULL OR e.starts_at::date <= m.left_on)))
        )
        -- Or they were actually marked at it. The roster window says which events an
        -- athlete was *expected* at; a line with an answer or a status on it is evidence
        -- they were involved, and evidence outranks the window. Without this, a coach who
        -- rosters a player today and then back-fills last month's register writes a fact
        -- that never appears in that player's own record -- the roster row starts today,
        -- so every event behind it falls outside the window and silently vanishes.
        --
        -- A blank line does not count: EnsureAttendance opens one before either setter
        -- runs, so a row on its own is not evidence of anything.
        OR EXISTS (
          SELECT 1 FROM attendances a2
          WHERE a2.person_id = @person_id
            AND ((e.kind = 'game' AND a2.game_id = e.event_id)
              OR (e.kind = 'session' AND a2.session_id = e.event_id))
            AND (a2.rsvp IS NOT NULL OR a2.status IS NOT NULL)
        )
      )
)
SELECT t.id AS team_id, t.name AS team_name,
       count(*) AS events,
       count(*) FILTER (WHERE a.status = 'present') AS present,
       count(*) FILTER (WHERE a.status = 'absent')  AS absent,
       count(*) FILTER (WHERE a.status = 'late')    AS late,
       count(*) FILTER (WHERE a.status = 'excused') AS excused,
       count(*) FILTER (WHERE a.status IS NULL)     AS not_recorded,
       count(*) FILTER (WHERE a.rsvp = 'going' AND a.status = 'absent') AS no_shows
FROM theirs ev
JOIN teams t ON t.id = ev.team_id
            AND t.deleted = false
            AND t.organization_id = @organization_id
LEFT JOIN attendances a
       ON a.person_id = @person_id
      AND ((ev.kind = 'game' AND a.game_id = ev.event_id)
        OR (ev.kind = 'session' AND a.session_id = ev.event_id))
GROUP BY t.id, t.name
ORDER BY t.name ASC;
