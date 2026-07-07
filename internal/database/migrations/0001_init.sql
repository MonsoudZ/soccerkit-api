-- SoccerCoachKit — the whole-castle schema.
-- Models club -> director -> coach -> parent -> player from day one. Phase 1
-- ships the solo-coach surface; every later tier is data that already has a home.
--
-- The five load-bearing seams (see architecture doc §5):
--   1. organizations + memberships + roles   (tiering is a join, never a column)
--   2. persons != user_accounts               (players are People without logins)
--   3. roster_memberships are time-bounded    (no team_id on a person)
--   4. the evaluation engine is generic       (templates/fields/instances/answers)
--   5. share_grants are polymorphic + scoped

-- ---------------------------------------------------------------------------
-- Identity & organization
-- ---------------------------------------------------------------------------

CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    kind       text NOT NULL DEFAULT 'personal' CHECK (kind IN ('personal', 'club')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A human. NOT an account. Contact/medical/identity live here because they are
-- true of the person regardless of which team(s) or org(s) they belong to.
CREATE TABLE persons (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name            text NOT NULL,
    given_name              text,
    family_name             text,
    birthdate               date,
    email                   text,
    phone                   text,
    emergency_contact_name  text,
    emergency_contact_phone text,
    medical_notes           text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

-- An authenticatable identity. Optional per person (young players have none).
CREATE TABLE user_accounts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id     uuid NOT NULL UNIQUE REFERENCES persons (id) ON DELETE CASCADE,
    email         text NOT NULL UNIQUE,
    password_hash text,
    apple_sub     text UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Rotating, revocable refresh tokens (opaque, DB-backed).
CREATE TABLE refresh_tokens (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token           text NOT NULL UNIQUE,
    user_account_id uuid NOT NULL REFERENCES user_accounts (id) ON DELETE CASCADE,
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_refresh_tokens_account ON refresh_tokens (user_account_id);

-- (person, org, role). A person may hold several: parent-who-also-coaches,
-- coach-who-is-also-director. Role is NEVER a column on a person.
CREATE TABLE memberships (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id       uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    role            text NOT NULL CHECK (role IN ('admin', 'director', 'coach', 'parent', 'player')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (person_id, organization_id, role)
);
CREATE INDEX idx_memberships_org ON memberships (organization_id);
CREATE INDEX idx_memberships_person ON memberships (person_id);

-- Links a parent to their child(ren) and only their child(ren).
CREATE TABLE guardianships (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    guardian_person_id uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    child_person_id    uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (guardian_person_id, child_person_id)
);

-- ---------------------------------------------------------------------------
-- Teams & movement
-- ---------------------------------------------------------------------------

CREATE TABLE teams (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name            text NOT NULL,
    age_group       text,
    season          text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_teams_org ON teams (organization_id);

-- The time-bounded join that replaces person.team_id. A person never "belongs
-- to" a team; they hold a membership with a start and (maybe) an end date.
-- Concurrent active memberships = playing up an age group.
CREATE TABLE roster_memberships (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id     uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    team_id       uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    jersey_number int,
    position      text,
    joined_on     date NOT NULL DEFAULT CURRENT_DATE,
    left_on       date,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_roster_team ON roster_memberships (team_id);
CREATE INDEX idx_roster_person ON roster_memberships (person_id);
-- At most one open (active) membership per person per team.
CREATE UNIQUE INDEX idx_roster_one_active
    ON roster_memberships (person_id, team_id) WHERE left_on IS NULL;

-- ---------------------------------------------------------------------------
-- The evaluation engine (the moat) — one generic primitive:
-- "a dated, scored, noted response about a subject, in a context."
-- ---------------------------------------------------------------------------

-- organization_id NULL = a personal template the coach carries between clubs.
CREATE TABLE form_templates (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid REFERENCES organizations (id) ON DELETE CASCADE,
    author_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,
    context          text NOT NULL CHECK (context IN
                     ('tryout', 'pre_game', 'post_game', 'development', 'movement', 'coach_review')),
    name             text NOT NULL,
    subject_type     text NOT NULL DEFAULT 'athlete' CHECK (subject_type IN ('athlete', 'coach', 'team')),
    version          int  NOT NULL DEFAULT 1,
    is_seed          boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_form_templates_org ON form_templates (organization_id);

CREATE TABLE form_fields (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id uuid NOT NULL REFERENCES form_templates (id) ON DELETE CASCADE,
    key         text NOT NULL,
    label       text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('scale', 'bool', 'number', 'text', 'select')),
    position    int  NOT NULL DEFAULT 0,
    config      jsonb,
    UNIQUE (template_id, key)
);

CREATE TABLE form_instances (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id            uuid NOT NULL REFERENCES form_templates (id),
    subject_person_id      uuid REFERENCES persons (id) ON DELETE CASCADE,
    subject_team_id        uuid REFERENCES teams (id) ON DELETE CASCADE,
    context_ref_type       text,
    context_ref_id         uuid,
    submitted_by_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,
    submitted_at           timestamptz NOT NULL DEFAULT now(),
    extra                  jsonb,
    created_at             timestamptz NOT NULL DEFAULT now(),
    CHECK (subject_person_id IS NOT NULL OR subject_team_id IS NOT NULL)
);
CREATE INDEX idx_form_instances_template ON form_instances (template_id);
CREATE INDEX idx_form_instances_subject_person ON form_instances (subject_person_id);

-- Normalized, not jsonb: readiness means, effort trends and tryout rankings are
-- cross-instance aggregations that want columns you can AVG / GROUP BY.
CREATE TABLE form_answers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_id   uuid NOT NULL REFERENCES form_instances (id) ON DELETE CASCADE,
    field_id      uuid NOT NULL REFERENCES form_fields (id) ON DELETE CASCADE,
    numeric_value double precision,
    bool_value    boolean,
    text_value    text,
    UNIQUE (instance_id, field_id)
);
CREATE INDEX idx_form_answers_field ON form_answers (field_id);

-- ---------------------------------------------------------------------------
-- Content & game day (schema now; handlers land in Phase 1b)
-- ---------------------------------------------------------------------------

CREATE TABLE drills (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    author_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,
    name             text NOT NULL,
    description      text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    author_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,
    team_id          uuid REFERENCES teams (id) ON DELETE SET NULL,
    title            text NOT NULL,
    scheduled_at     timestamptz,
    notes            text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE session_blocks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id   uuid NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    drill_id     uuid REFERENCES drills (id) ON DELETE SET NULL,
    title        text NOT NULL,
    duration_min int,
    position     int NOT NULL DEFAULT 0,
    notes        text
);

CREATE TABLE games (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    team_id         uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    opponent        text,
    kickoff_at      timestamptz,
    home_away       text CHECK (home_away IN ('home', 'away', 'neutral')),
    our_score       int,
    opponent_score  int,
    status          text NOT NULL DEFAULT 'scheduled'
                    CHECK (status IN ('scheduled', 'in_progress', 'completed', 'cancelled')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_games_team ON games (team_id);

-- ---------------------------------------------------------------------------
-- Sharing (seam 5) — the entire coach-to-coach / club-library feature as one
-- polymorphic, scoped table.
-- ---------------------------------------------------------------------------

CREATE TABLE share_grants (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    shareable_type       text NOT NULL CHECK (shareable_type IN ('session', 'drill', 'form_template')),
    shareable_id         uuid NOT NULL,
    scope                text NOT NULL DEFAULT 'private' CHECK (scope IN ('private', 'team', 'org', 'link')),
    organization_id      uuid REFERENCES organizations (id) ON DELETE CASCADE,
    team_id              uuid REFERENCES teams (id) ON DELETE CASCADE,
    granted_by_person_id uuid REFERENCES persons (id) ON DELETE SET NULL,
    expires_at           timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_share_grants_shareable ON share_grants (shareable_type, shareable_id);
CREATE INDEX idx_share_grants_org ON share_grants (organization_id);
