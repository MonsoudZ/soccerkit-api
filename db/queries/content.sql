-- Drills --------------------------------------------------------------------

-- name: CreateDrill :one
-- A drill created here is a drill the app can see, for the reasons CreateTeam sets out:
-- sync_account_id puts the row in the caller's stream, seq gives a cursor something to
-- deliver, and payload is what a pull actually returns.
--
-- The payload carries only id, title and fieldSetup, because that is all POST /drills
-- collects. Until now the app required a category, a duration and coaching points as
-- well, so a drill made this way could not be decoded at all and was dropped whole --
-- Codable loses the record over one missing key. Those fields are optional on the app
-- side now, so the drill arrives with blanks the coach can fill in rather than not
-- arriving. Inventing a category here was the alternative, and putting a coaching
-- decision nobody made into a coach's library is worse than leaving it empty.
INSERT INTO drills (id, organization_id, author_person_id, sync_account_id, name, description, payload, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, nextval('sync_seq'))
RETURNING *;

-- name: GetDrill :one
SELECT * FROM drills WHERE id = $1 AND deleted = false;

-- name: CountDrillsInOrgByIDs :one
-- How many of these ids are live drills in this organization. handleCreateSession asks
-- once for every drill its blocks reference and compares the answer with the number of
-- distinct ids it asked about; fewer means at least one block points at a drill that is
-- deleted, missing, or another org's. Counting rather than returning the rows is enough
-- because the request is rejected as a whole, and it keeps the check to one round trip
-- however many blocks the session has.
SELECT count(*) FROM drills
WHERE id = ANY(@ids::uuid[]) AND organization_id = @organization_id AND deleted = false;

-- name: ListDrillsInOrg :many
SELECT * FROM drills WHERE organization_id = $1 AND deleted = false ORDER BY name ASC;

-- Sessions ------------------------------------------------------------------

-- name: CreateSession :one
-- Same three columns as CreateDrill, and one field that needs saying out loud: the app
-- requires a date and the server always has one, so the payload carries scheduled_at
-- when it was given and the creation time when it was not. It is written as a number of
-- seconds since 2001-01-01, which is how Swift encodes a Date -- see
-- TestContractSwiftDatesSurviveAsNumbers, which pins that against this side drifting.
--
-- teamID and each block's drillID may be absent. Both were required by the app until
-- now, and both are things this API has always let a caller leave out, so a session
-- without a team, or with a warm-up block that runs no drill, simply never arrived.
INSERT INTO sessions (id, organization_id, author_person_id, sync_account_id, team_id, title, scheduled_at, notes, payload, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nextval('sync_seq'))
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1 AND deleted = false;

-- name: ListSessionsInOrg :many
SELECT * FROM sessions
WHERE organization_id = $1
  AND deleted = false
  AND (sqlc.narg('team_id')::uuid IS NULL OR team_id = sqlc.narg('team_id'))
ORDER BY scheduled_at DESC NULLS LAST, created_at DESC;

-- name: UpdateSession :one
-- Sessions could be created and deleted over REST but never edited, so a coach who moved
-- Tuesday training to Thursday had to delete the session and build it again -- taking its
-- register with it, now that a session has one. It is the same gap UpdateTeam closed for
-- teams, arriving late because nothing scheduled by a session used to be attended.
--
-- Written twice, into the columns this API reads and into the payload a pull returns, for
-- the reason UpdateTeam sets out at length: a pull returns `payload`, so a column-only
-- edit is invisible on the phone and the app's next push writes the old values straight
-- back over it, with nothing logged and nobody told.
--
-- Blocks are deliberately untouched. They are an ordered collection whose ids the payload
-- carries, so replacing them is a different operation from editing the session's own
-- fields -- folding the two together would mean every rename rewrote the plan, and a
-- caller who sent no blocks would erase one.
UPDATE sessions
SET title        = COALESCE(sqlc.narg('title'), title),
    scheduled_at = CASE WHEN sqlc.arg('set_scheduled_at')::bool THEN sqlc.narg('scheduled_at') ELSE scheduled_at END,
    notes        = CASE WHEN sqlc.arg('set_notes')::bool THEN sqlc.narg('notes') ELSE notes END,
    team_id      = CASE WHEN sqlc.arg('set_team_id')::bool THEN sqlc.narg('team_id') ELSE team_id END,
    payload      = CASE WHEN sqlc.arg('patch_payload')::bool
                        THEN COALESCE(payload, '{}'::jsonb) || sqlc.arg('payload_patch')::jsonb
                        ELSE payload END,
    seq          = nextval('sync_seq'),
    updated_at   = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteSession :exec
-- Tombstoned, not dropped — see DeleteTeam.
UPDATE sessions SET deleted = true, seq = nextval('sync_seq'), updated_at = now(),
    title = '', notes = NULL, payload = NULL
WHERE id = $1;

-- name: CreateSessionBlock :one
-- The id is supplied rather than defaulted, because the session's payload has to carry
-- each block's id and is written in the same statement as the session itself.
INSERT INTO session_blocks (id, session_id, drill_id, title, duration_min, position, notes)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListSessionBlocks :many
SELECT sb.*, d.name AS drill_name
FROM session_blocks sb
LEFT JOIN drills d ON d.id = sb.drill_id
WHERE sb.session_id = $1
ORDER BY sb.position, sb.title;

-- Games (game day) ----------------------------------------------------------

-- name: CreateGame :one
INSERT INTO games (organization_id, team_id, opponent, kickoff_at, home_away)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetGame :one
SELECT * FROM games WHERE id = $1;

-- name: ListGamesForTeam :many
SELECT * FROM games WHERE team_id = $1 ORDER BY kickoff_at DESC NULLS LAST, created_at DESC;

-- name: UpdateGame :one
-- Every clearable field is a set-flag plus a nullable value, kickoff_at included. It
-- used to be COALESCE(narg, kickoff_at), which reads NULL as "leave this alone" and so
-- leaves no value that means "clear it": a cancelled fixture's kickoff time could not be
-- unset. See docs/AUDIT-2.md L3.
UPDATE games
SET opponent       = CASE WHEN sqlc.arg('set_opponent')::bool THEN sqlc.narg('opponent') ELSE opponent END,
    kickoff_at     = CASE WHEN sqlc.arg('set_kickoff_at')::bool THEN sqlc.narg('kickoff_at') ELSE kickoff_at END,
    home_away      = CASE WHEN sqlc.arg('set_home_away')::bool THEN sqlc.narg('home_away') ELSE home_away END,
    our_score      = CASE WHEN sqlc.arg('set_scores')::bool THEN sqlc.narg('our_score') ELSE our_score END,
    opponent_score = CASE WHEN sqlc.arg('set_scores')::bool THEN sqlc.narg('opponent_score') ELSE opponent_score END,
    status         = COALESCE(sqlc.narg('status'), status),
    updated_at     = now()
WHERE id = sqlc.arg('id')
RETURNING *;
