-- name: UpsertMatchStat :one
INSERT INTO player_match_stats (user_id, match_id, goals, assists, yellow_cards, red_cards, minutes_played)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, match_id)
DO UPDATE SET goals = EXCLUDED.goals, assists = EXCLUDED.assists,
    yellow_cards = EXCLUDED.yellow_cards, red_cards = EXCLUDED.red_cards,
    minutes_played = EXCLUDED.minutes_played, updated_at = now()
RETURNING *;

-- name: UpsertFixtureStat :one
INSERT INTO player_match_stats (user_id, fixture_id, goals, assists, yellow_cards, red_cards, minutes_played)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, fixture_id)
DO UPDATE SET goals = EXCLUDED.goals, assists = EXCLUDED.assists,
    yellow_cards = EXCLUDED.yellow_cards, red_cards = EXCLUDED.red_cards,
    minutes_played = EXCLUDED.minutes_played, updated_at = now()
RETURNING *;

-- name: ListMatchStats :many
SELECT s.*,
    u.email, u.display_name, u.position, u.skill_level, u.bio, u.avatar_url, u.created_at AS user_created_at
FROM player_match_stats s
JOIN users u ON u.id = s.user_id
WHERE s.match_id = $1
ORDER BY s.goals DESC, s.assists DESC;

-- name: ListFixtureStats :many
SELECT s.*,
    u.email, u.display_name, u.position, u.skill_level, u.bio, u.avatar_url, u.created_at AS user_created_at
FROM player_match_stats s
JOIN users u ON u.id = s.user_id
WHERE s.fixture_id = $1
ORDER BY s.goals DESC, s.assists DESC;
