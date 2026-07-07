# SoccerKit API

Backend for the **SoccerKit** app. A JSON/REST API covering authentication,
player profiles, pickup matches with RSVPs, teams, leagues (fixtures &
standings), and per-match player statistics.

Built with **Go** — [`chi`](https://github.com/go-chi/chi) router,
[`sqlc`](https://sqlc.dev)-generated type-safe queries over
[`pgx`](https://github.com/jackc/pgx)/**PostgreSQL**, JWT auth, and an
**OpenAPI 3** spec served at `/openapi.yaml` for generating a typed Swift
client (e.g. with [swift-openapi-generator](https://github.com/apple/swift-openapi-generator)).

---

## Requirements

- Go ≥ 1.24
- PostgreSQL ≥ 13 (needs `gen_random_uuid()`, in core since PG13)

## Quick start

```bash
# 1. configure environment
cp .env.example .env        # then export the vars, or use `direnv`/`set -a; . ./.env`

# 2. point at your database and secrets
export DATABASE_URL="postgresql://postgres:postgres@localhost:5432/soccerkit?sslmode=disable"
export JWT_ACCESS_SECRET="dev-access-secret"
export JWT_REFRESH_SECRET="dev-refresh-secret"

# 3. run it — migrations apply automatically on boot
make run                    # http://localhost:3000  ·  docs at /docs

# 4. (optional) load sample data
make seed                   # players log in with password "password123"
```

Migrations are embedded in the binary (`internal/database/migrations`) and
applied on startup, tracked in a `schema_migrations` table — no separate
migrate step needed.

### With Docker Compose

```bash
docker compose up --build   # API on :3000, Postgres on :5432
```

## API surface

Feature routes are versioned under `/api/v1`. Interactive docs are at `/docs`;
the raw spec is at `/openapi.yaml`.

| Area      | Highlights |
|-----------|-----------|
| **auth**    | `POST /auth/register`, `/auth/login`, `/auth/refresh` (rotating), `/auth/logout` |
| **users**   | `GET/PATCH /me`, `GET /players` (search), `GET /players/:id`, `GET /players/:id/stats` (career totals) |
| **venues**  | `GET /venues`, `GET /venues/:id`, `POST /venues` |
| **matches** | `GET/POST /matches`, `GET/PATCH/DELETE /matches/:id`, `PUT/DELETE /matches/:id/rsvp` (waitlist + auto-promotion) |
| **teams**   | `GET/POST /teams`, `GET/PATCH/DELETE /teams/:id`, `POST/DELETE /teams/:id/members` |
| **leagues** | `GET/POST /leagues`, `POST /leagues/:id/teams`, `GET/POST /leagues/:id/fixtures`, `GET /leagues/:id/standings`, `PUT /leagues/fixtures/:id/result` |
| **stats**   | `GET/PUT /matches/:id/stats`, `GET/PUT /fixtures/:id/stats` |

### Authentication

`register`/`login`/`refresh` return an **access token** (short-lived JWT, sent
as `Authorization: Bearer <token>`) and an opaque **refresh token** (stored
server-side, rotated on every use, revocable via `logout`).

### Error format

Every error uses a consistent envelope:

```json
{ "error": { "code": "NOT_FOUND", "message": "match not found" } }
```

## Domain notes

- **Pickup matches** track a roster of RSVPs. The host is auto-confirmed. When a
  match is full, further `GOING` RSVPs land on the `WAITLIST`; freeing a slot
  promotes the longest-waiting player — done atomically in a transaction.
- **Standings** are computed on read from completed fixtures (3 pts win, 1 draw),
  ordered by points → goal difference → goals for → name.
- **Player stats** attach to either a pickup match or a league fixture; career
  totals aggregate across both.

## Project layout

```
cmd/
  api/main.go              # entrypoint: config, migrate, serve, graceful shutdown
  seed/main.go             # sample development data
internal/
  config/                  # env configuration
  database/
    database.go            # pgx pool + embedded migration runner
    migrations/*.sql        # schema (source of truth for sqlc + runtime)
  store/                   # sqlc-generated queries & models (DO NOT EDIT)
  api/
    server.go              # chi router, middleware, route mounting
    auth.go                # JWT, bcrypt, auth middleware
    respond.go helpers.go  # JSON + error envelope helpers
    dto.go                 # API response types + mapping from store models
    handlers_*.go          # one file per domain
    openapi.yaml           # served at /openapi.yaml, embedded in the binary
    *_test.go              # httptest integration tests
db/queries/*.sql           # sqlc query definitions
```

## Development

| Command | Description |
|---------|-------------|
| `make run` | Run the API |
| `make build` | Compile to `bin/api` |
| `make test` | Run the integration test suite |
| `make vet` | `go vet ./...` |
| `make sqlc` | Regenerate `internal/store` from `db/queries` |
| `make seed` | Load sample data |

### Tests

Integration tests exercise the real router against a Postgres database
(`soccerkit_test` by default; override with `TEST_DATABASE_URL`). The schema is
applied automatically:

```bash
export TEST_DATABASE_URL="postgresql://postgres:postgres@localhost:5432/soccerkit_test?sslmode=disable"
make test
```

### Regenerating queries

Edit the SQL in `db/queries/*.sql` (and the schema in
`internal/database/migrations`), then:

```bash
make sqlc
```
