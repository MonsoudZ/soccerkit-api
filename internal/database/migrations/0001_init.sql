-- SoccerKit schema: identity, pickup matches + RSVPs, teams, leagues/fixtures,
-- and per-match player statistics.

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    display_name  text NOT NULL,
    position      text CHECK (position IN ('GK', 'DEF', 'MID', 'FWD')),
    skill_level   int  NOT NULL DEFAULT 3 CHECK (skill_level BETWEEN 1 AND 5),
    bio           text,
    avatar_url    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_users_skill_level ON users (skill_level);

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token      text NOT NULL UNIQUE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_refresh_tokens_user_id ON refresh_tokens (user_id);

CREATE TABLE venues (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    address    text,
    city       text,
    latitude   double precision,
    longitude  double precision,
    surface    text CHECK (surface IN ('GRASS', 'TURF', 'INDOOR', 'CONCRETE')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE matches (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id      uuid NOT NULL REFERENCES users (id),
    venue_id     uuid REFERENCES venues (id) ON DELETE SET NULL,
    title        text NOT NULL,
    description  text,
    format       text NOT NULL,
    max_players  int  NOT NULL,
    kickoff_at   timestamptz NOT NULL,
    duration_min int  NOT NULL DEFAULT 60,
    status       text NOT NULL DEFAULT 'SCHEDULED'
                 CHECK (status IN ('SCHEDULED', 'IN_PROGRESS', 'COMPLETED', 'CANCELLED')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_matches_kickoff_at ON matches (kickoff_at);
CREATE INDEX idx_matches_status ON matches (status);

CREATE TABLE match_rsvps (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    match_id   uuid NOT NULL REFERENCES matches (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status     text NOT NULL DEFAULT 'GOING'
               CHECK (status IN ('GOING', 'MAYBE', 'WAITLIST', 'DECLINED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (match_id, user_id)
);
CREATE INDEX idx_match_rsvps_user_id ON match_rsvps (user_id);

CREATE TABLE teams (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    crest_url  text,
    owner_id   uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE team_members (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id       uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role          text NOT NULL DEFAULT 'PLAYER'
                  CHECK (role IN ('OWNER', 'CAPTAIN', 'PLAYER')),
    jersey_number int,
    joined_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (team_id, user_id)
);
CREATE INDEX idx_team_members_user_id ON team_members (user_id);

CREATE TABLE leagues (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    season     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE league_teams (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    league_id uuid NOT NULL REFERENCES leagues (id) ON DELETE CASCADE,
    team_id   uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    UNIQUE (league_id, team_id)
);

CREATE TABLE fixtures (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    league_id    uuid NOT NULL REFERENCES leagues (id) ON DELETE CASCADE,
    home_team_id uuid NOT NULL REFERENCES teams (id),
    away_team_id uuid NOT NULL REFERENCES teams (id),
    kickoff_at   timestamptz NOT NULL,
    home_score   int,
    away_score   int,
    status       text NOT NULL DEFAULT 'SCHEDULED'
                 CHECK (status IN ('SCHEDULED', 'IN_PROGRESS', 'COMPLETED', 'CANCELLED')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_fixtures_league_id ON fixtures (league_id);
CREATE INDEX idx_fixtures_kickoff_at ON fixtures (kickoff_at);

CREATE TABLE player_match_stats (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    match_id       uuid REFERENCES matches (id) ON DELETE CASCADE,
    fixture_id     uuid REFERENCES fixtures (id) ON DELETE CASCADE,
    goals          int NOT NULL DEFAULT 0,
    assists        int NOT NULL DEFAULT 0,
    yellow_cards   int NOT NULL DEFAULT 0,
    red_cards      int NOT NULL DEFAULT 0,
    minutes_played int NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, match_id),
    UNIQUE (user_id, fixture_id)
);
CREATE INDEX idx_player_match_stats_user_id ON player_match_stats (user_id);
