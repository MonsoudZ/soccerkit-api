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
	apple *appleVerifier
}

func NewServer(cfg *config.Config, pool *pgxpool.Pool) *Server {
	return &Server{
		cfg:   cfg,
		pool:  pool,
		store: store.New(pool),
		apple: newAppleVerifier(cfg.AppleClientID, cfg.DevAppleBypass),
	}
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
		// Auth (public)
		r.Route("/auth", func(r chi.Router) {
			r.Post("/register", s.handleRegister)
			r.Post("/login", s.handleLogin)
			r.Post("/apple", s.handleAppleAuth)
			r.Post("/refresh", s.handleRefresh)
			r.Post("/logout", s.handleLogout)
		})

		// Everything below requires an authenticated Person.
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)

			r.Get("/me", s.handleGetMe)
			r.Delete("/me", s.handleDeleteMe)

			// iOS opaque delta-sync (projection over the domain tables).
			r.Get("/sync", s.handleSyncPull)
			r.Post("/sync", s.handleSyncPush)

			// People (athletes / coaches / parents as Persons)
			r.Route("/persons", func(r chi.Router) {
				r.Post("/", s.handleCreatePerson)
				r.Get("/{id}", s.handleGetPerson)
				r.Get("/{id}/instances", s.handleListPersonInstances)
				r.Get("/{id}/aggregate", s.handlePersonAggregate)
			})

			// Teams & time-bounded roster
			r.Route("/teams", func(r chi.Router) {
				r.Get("/", s.handleListTeams)
				r.Post("/", s.handleCreateTeam)
				r.Get("/{id}", s.handleGetTeam)
				r.Delete("/{id}", s.handleDeleteTeam)
				r.Post("/{id}/roster", s.handleAddRoster)
				r.Delete("/{id}/roster/{personId}", s.handleEndRoster)
				// Game day
				r.Get("/{id}/games", s.handleListGames)
				r.Post("/{id}/games", s.handleCreateGame)
			})
			r.Route("/games", func(r chi.Router) {
				r.Get("/{id}", s.handleGetGame)
				r.Patch("/{id}", s.handleUpdateGame)
			})

			// Training content
			r.Route("/drills", func(r chi.Router) {
				r.Get("/", s.handleListDrills)
				r.Post("/", s.handleCreateDrill)
			})
			r.Route("/sessions", func(r chi.Router) {
				r.Get("/", s.handleListSessions)
				r.Post("/", s.handleCreateSession)
				r.Get("/{id}", s.handleGetSession)
				r.Delete("/{id}", s.handleDeleteSession)
			})

			// Evaluation engine
			r.Route("/templates", func(r chi.Router) {
				r.Get("/", s.handleListTemplates)
				r.Post("/", s.handleCreateTemplate)
				r.Get("/{id}", s.handleGetTemplate)
			})
			r.Route("/form-instances", func(r chi.Router) {
				r.Post("/", s.handleSubmitInstance)
				r.Get("/{id}", s.handleGetInstance)
			})
		})
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
