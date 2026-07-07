// Command seed loads idempotent sample data for local development.
// Every player logs in with the password "password123".
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
	log.Println("seed complete — players log in with password \"password123\"")
}

func seed(ctx context.Context, pool *pgxpool.Pool, hash string) error {
	// Users are inserted first with the parameterised password hash.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, position, skill_level) VALUES
			('00000000-0000-0000-0000-000000000001', 'alice@soccerkit.dev', $1, 'Alice Ramos', 'FWD', 5),
			('00000000-0000-0000-0000-000000000002', 'ben@soccerkit.dev',   $1, 'Ben Novak',   'GK',  4),
			('00000000-0000-0000-0000-000000000003', 'chloe@soccerkit.dev', $1, 'Chloe Diallo','MID', 4),
			('00000000-0000-0000-0000-000000000004', 'diego@soccerkit.dev', $1, 'Diego Costa', 'DEF', 3)
		 ON CONFLICT (id) DO NOTHING`, hash); err != nil {
		return err
	}

	stmts := []string{
		`INSERT INTO venues (id, name, city, surface, latitude, longitude) VALUES
			('00000000-0000-0000-0000-0000000000a1', 'Riverside Astro Pitches', 'Portland', 'TURF', 45.5231, -122.6765)
		 ON CONFLICT (id) DO NOTHING`,

		`INSERT INTO matches (id, host_id, venue_id, title, description, format, max_players, kickoff_at) VALUES
			('00000000-0000-0000-0000-0000000000b1',
			 '00000000-0000-0000-0000-000000000001',
			 '00000000-0000-0000-0000-0000000000a1',
			 'Sunday Morning Kickabout', 'Casual 5-a-side, all levels welcome.', '5v5', 10,
			 '2026-08-02T10:00:00Z')
		 ON CONFLICT (id) DO NOTHING`,

		`INSERT INTO match_rsvps (match_id, user_id, status) VALUES
			('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-000000000001', 'GOING'),
			('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-000000000002', 'GOING'),
			('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-000000000003', 'GOING')
		 ON CONFLICT (match_id, user_id) DO NOTHING`,

		`INSERT INTO teams (id, name, owner_id) VALUES
			('00000000-0000-0000-0000-0000000000c1', 'Riverside Lions', '00000000-0000-0000-0000-000000000001'),
			('00000000-0000-0000-0000-0000000000c2', 'East Side Tigers', '00000000-0000-0000-0000-000000000004')
		 ON CONFLICT (id) DO NOTHING`,

		`INSERT INTO team_members (team_id, user_id, role) VALUES
			('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000001', 'OWNER'),
			('00000000-0000-0000-0000-0000000000c2', '00000000-0000-0000-0000-000000000004', 'OWNER')
		 ON CONFLICT (team_id, user_id) DO NOTHING`,

		`INSERT INTO leagues (id, name, season) VALUES
			('00000000-0000-0000-0000-0000000000d1', 'City Sunday League', '2026 Summer')
		 ON CONFLICT (id) DO NOTHING`,

		`INSERT INTO league_teams (league_id, team_id) VALUES
			('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000c1'),
			('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000c2')
		 ON CONFLICT (league_id, team_id) DO NOTHING`,

		`INSERT INTO fixtures (id, league_id, home_team_id, away_team_id, kickoff_at, home_score, away_score, status) VALUES
			('00000000-0000-0000-0000-0000000000e1', '00000000-0000-0000-0000-0000000000d1',
			 '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-0000000000c2',
			 '2026-08-09T15:00:00Z', 2, 1, 'COMPLETED')
		 ON CONFLICT (id) DO NOTHING`,

		`INSERT INTO player_match_stats (user_id, fixture_id, goals, assists, minutes_played) VALUES
			('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000e1', 2, 0, 90)
		 ON CONFLICT (user_id, fixture_id) DO NOTHING`,
	}

	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
