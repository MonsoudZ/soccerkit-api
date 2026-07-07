// Package api wires the HTTP router, middleware and handlers for the SoccerKit API.
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/monsoudz/soccerkit-api/internal/config"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

type Server struct {
	cfg   *config.Config
	pool  *pgxpool.Pool
	store *store.Queries
}

func NewServer(cfg *config.Config, pool *pgxpool.Pool) *Server {
	return &Server{cfg: cfg, pool: pool, store: store.New(pool)}
}

// Router builds the chi router with all routes and middleware mounted.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	origins := []string{"*"}
	if s.cfg.CORSOrigins != "*" {
		origins = splitTrim(s.cfg.CORSOrigins)
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   origins,
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/health", s.handleHealth)
	r.Get("/openapi.yaml", s.handleOpenAPISpec)
	r.Get("/docs", s.handleDocs)

	r.Route("/api/v1", func(r chi.Router) {
		// auth
		r.Route("/auth", func(r chi.Router) {
			r.Post("/register", s.handleRegister)
			r.Post("/login", s.handleLogin)
			r.Post("/refresh", s.handleRefresh)
			r.Post("/logout", s.handleLogout)
		})

		// users / profiles
		r.Get("/players", s.handleListPlayers)
		r.Get("/players/{id}", s.handleGetPlayer)
		r.Get("/players/{id}/stats", s.handlePlayerCareerStats)
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/me", s.handleGetMe)
			r.Patch("/me", s.handleUpdateMe)
		})

		// venues
		r.Route("/venues", func(r chi.Router) {
			r.Get("/", s.handleListVenues)
			r.Get("/{id}", s.handleGetVenue)
			r.With(s.requireAuth).Post("/", s.handleCreateVenue)
		})

		// matches
		r.Route("/matches", func(r chi.Router) {
			r.Get("/", s.handleListMatches)
			r.Get("/{id}", s.handleGetMatch)
			r.Get("/{id}/stats", s.handleListMatchStats)
			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth)
				r.Post("/", s.handleCreateMatch)
				r.Patch("/{id}", s.handleUpdateMatch)
				r.Delete("/{id}", s.handleCancelMatch)
				r.Put("/{id}/rsvp", s.handleRsvp)
				r.Delete("/{id}/rsvp", s.handleLeaveMatch)
				r.Put("/{id}/stats", s.handleRecordMatchStat)
			})
		})

		// teams
		r.Route("/teams", func(r chi.Router) {
			r.Get("/", s.handleListTeams)
			r.Get("/{id}", s.handleGetTeam)
			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth)
				r.Post("/", s.handleCreateTeam)
				r.Patch("/{id}", s.handleUpdateTeam)
				r.Delete("/{id}", s.handleDeleteTeam)
				r.Post("/{id}/members", s.handleAddTeamMember)
				r.Delete("/{id}/members/{userId}", s.handleRemoveTeamMember)
			})
		})

		// leagues
		r.Route("/leagues", func(r chi.Router) {
			r.Get("/", s.handleListLeagues)
			r.Get("/{id}", s.handleGetLeague)
			r.Get("/{id}/fixtures", s.handleListFixtures)
			r.Get("/{id}/standings", s.handleStandings)
			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth)
				r.Post("/", s.handleCreateLeague)
				r.Post("/{id}/teams", s.handleAddLeagueTeam)
				r.Post("/{id}/fixtures", s.handleCreateFixture)
				r.Put("/fixtures/{fixtureId}/result", s.handleRecordResult)
			})
		})

		// fixture stats
		r.Get("/fixtures/{id}/stats", s.handleListFixtureStats)
		r.With(s.requireAuth).Put("/fixtures/{id}/stats", s.handleRecordFixtureStat)
	})

	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
