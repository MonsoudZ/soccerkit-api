// Package config loads and validates process configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Env             string
	Port            int
	DatabaseURL     string
	JWTAccessSecret []byte
	JWTAccessTTL    time.Duration
	JWTRefreshTTL   time.Duration
	CORSOrigins     string

	// AppleClientID is the expected `aud` of the Apple identity token — the iOS
	// app's bundle identifier. Required for Sign in with Apple unless
	// DevAppleBypass is set.
	AppleClientID string
	// DevAppleBypass trusts the identity token's claims without verifying Apple's
	// signature. Local development only — never enable in a deployed environment.
	DevAppleBypass bool
}

// Load reads configuration from the environment, applying defaults for local
// development and returning an error if a required value is missing.
func Load() (*Config, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	accessSecret := getenv("JWT_ACCESS_SECRET", "")
	if accessSecret == "" {
		return nil, fmt.Errorf("JWT_ACCESS_SECRET is required")
	}

	port, err := strconv.Atoi(getenv("PORT", "3000"))
	if err != nil {
		return nil, fmt.Errorf("invalid PORT: %w", err)
	}

	accessTTL, err := time.ParseDuration(getenv("JWT_ACCESS_TTL", "15m"))
	if err != nil {
		return nil, fmt.Errorf("invalid JWT_ACCESS_TTL: %w", err)
	}
	refreshTTL, err := time.ParseDuration(getenv("JWT_REFRESH_TTL", "720h")) // 30 days
	if err != nil {
		return nil, fmt.Errorf("invalid JWT_REFRESH_TTL: %w", err)
	}

	bypass := getenv("DEV_APPLE_BYPASS", "") == "true"
	appleClientID := getenv("APPLE_CLIENT_ID", "")
	if appleClientID == "" && !bypass {
		return nil, fmt.Errorf("APPLE_CLIENT_ID is required for Sign in with Apple (or set DEV_APPLE_BYPASS=true for local dev)")
	}

	cfg := &Config{
		Env:             getenv("ENV", "development"),
		Port:            port,
		DatabaseURL:     dbURL,
		JWTAccessSecret: []byte(accessSecret),
		JWTAccessTTL:    accessTTL,
		JWTRefreshTTL:   refreshTTL,
		CORSOrigins:     getenv("CORS_ORIGINS", "*"),
		AppleClientID:   appleClientID,
		DevAppleBypass:  bypass,
	}
	if err := cfg.validateDeployed(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// IsDeployed reports whether this process is running outside a developer's
// machine. It fails closed: anything that is not explicitly development or test
// counts as deployed, so a typo'd or unset-in-CI ENV cannot silently unlock the
// development-only escape hatches below.
func (c *Config) IsDeployed() bool {
	return c.Env != "development" && c.Env != "test"
}

// minSecretLen is the shortest JWT signing secret accepted in a deployed
// environment. 32 bytes matches the HMAC-SHA256 output the tokens are signed with.
const minSecretLen = 32

// placeholderSecrets are the values shipped in this repo's own compose file,
// .env.example, README and test setup. They are public, so a deployment using one
// has a forgeable token for every account.
var placeholderSecrets = map[string]bool{
	"change-me-in-production":     true,
	"change-me-too-in-production": true,
	"change-me":                   true,
	"dev-access-secret":           true,
	"dev-refresh-secret":          true,
	"test-access-secret":          true,
	"test-refresh-secret":         true,
	"secret":                      true,
}

// validateDeployed refuses to boot a deployed process that is configured
// insecurely. Every check here guards a setting that is safe (and convenient) on
// a laptop and catastrophic on the internet, and each was previously reachable in
// production with no guard at all — Env was set and then never read anywhere.
func (c *Config) validateDeployed() error {
	if !c.IsDeployed() {
		return nil
	}
	if c.DevAppleBypass {
		return fmt.Errorf("DEV_APPLE_BYPASS must not be set when ENV=%q: it skips Apple's "+
			"signature, issuer, audience and expiry checks entirely, so anyone can mint an "+
			"unsigned token for any account", c.Env)
	}
	if placeholderSecrets[string(c.JWTAccessSecret)] {
		return fmt.Errorf("JWT_ACCESS_SECRET is a placeholder value from this repo when ENV=%q: "+
			"it is public, so every access token is forgeable", c.Env)
	}
	if len(c.JWTAccessSecret) < minSecretLen {
		return fmt.Errorf("JWT_ACCESS_SECRET must be at least %d bytes when ENV=%q (got %d)",
			minSecretLen, c.Env, len(c.JWTAccessSecret))
	}
	return nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
