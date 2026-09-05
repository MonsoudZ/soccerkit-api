-- When a squad was last nudged about a fixture they have not answered.
--
-- The push a fixture sends when it is created is one shot. A squad that swipes it away on
-- Monday leaves the coach reading a sheet of silence on Friday night, which is the state
-- the register exists to get them out of -- so something has to ask again as the date
-- approaches, and something has to remember that it did.
--
-- One timestamp per event rather than per person. A reminder sweep asks everyone who has
-- not replied at that moment, so "has this fixture been chased" is a fact about the
-- fixture; a row per recipient would record the same answer once per player and give the
-- sweep a second table to reconcile.
--
-- It is also the lock. A deployed service runs more than one instance, and every instance
-- runs the same sweep on the same schedule -- so without a claim each squad gets one push
-- per replica. The sweep sets this column in the same statement that selects the rows
-- (see ClaimGamesDueForReminder), so exactly one instance wins each fixture and the rest
-- find nothing to do.
ALTER TABLE games ADD COLUMN reminder_sent_at timestamptz;
ALTER TABLE sessions ADD COLUMN reminder_sent_at timestamptz;

-- The sweep reads "unchased, scheduled, starting soon", so the partial index carries only
-- the rows it can still return. Fixtures already chased -- which is nearly all of them,
-- forever -- stay out of it.
CREATE INDEX idx_games_due_for_reminder ON games (kickoff_at)
    WHERE reminder_sent_at IS NULL AND kickoff_at IS NOT NULL;
CREATE INDEX idx_sessions_due_for_reminder ON sessions (scheduled_at)
    WHERE reminder_sent_at IS NULL AND scheduled_at IS NOT NULL;
