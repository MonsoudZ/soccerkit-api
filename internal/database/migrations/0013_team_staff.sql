-- Which staff run which team.
--
-- The permission matrix distinguishes an admin or director, who see every team in the
-- organization, from a coach, who sees their own. The schema could not express that
-- difference: a person's only link to a team was roster_memberships, which is the athlete
-- roster, so "the teams this coach coaches" had no answer and GET /teams returned all of
-- them to everyone holding any membership.
--
-- No role column. Org membership already says someone is a coach; this says which teams,
-- and a second vocabulary here would immediately raise the question of what an assistant
-- may do that a coach may not -- a question nothing else in the schema asks yet.
CREATE TABLE team_staff (
    team_id    uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    person_id  uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, person_id)
);
CREATE INDEX idx_team_staff_person ON team_staff (person_id);

-- Backfill, and it is the load-bearing half of this migration. Scoping GET /teams to
-- team_staff on an empty table would answer "no teams" to every coach in existence,
-- which is the app's main screen going blank on deploy.
--
-- Whoever the team already belongs to: the account that pushed it through sync, or
-- failing that the organization's owner. For every team that exists today those are the
-- same solo coach, since a club has never had a second member.
INSERT INTO team_staff (team_id, person_id)
SELECT t.id, COALESCE(t.sync_account_id, o.owner_person_id)
FROM teams t
JOIN organizations o ON o.id = t.organization_id
WHERE COALESCE(t.sync_account_id, o.owner_person_id) IS NOT NULL
ON CONFLICT DO NOTHING;
