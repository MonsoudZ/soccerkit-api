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

	return &Config{
		Env:             getenv("ENV", "development"),
		Port:            port,
		DatabaseURL:     dbURL,
		JWTAccessSecret: []byte(accessSecret),
		JWTAccessTTL:    accessTTL,
		JWTRefreshTTL:   refreshTTL,
		CORSOrigins:     getenv("CORS_ORIGINS", "*"),
		AppleClientID:   appleClientID,
		DevAppleBypass:  bypass,
	}, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
