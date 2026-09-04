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

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/monsoudz/soccerkit-api/internal/api"
	"github.com/monsoudz/soccerkit-api/internal/config"
	"github.com/monsoudz/soccerkit-api/internal/database"
	"github.com/monsoudz/soccerkit-api/internal/push"
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

	// Push delivery, installed here for the same reason as the reaper above: it holds
	// Apple's signing key and talks to their servers, so it is a property of this process
	// and not of every test that builds a Server. With APNs unconfigured the server keeps
	// its nil notifier and simply tells nobody, which is what a developer's machine
	// should do.
	if cfg.PushConfigured() {
		sender, err := newPushSender(cfg, store.New(pool))
		if err != nil {
			return fmt.Errorf("push notifications: %w", err)
		}
		pushCtx, stopPush := context.WithCancel(ctx)
		defer stopPush()
		go sender.Run(pushCtx)
		srv.SetNotifier(pushNotifier{sender})
		log.Printf("push notifications enabled (bundle %s, %s)", cfg.APNsBundleID, apnsHostName(cfg))
	} else {
		log.Printf("push notifications disabled (APNS_* unset); invitations will not notify")
	}

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

// newPushSender parses the APNs key and builds the sender.
//
// The key is parsed at boot rather than at first push, so a malformed one fails the
// deploy instead of failing silently the first time somebody is invited -- the same
// reasoning as config.validateAPNs, which checks that the four values arrive together.
func newPushSender(cfg *config.Config, q *store.Queries) (*push.Sender, error) {
	key, err := jwt.ParseECPrivateKeyFromPEM([]byte(cfg.APNsPrivateKey))
	if err != nil {
		return nil, fmt.Errorf("APNS_PRIVATE_KEY is not a PEM EC private key (expect the .p8 "+
			"Apple issued, newlines and all): %w", err)
	}
	host := push.SandboxHost
	if cfg.APNsProduction {
		host = push.ProductionHost
	}
	return push.NewSender(q, push.Config{
		KeyID: cfg.APNsKeyID, TeamID: cfg.APNsTeamID, BundleID: cfg.APNsBundleID,
		PrivateKey: key, Host: host,
	}), nil
}

func apnsHostName(cfg *config.Config) string {
	if cfg.APNsProduction {
		return "production"
	}
	return "sandbox"
}

// pushNotifier joins the two halves without either package importing the other.
//
// internal/api says who to tell and what about; internal/push knows Apple. Each declares
// the notification shape it needs, and this converts between them -- four fields of
// copying, in exchange for a delivery package that the API does not depend on and a test
// server that cannot accidentally acquire one.
type pushNotifier struct{ sender *push.Sender }

func (p pushNotifier) Notify(ctx context.Context, personID uuid.UUID, note api.Notification) {
	p.sender.Notify(ctx, personID, push.Notification{
		Title: note.Title, Body: note.Body, Data: note.Data,
	})
}
