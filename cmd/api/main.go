package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/monsoudz/soccerkit-api/internal/api"
	"github.com/monsoudz/soccerkit-api/internal/config"
	"github.com/monsoudz/soccerkit-api/internal/database"
	"github.com/monsoudz/soccerkit-api/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Println("migrations up to date")

	// Reap expired refresh tokens periodically. Nothing ever deleted them: the table
	// grew by a row per sign-in and per refresh, forever, and since password
	// authentication was removed a refresh token is the only credential this service
	// stores. Runs here rather than in NewServer so it is a property of the process, not
	// of every test that builds a server. See docs/AUDIT-2.md L4.
	reaperCtx, stopReaper := context.WithCancel(ctx)
	defer stopReaper()
	go reapRefreshTokens(reaperCtx, pool)

	srv := api.NewServer(cfg, pool)
	// Every timeout, not just the header one. middleware.Timeout bounds how long a
	// handler may run; it says nothing about how slowly a client may dribble out a
	// request body or read a response back, and limitBody caps the size rather than the
	// duration. Without these, one slow client holds a connection and a goroutine
	// indefinitely at no cost to itself.
	//
	// WriteTimeout is deliberately longer than the router's 30s handler timeout: it has
	// to outlast the handler, or the timeout response itself cannot be written and the
	// caller sees a dropped connection instead of a 503.
	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	shutdownErr := make(chan error, 1)
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		shutdownErr <- httpServer.Shutdown(ctx)
	}()

	log.Printf("SoccerKit API listening on :%d (docs at /docs)", cfg.Port)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-shutdownErr
}

// Refresh-token reaping. The grace period is measured from expiry, not from revocation:
// a revoked row is what replay detection reads, so it has to outlive the token it
// describes -- see ReapExpiredRefreshTokens.
const (
	refreshTokenGrace   = 30 * 24 * time.Hour
	refreshReapInterval = 6 * time.Hour
)

// reapRefreshTokens deletes long-expired refresh tokens, once at boot and then on a
// ticker, until the context is cancelled.
//
// Failure is logged and not fatal. This is housekeeping: a database that cannot serve it
// has a problem the readiness probe already reports, and refusing to serve requests over
// a skipped cleanup would turn a growing table into an outage.
func reapRefreshTokens(ctx context.Context, pool *pgxpool.Pool) {
	q := store.New(pool)
	reap := func() {
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := q.ReapExpiredRefreshTokens(runCtx, pgtype.Interval{
			Microseconds: int64(refreshTokenGrace / time.Microsecond), Valid: true,
		})
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("refresh token reaper: %v", err)
			}
			return
		}
		if n > 0 {
			log.Printf("refresh token reaper: deleted %d token(s) expired more than %s ago",
				n, refreshTokenGrace)
		}
	}

	reap()
	ticker := time.NewTicker(refreshReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}
