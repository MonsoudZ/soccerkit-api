package config

import (
	"os"
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
		"ENV":               "development",
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

// An unset ENV must not boot. It used to default to "development" — the most
// permissive value in the set — which meant a deployment that simply never set the
// variable ran with a placeholder signing secret, DEV_APPLE_BYPASS honoured and no
// throttle on the credential endpoints. Every guard below existed and none of them ran.
func TestUnsetEnvRefusesToBoot(t *testing.T) {
	env(t, map[string]string{
		"ENV":               "",
		"DEV_APPLE_BYPASS":  "true",
		"APPLE_CLIENT_ID":   "",
		"JWT_ACCESS_SECRET": "secret", // a listed placeholder, six bytes
	})
	cfg, err := Load()
	if err == nil {
		t.Fatalf("unset ENV booted: Env=%q IsDeployed=%v DevAppleBypass=%v secretLen=%d",
			cfg.Env, cfg.IsDeployed(), cfg.DevAppleBypass, len(cfg.JWTAccessSecret))
	}
	if !strings.Contains(err.Error(), "ENV") {
		t.Errorf("the error should name ENV, got %v", err)
	}
}

// ENV=test is a laptop and a CI runner, not a server: it keeps the escape hatches.
func TestTestEnvIsNotDeployed(t *testing.T) {
	env(t, map[string]string{
		"ENV": "test", "DEV_APPLE_BYPASS": "true", "APPLE_CLIENT_ID": "",
		"JWT_ACCESS_SECRET": "test-access-secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("ENV=test must boot with the test harness's own settings: %v", err)
	}
	if cfg.IsDeployed() {
		t.Error("ENV=test is not deployed")
	}
}

// TestDeployedRejectsTheEnvExampleSecret — placeholderSecrets listed several literals
// but not the one this repo actually ships in .env.example. It was caught only by the
// length minimum, which is luck rather than the check written for it.
func TestDeployedRejectsTheEnvExampleSecret(t *testing.T) {
	envExample, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	var secret string
	for _, line := range strings.Split(string(envExample), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "JWT_ACCESS_SECRET="); ok {
			secret = v
			break
		}
	}
	if secret == "" {
		t.Fatal("no JWT_ACCESS_SECRET in .env.example")
	}
	if !placeholderSecrets[secret] {
		t.Errorf("the secret shipped in .env.example (%q) is not in placeholderSecrets, "+
			"so a deployment using it verbatim is refused only if it happens to be short", secret)
	}
}
