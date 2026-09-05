-- Reminders: chasing the squad that has not replied yet -----------------------

-- name: ClaimGamesDueForReminder :many
-- Take the fixtures that are close enough to chase, and mark them chased in the same
-- statement.
--
-- The claim is the point. A deployed service runs more than one instance and every
-- instance sweeps on the same schedule, so a select-then-update would hand the same
-- fixture to all of them and every player would get one push per replica. Here the row is
-- only returned if this statement is the one that set the column: under READ COMMITTED a
-- second instance re-evaluates `reminder_sent_at IS NULL` after the first commits, finds
-- it set, and updates nothing. No advisory lock, no leader election, no SKIP LOCKED --
-- the column that records the work is the same column that claims it.
--
-- Only 'scheduled' games. A cancelled fixture has nothing to reply to (POST /rsvp refuses
-- it), and a completed one is in the past by definition.
--
-- The upper bound comes in as an instant rather than an interval so the caller owns the
-- lead time, and `kickoff_at > now()` keeps a match that has already started out of it: a
-- sweep that fell behind should stay quiet rather than ask a squad whether they are
-- coming to something that kicked off an hour ago.
UPDATE games SET reminder_sent_at = now()
WHERE reminder_sent_at IS NULL
  AND id IN (
    SELECT g.id FROM games g
    JOIN teams t ON t.id = g.team_id AND t.deleted = false
    WHERE g.reminder_sent_at IS NULL
      AND g.status = 'scheduled'
      AND g.kickoff_at IS NOT NULL
      AND g.kickoff_at > now()
      AND g.kickoff_at <= @until
  )
RETURNING id, team_id, opponent, kickoff_at;

-- name: ClaimSessionsDueForReminder :many
-- The training half, claimed the same way. Two conditions differ: a session has no status
-- column, and its team is optional -- a plan a coach drafted for themselves has no roster
-- to chase, so a NULL team_id drops out here rather than producing a sweep that finds
-- nobody.
UPDATE sessions SET reminder_sent_at = now()
WHERE reminder_sent_at IS NULL
  AND id IN (
    SELECT s.id FROM sessions s
    JOIN teams t ON t.id = s.team_id AND t.deleted = false
    WHERE s.reminder_sent_at IS NULL
      AND s.deleted = false
      AND s.team_id IS NOT NULL
      AND s.scheduled_at IS NOT NULL
      AND s.scheduled_at > now()
      AND s.scheduled_at <= @until
  )
RETURNING id, team_id, title, scheduled_at;

-- name: ListUnansweredReachablePeopleForEvent :many
-- Who still owes an answer, and can be reached to be asked for it.
--
-- Narrower than ListReachablePeopleForTeam in the way that matters: someone who already
-- said they are coming has done what was asked, and chasing them is how a reminder
-- becomes noise people turn off. A line with a recorded attendance but no RSVP still
-- counts as unanswered -- the coach marking someone present is not that person replying.
--
-- Guardians come along for the child's silence, not their own. A nine-year-old has no
-- device and no login; the reply is going to come from a parent or not at all, so the
-- parents of every unanswered player are in the set even though the players themselves
-- may not be.
WITH unanswered AS (
    SELECT r.person_id FROM roster_memberships r
    WHERE r.team_id = @team_id AND r.left_on IS NULL
      AND NOT EXISTS (
        SELECT 1 FROM attendances a
        WHERE a.person_id = r.person_id
          AND (a.game_id = sqlc.narg('game_id') OR a.session_id = sqlc.narg('session_id'))
          AND a.rsvp IS NOT NULL
      )
), people AS (
    SELECT person_id FROM unanswered
    UNION
    SELECT g.guardian_person_id AS person_id FROM guardianships g
    WHERE g.child_person_id IN (SELECT person_id FROM unanswered)
)
SELECT p.id FROM people pe
JOIN persons p ON p.id = pe.person_id AND p.deleted = false
WHERE EXISTS (SELECT 1 FROM device_tokens d WHERE d.person_id = p.id);
