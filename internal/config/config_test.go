package config

import (
	"strings"
	"testing"
)

// internal/config had no tests, which is not a coincidence: it is where the two
// worst misconfigurations in the project lived. DEV_APPLE_BYPASS disables Apple
// signature verification outright, Env was set and then read nowhere, and the
// repo's own docker-compose.yml shipped ENV=production with the bypass on and a
// published signing secret. A deployed process must not boot in that state.

const goodSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

func env(t *testing.T, kv map[string]string) {
	t.Helper()
	base := map[string]string{
		"DATABASE_URL":      "postgresql://localhost:5432/x",
		"JWT_ACCESS_SECRET": goodSecret,
		"APPLE_CLIENT_ID":   "com.example.app",
		"DEV_APPLE_BYPASS":  "",
		"ENV":               "",
		"CORS_ORIGINS":      "",
		"PORT":              "",
		"JWT_ACCESS_TTL":    "",
		"JWT_REFRESH_TTL":   "",
	}
	for k, v := range kv {
		base[k] = v
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
}

func TestDeployedRejectsAppleBypass(t *testing.T) {
	for _, e := range []string{"production", "staging", "prod", "anything-not-development"} {
		env(t, map[string]string{"ENV": e, "DEV_APPLE_BYPASS": "true"})
		_, err := Load()
		if err == nil {
			t.Fatalf("ENV=%q with DEV_APPLE_BYPASS=true booted; it must refuse", e)
		}
		if !strings.Contains(err.Error(), "DEV_APPLE_BYPASS") {
			t.Errorf("ENV=%q: error should name DEV_APPLE_BYPASS, got %v", e, err)
		}
	}
}

func TestDeployedRejectsPlaceholderSecret(t *testing.T) {
	// The exact values this repo ships in docker-compose.yml, .env.example and
	// the README quick-start.
	for _, secret := range []string{"change-me-in-production", "dev-access-secret", "test-access-secret"} {
		env(t, map[string]string{"ENV": "production", "JWT_ACCESS_SECRET": secret})
		if _, err := Load(); err == nil {
			t.Errorf("ENV=production with the published secret %q booted; it must refuse", secret)
		}
	}
}

func TestDeployedRejectsShortSecret(t *testing.T) {
	env(t, map[string]string{"ENV": "production", "JWT_ACCESS_SECRET": "short-but-not-a-placeholder"})
	if _, err := Load(); err == nil {
		t.Error("ENV=production with a 27-byte secret booted; it must require 32")
	}
}

func TestDeployedBootsWhenConfiguredProperly(t *testing.T) {
	env(t, map[string]string{"ENV": "production", "JWT_ACCESS_SECRET": goodSecret})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a properly configured production process must boot: %v", err)
	}
	if !cfg.IsDeployed() {
		t.Error("ENV=production should be deployed")
	}
	if cfg.DevAppleBypass {
		t.Error("bypass should be off")
	}
}

// Development keeps every escape hatch: the whole point is that a laptop can run
// the stack without an Apple client id or a real secret.
func TestDevelopmentKeepsTheEscapeHatches(t *testing.T) {
	env(t, map[string]string{
		"ENV":               "development",
		"DEV_APPLE_BYPASS":  "true",
		"APPLE_CLIENT_ID":   "",
		"JWT_ACCESS_SECRET": "dev-access-secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("development must still boot with the bypass and a dev secret: %v", err)
	}
	if !cfg.DevAppleBypass {
		t.Error("development should keep the bypass")
	}
	if cfg.IsDeployed() {
		t.Error("development is not deployed")
	}
}

// An unset ENV means a laptop, not a server: it is the default in getenv.
func TestUnsetEnvIsDevelopment(t *testing.T) {
	env(t, map[string]string{"ENV": "", "DEV_APPLE_BYPASS": "true", "APPLE_CLIENT_ID": ""})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unset ENV should behave as development: %v", err)
	}
	if cfg.Env != "development" || cfg.IsDeployed() {
		t.Errorf("unset ENV = %q, IsDeployed=%v; want development/false", cfg.Env, cfg.IsDeployed())
	}
}
