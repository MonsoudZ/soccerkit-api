#!/bin/bash
# SessionStart hook for Claude Code on the web.
# Ensures Postgres is running with the dev + test databases, downloads Go
# modules, installs sqlc, and exports the connection env so `go test` /
# `go run ./cmd/api` work immediately.
set -euo pipefail

# Only run inside Claude Code on the web (a real dev machine already has its
# own Postgres/toolchain).
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

# 1. Start the Postgres cluster if it is installed and not already up.
if command -v pg_ctlcluster >/dev/null 2>&1; then
  pg_ctlcluster 16 main start 2>/dev/null || true
fi

# 2. Wait for Postgres, then ensure the role password and databases exist.
if command -v psql >/dev/null 2>&1; then
  for _ in $(seq 1 15); do
    pg_isready -h localhost -U postgres >/dev/null 2>&1 && break
    sleep 1
  done
  su postgres -c "psql -c \"ALTER USER postgres PASSWORD 'postgres';\"" >/dev/null 2>&1 || true
  for db in soccerkit soccerkit_test; do
    su postgres -c "psql -tc \"SELECT 1 FROM pg_database WHERE datname='${db}'\" | grep -q 1 \
      || psql -c 'CREATE DATABASE ${db}'" >/dev/null 2>&1 || true
  done
fi

# 3. Go dependencies. sqlc is only needed to *regenerate* the query layer
# (the generated code is committed), so install it best-effort and quietly.
go mod download
command -v sqlc >/dev/null 2>&1 || go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest >/dev/null 2>&1 || true

# 4. Persist connection + auth env for the session.
{
  echo 'export DATABASE_URL="postgresql://postgres:postgres@localhost:5432/soccerkit?sslmode=disable"'
  echo 'export TEST_DATABASE_URL="postgresql://postgres:postgres@localhost:5432/soccerkit_test?sslmode=disable"'
  echo 'export JWT_ACCESS_SECRET="dev-access-secret"'
  echo 'export JWT_REFRESH_SECRET="dev-refresh-secret"'
  echo 'export PATH="$PATH:$(go env GOPATH)/bin"'
} >> "$CLAUDE_ENV_FILE"

echo "SoccerKit dev environment ready (Postgres up, databases created, deps downloaded)."
