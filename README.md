# SoccerCoachKit API

Backend for **SoccerCoachKit** — a coach → club athlete-evaluation platform.

Built to the *whole-castle* architecture: the schema models
club → director → coach → parent → player from day one, while the shipped
product is the **solo-coach on-ramp** — teams, a time-bounded roster, and the
**pre/post-game evaluation loop** with cross-instance score aggregation. Every
later tier is data that already has a home.

Stack: **Go** — [`chi`](https://github.com/go-chi/chi) router,
[`sqlc`](https://sqlc.dev)-generated type-safe queries over
[`pgx`](https://github.com/jackc/pgx)/**PostgreSQL**, JWT auth, and an
**OpenAPI 3** spec at `/openapi.yaml` for generating a typed Swift client.

---

## The five load-bearing seams

The architecture is designed so later tiers snap on without a rewrite. All five
exist in the schema now:

1. **`organizations` + `memberships` + `roles`** — tiering is a join, never a
   column. A solo coach gets a personal org auto-created at signup; a club is
   the same row with `kind = club`.
2. **`persons` ≠ `user_accounts`** — a U9 player is a Person with no login.
   Contact/medical/identity live on `persons`.
3. **`roster_memberships` are time-bounded** — no `team_id` on a person. Moving
   a player, playing up an age group, and season rollover all fall out for free.
4. **The evaluation engine is generic** — `form_templates` / `form_fields` /
   `form_instances` / `form_answers`. One primitive ("a dated, scored, noted
   response about a subject, in a context") powers tryouts, the habit loop,
   development tracking, and movement decisions.
5. **`share_grants` are polymorphic + scoped** — the entire coach-to-coach /
   club-library feature as one table.

## What's shipped (Phase 1 core)

| Area | Endpoints |
|------|-----------|
| **auth** | `POST /auth/register` (provisions Person + UserAccount + personal Org + admin/director/coach memberships + seeded templates), `/auth/login`, `/auth/apple` (Sign in with Apple — same provisioning; returns `{ token, personID }` for the iOS app), `/auth/refresh` (rotating), `/auth/logout` |
| **me** | `GET /me` — the authenticated person + their org memberships |
| **persons** | `POST /persons` (add an athlete), `GET /persons/:id`, `GET /persons/:id/instances`, `GET /persons/:id/aggregate` |
| **teams** | `GET/POST /teams`, `GET/DELETE /teams/:id`, `POST /teams/:id/roster`, `DELETE /teams/:id/roster/:personId` |
| **evaluation** | `GET/POST /templates`, `GET /templates/:id`, `POST /form-instances`, `GET /form-instances/:id` |
| **content** | `GET/POST /drills`, `GET/POST /sessions`, `GET/DELETE /sessions/:id` (sessions carry ordered blocks that can reference drills) |
| **game day** | `GET/POST /teams/:id/games`, `GET/PATCH /games/:id` (record kickoff, status, and result); post-game reports attach via a form instance's `contextRef` |

**Next up (schema already present):** `ShareGrant` scopes (coach-to-coach +
club library), the director tier, and parent/player self-service.

### The moat, concretely

Registering seeds the **pre-game check-in** (8 scale + 2 bool fields) and
**post-game report** templates. Submit instances against an athlete, then:

```
GET /api/v1/persons/{id}/aggregate?context=pre_game
→ [{ "key": "sleep", "average": 3, "samples": 12, "minimum": 1, "maximum": 5 }, …]
```

That readiness-mean / effort-trend query is a single normalized aggregation over
`form_answers` — the reason answers are columns, not a jsonb blob.

## Multi-tenancy

Every write is scoped to an organization resolved from the caller's memberships.
Send `X-Organization-ID` to act in a specific org; it defaults to your single
org (the solo-coach case). Roles are checked per request against the permission
matrix (admin/director/coach can manage; parent/player are read/self — dark
until those tiers ship).

## Quick start

```bash
export DATABASE_URL="postgresql://postgres:postgres@localhost:5432/soccerkit?sslmode=disable"
export JWT_ACCESS_SECRET="dev-access-secret"
export JWT_REFRESH_SECRET="dev-refresh-secret"

make run                    # migrations apply on boot; docs at /docs
make seed                   # coach@soccerkit.dev / password123
make test                   # needs TEST_DATABASE_URL
```

Interactive docs at `/docs`; raw spec at `/openapi.yaml`.

## Project layout

```
cmd/
  api/main.go              # entrypoint: config, migrate, serve, graceful shutdown
  seed/main.go             # sample coach/team/roster/evaluations
internal/
  config/                  # env configuration
  database/
    database.go            # pgx pool + embedded migration runner
    migrations/*.sql        # the whole-castle schema (source of truth for sqlc)
  store/                   # sqlc-generated queries & models (DO NOT EDIT)
  api/
    server.go              # chi router, middleware, route mounting
    auth.go                # JWT, bcrypt, auth middleware, org/role resolution
    dto.go                 # API response types + mapping from store models
    handlers_*.go          # auth · people · teams · forms (the engine)
    openapi.yaml           # served at /openapi.yaml, embedded in the binary
    *_test.go              # httptest integration tests
db/queries/*.sql           # sqlc query definitions
```

## Development

| Command | Description |
|---------|-------------|
| `make run` | Run the API |
| `make build` | Compile to `bin/api` |
| `make test` | Integration test suite (needs `TEST_DATABASE_URL`) |
| `make vet` | `go vet ./...` |
| `make sqlc` | Regenerate `internal/store` from `db/queries` |
| `make seed` | Load sample data |

Edit SQL in `db/queries/*.sql` and the schema in
`internal/database/migrations`, then `make sqlc`.
