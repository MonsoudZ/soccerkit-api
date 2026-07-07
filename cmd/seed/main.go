// Command seed loads idempotent sample data for local development: one coach
// (login coach@soccerkit.dev / password123), their personal org, a team with a
// few athletes on the roster, and a couple of pre-game check-ins to populate
// the evaluation aggregates.
package main

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/monsoudz/soccerkit-api/internal/config"
	"github.com/monsoudz/soccerkit-api/internal/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal(err)
	}
	if err := seed(ctx, pool, string(hash)); err != nil {
		log.Fatal(err)
	}
	log.Println("seed complete — coach logs in as coach@soccerkit.dev / password123")
}

const (
	orgID    = "00000000-0000-0000-0000-0000000000f0"
	coachID  = "00000000-0000-0000-0000-000000000c01"
	teamID   = "00000000-0000-0000-0000-000000000071"
	preTplID = "00000000-0000-0000-0000-0000000000e1"
)

func seed(ctx context.Context, pool *pgxpool.Pool, hash string) error {
	// Organization + coach identity.
	if _, err := pool.Exec(ctx, `
		INSERT INTO organizations (id, name, kind) VALUES ($1, 'Riverside FC', 'personal')
		ON CONFLICT (id) DO NOTHING`, orgID); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO persons (id, display_name, email) VALUES ($1, 'Coach Rivera', 'coach@soccerkit.dev')
		ON CONFLICT (id) DO NOTHING`, coachID); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_accounts (person_id, email, password_hash) VALUES ($1, 'coach@soccerkit.dev', $2)
		ON CONFLICT (email) DO NOTHING`, coachID, hash); err != nil {
		return err
	}
	for _, role := range []string{"admin", "director", "coach"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO memberships (person_id, organization_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (person_id, organization_id, role) DO NOTHING`, coachID, orgID, role); err != nil {
			return err
		}
	}

	// A team and three athletes on its roster.
	if _, err := pool.Exec(ctx, `
		INSERT INTO teams (id, organization_id, name, age_group, season)
		VALUES ($1, $2, 'Riverside U11', 'U11', '2026 Spring')
		ON CONFLICT (id) DO NOTHING`, teamID, orgID); err != nil {
		return err
	}
	athletes := []struct{ id, name string }{
		{"00000000-0000-0000-0000-0000000000a1", "Ana Duarte"},
		{"00000000-0000-0000-0000-0000000000a2", "Bruno Silva"},
		{"00000000-0000-0000-0000-0000000000a3", "Cira Mendez"},
	}
	for i, a := range athletes {
		if _, err := pool.Exec(ctx, `
			INSERT INTO persons (id, display_name) VALUES ($1, $2)
			ON CONFLICT (id) DO NOTHING`, a.id, a.name); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO memberships (person_id, organization_id, role) VALUES ($1, $2, 'player')
			ON CONFLICT (person_id, organization_id, role) DO NOTHING`, a.id, orgID); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO roster_memberships (person_id, team_id, jersey_number)
			SELECT $1, $2, $3
			WHERE NOT EXISTS (
				SELECT 1 FROM roster_memberships WHERE person_id = $1 AND team_id = $2 AND left_on IS NULL
			)`, a.id, teamID, int32(i+7)); err != nil {
			return err
		}
	}

	// A pre-game template with two scored fields + two check-ins for Ana.
	if _, err := pool.Exec(ctx, `
		INSERT INTO form_templates (id, organization_id, author_person_id, context, name, subject_type, is_seed)
		VALUES ($1, $2, $3, 'pre_game', 'Pre-Game Check-In', 'athlete', true)
		ON CONFLICT (id) DO NOTHING`, preTplID, orgID, coachID); err != nil {
		return err
	}
	fields := []struct{ id, key, label string }{
		{"00000000-0000-0000-0000-0000000000f1", "sleep", "Sleep quality"},
		{"00000000-0000-0000-0000-0000000000f2", "energy", "Energy"},
	}
	for i, f := range fields {
		if _, err := pool.Exec(ctx, `
			INSERT INTO form_fields (id, template_id, key, label, kind, position, config)
			VALUES ($1, $2, $3, $4, 'scale', $5, '{"min":1,"max":5}')
			ON CONFLICT (template_id, key) DO NOTHING`, f.id, preTplID, f.key, f.label, int32(i)); err != nil {
			return err
		}
	}
	checkIns := []struct {
		id            string
		sleep, energy float64
	}{
		{"00000000-0000-0000-0000-0000000000c1", 4, 5},
		{"00000000-0000-0000-0000-0000000000c2", 2, 3},
	}
	for _, c := range checkIns {
		if _, err := pool.Exec(ctx, `
			INSERT INTO form_instances (id, template_id, subject_person_id, submitted_by_person_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
			c.id, preTplID, athletes[0].id, coachID); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO form_answers (instance_id, field_id, numeric_value) VALUES
				($1, $2, $3), ($1, $4, $5)
			ON CONFLICT (instance_id, field_id) DO NOTHING`,
			c.id, fields[0].id, c.sleep, fields[1].id, c.energy); err != nil {
			return err
		}
	}
	return nil
}
