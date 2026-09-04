// Package api wires the HTTP router, middleware and handlers for the SoccerKit API.
package api

import (
	"context"
	"log"
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
	cfg         *config.Config
	pool        *pgxpool.Pool
	store       *store.Queries
	apple       *appleVerifier
	authLimiter *ipRateLimiter
	// notifier delivers pushes. Nil until SetNotifier installs one, which cmd/api does
	// and no test does -- see the Notifier interface in handlers_devices.go.
	notifier Notifier
}

// SetNotifier installs the push delivery this server uses. Kept off NewServer for the
// reason the refresh-token reaper lives in cmd/api: it is a property of the running
// process, not of every test that builds a server, and a test that accidentally spoke to
// Apple would be a bad afternoon.
func (s *Server) SetNotifier(n Notifier) { s.notifier = n }

func NewServer(cfg *config.Config, pool *pgxpool.Pool) *Server {
	return &Server{
		cfg:         cfg,
		pool:        pool,
		store:       store.New(pool),
		apple:       newAppleVerifier(cfg.AppleClientID, cfg.DevAppleBypass),
		authLimiter: newIPRateLimiter(authRate, authBurst),
	}
}

// Router builds the chi router with all routes and middleware mounted.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(s.clientIP())
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(limitBody)

	origins := []string{"*"}
	if s.cfg.CORSOrigins != "*" {
		origins = splitTrim(s.cfg.CORSOrigins)
	} else if s.cfg.IsDeployed() {
		// Not fatal: with AllowCredentials off and bearer-token auth, a wildcard origin
		// does not by itself let a page read anything it does not already hold a token
		// for. Still worth saying out loud, because it is a default nobody chose.
		log.Printf("warning: CORS_ORIGINS is %q in a deployed environment (ENV=%s); "+
			"set it to your actual origins", s.cfg.CORSOrigins, s.cfg.Env)
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: origins,
		AllowedMethods: []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		// X-Organization-ID is how a caller picks which org to act in, so a browser
		// client could not send it while it was missing here: preflight rejected it.
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Organization-ID"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/health", s.handleHealth)
	r.Get("/ready", s.handleReady)
	r.Get("/openapi.yaml", s.handleOpenAPISpec)
	r.Get("/docs", s.handleDocs)

	r.Route("/api/v1", func(r chi.Router) {
		// Auth (public). Sign in with Apple is the only way into an account: there is no
		// registration or password login, so there is no address anybody typed and no
		// credential this service has to store, verify or reset. See handlers_auth.go.
		//
		// Throttled per IP in deployed environments: these are the only unauthenticated
		// endpoints, /apple verifies an RSA signature on an attacker-supplied token, and
		// /refresh is a lookup on a value the caller chooses. Left off on a laptop, where
		// the only traffic is the developer and the test suite — the same fail-closed
		// IsDeployed switch config.go uses for the other environment-specific behaviour.
		r.Route("/auth", func(r chi.Router) {
			if s.hasAuthLimiter() {
				r.Use(s.authLimiter.middleware)
			}
			r.Post("/apple", s.handleAppleAuth)
			r.Post("/refresh", s.handleRefresh)
			r.Post("/logout", s.handleLogout)
		})

		// Everything below requires an authenticated Person.
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)

			r.Get("/me", s.handleGetMe)
			r.Delete("/me", s.handleDeleteMe)
			r.Get("/me/invitations", s.handleListMyInvitations)
			// The teams this person is rostered on — narrower than GET /teams, which
			// answers what they may see rather than what they are part of.
			r.Get("/me/teams", s.handleListMyTeams)

			// Where to reach this person. Registered on launch, dropped on sign-out.
			r.Post("/me/devices", s.handleRegisterDevice)
			r.Delete("/me/devices/{token}", s.handleUnregisterDevice)

			// Answering an invitation is not scoped to an organization: the caller is
			// not a member of it yet, so resolveOrg could not name it.
			r.Post("/invitations/{invitationId}/accept", s.handleAcceptInvitation)
			r.Post("/invitations/{invitationId}/decline", s.handleDeclineInvitation)

			// iOS opaque delta-sync (projection over the domain tables).
			r.Get("/sync", s.handleSyncPull)
			r.Post("/sync", s.handleSyncPush)

			// People (athletes / coaches / parents as Persons)
			r.Route("/persons", func(r chi.Router) {
				r.Post("/", s.handleCreatePerson)
				r.Get("/{id}", s.handleGetPerson)
				r.Patch("/{id}", s.handleUpdatePerson)
				r.Get("/{id}/instances", s.handleListPersonInstances)
				// Guardianships: what makes the parent role narrower than the club.
				r.Get("/{id}/guardians", s.handleListGuardians)
				r.Post("/{id}/guardians", s.handleAddGuardian)
				r.Delete("/{id}/guardians/{personId}", s.handleRemoveGuardian)
				r.Get("/{id}/aggregate", s.handlePersonAggregate)
			})

			// Teams & time-bounded roster
			r.Route("/teams", func(r chi.Router) {
				r.Get("/", s.handleListTeams)
				r.Post("/", s.handleCreateTeam)
				r.Get("/{id}", s.handleGetTeam)
				r.Patch("/{id}", s.handleUpdateTeam)
				r.Delete("/{id}", s.handleDeleteTeam)
				// Who runs a team, which is what scopes GET /teams for a coach.
				r.Get("/{id}/staff", s.handleListTeamStaff)
				r.Post("/{id}/staff", s.handleAddTeamStaff)
				r.Delete("/{id}/staff/{personId}", s.handleRemoveTeamStaff)
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

			// The club itself (rename only -- see updatableOrganizationFields).
			r.Route("/organizations", func(r chi.Router) {
				r.Patch("/{id}", s.handleUpdateOrganization)
				// Who is in the club, and what they may do.
				r.Get("/{id}/members", s.handleListMembers)
				r.Patch("/{id}/members/{personId}", s.handleSetMemberRoles)
				r.Delete("/{id}/members/{personId}", s.handleRemoveMember)
				// Joining is an offer the invitee answers -- there is no direct add.
				r.Get("/{id}/invitations", s.handleListInvitations)
				r.Post("/{id}/invitations", s.handleCreateInvitation)
				r.Delete("/{id}/invitations/{invitationId}", s.handleRevokeInvitation)
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

// clientIP records who is calling, for the rate limiter to bucket on. It replaces
// middleware.RealIP, which chi deprecates as spoofable and which was doing real
// damage here: RealIP overwrites r.RemoteAddr from True-Client-IP, X-Real-IP or
// X-Forwarded-For with no notion of which hop wrote them, so a caller who varied
// that header got a fresh bucket on every request and the credential endpoints
// were effectively unthrottled — the exact attack the limiter exists to stop.
//
// The replacement never trusts a header the caller could have written. With no
// TRUSTED_PROXIES configured the client is the TCP peer; with proxies configured,
// chi walks X-Forwarded-For from the right and takes the first entry outside
// those ranges, which is the leftmost address our own infrastructure vouches for.
func (s *Server) clientIP() func(http.Handler) http.Handler {
	if len(s.cfg.TrustedProxies) == 0 {
		if s.cfg.IsDeployed() {
			// Not fatal, and it fails closed rather than open: behind an unlisted proxy
			// every caller shares that proxy's address, so the limit becomes global
			// instead of per-client. Worth saying out loud, because it turns a brake on
			// one attacker into a brake on everybody.
			log.Printf("warning: TRUSTED_PROXIES is unset in a deployed environment (ENV=%s); "+
				"if this process sits behind a load balancer, set it to the balancer's CIDRs "+
				"or every caller shares one rate-limit bucket", s.cfg.Env)
		}
		return middleware.ClientIPFromRemoteAddr
	}
	return middleware.ClientIPFromXFF(s.cfg.TrustedProxies...)
}

// handleHealth answers whether this process is running. It deliberately touches
// nothing: a liveness check that can fail on a dependency asks a supervisor to restart a
// process over a problem restarting will not fix.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady answers whether this process can serve a request, which is the question a
// load balancer is really asking and the one /health cannot answer.
//
// database.Connect pings at boot, so an unreachable database used to stop a process from
// starting — and that was the whole of it. A database that went away afterwards (a
// failover, a rotated credential, an exhausted pool) left /health saying "ok" while every
// request returned 500, with nothing in the platform's control loop able to tell.
//
// The timeout is short and separate from the request's: a readiness probe that hangs is
// a readiness probe that has already failed, and it must not sit in the pool's queue
// behind the very saturation it is meant to report.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.pool.Ping(ctx); err != nil {
		log.Printf("readiness: database unreachable: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "database",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
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
