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
| **auth** | `POST /auth/apple` — Sign in with Apple, the only way into an account; a first sign-in provisions Person + UserAccount + personal Org + admin/director/coach memberships + seeded templates, and it returns `{ token, refreshToken, personID }` for the iOS app. `/auth/refresh` (rotating), `/auth/logout`. There is no registration or password login: nothing shipped used them, and they let anyone create an account at an address nobody verified (see `docs/AUDIT-3.md`). |
| **me** | `GET /me` — the authenticated person + their org memberships, `DELETE /me` (full account erasure) |
| **persons** | `POST /persons` (add an athlete), `GET /persons/:id`, `GET /persons/:id/instances`, `GET /persons/:id/aggregate` |
| **teams** | `GET/POST /teams`, `GET/DELETE /teams/:id`, `POST /teams/:id/roster`, `DELETE /teams/:id/roster/:personId` |
| **evaluation** | `GET/POST /templates`, `GET /templates/:id`, `POST /form-instances`, `GET /form-instances/:id` |
| **content** | `GET/POST /drills`, `GET/POST /sessions`, `GET/DELETE /sessions/:id` (sessions carry ordered blocks that can reference drills) |
| **game day** | `GET/POST /teams/:id/games`, `GET/PATCH /games/:id` (record kickoff, status, and result); post-game reports attach via a form instance's `contextRef` |
| **iOS sync** | `GET/POST /sync` — opaque `{type,id,payload}` delta-sync for the offline-first app. A **projection over the domain tables**: projected types (`Team`, `Drill`, `Session`) land in their real table (columns projected from the payload, full payload retained); other types round-trip losslessly via a generic `sync_documents` store until they graduate. A shared `seq` sequence is the cursor; rows are scoped per account (Person) |

**Next up (schema already present):** `ShareGrant` scopes (coach-to-coach +
club library), an invitation flow so a signed-in account can join someone else's
club, and the per-role surfaces the roles below now have a place to hang from.

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
org (the solo-coach case).

## Roles & capabilities

`internal/authz` is the permission model: **roles are sets of capabilities**, and
handlers ask *"can you do this?"* rather than *"who are you?"*. A capability is a
product verb (`roster.manage`, `evaluation.submit`, `member.grant`), not an
endpoint, so adding a route does not mean inventing a new rule — and adding a
role is a row in one table instead of a hunt through twenty `if` statements.

A person holds roles **per organization**, and often several at once: signing up
as a solo coach creates a personal org with admin + director + coach, and the
same human is a parent at their child's club. Permissions are the union.

| Role | Runs | Cannot |
|------|------|--------|
| **admin** | Everything, including who else holds which role | — |
| **director** | The sporting side: staffing, teams across age groups, club reporting, coach reviews | Delete the org; appoint an admin |
| **coach** | Their teams: roster, sessions, game day, athlete evaluation | Staff the club; review coaches; org settings |
| **parent** | Their own children: schedule, evaluations, the forms asked of a parent | See any other family, any staff surface |
| **player** | Themselves: their schedule, self-assessments, history | Anything about anyone else |

Two rules do the work, and both are load-bearing:

- **Capability** — *may you do this at all?* (`authz.Set.Can`)
- **Scope** — *to whose rows?* (`authz.Set.Scope`) — `org` for staff, `own` for a
  parent or player. A parent and a coach both hold `person.read`; without scope
  a parent would read every other family's minor's medical notes.

The client does not need a copy of any of this:

```
GET /api/v1/roles       → the whole matrix: every role, label, rank, capabilities
GET /api/v1/me/access   → your roles, capabilities, scope and grantable roles here
GET /api/v1/members     → who belongs to this org and as what
POST /api/v1/members    → { personId, role }   (member.grant, capped by your own rank)
DELETE /api/v1/members/{personId}/roles/{role}
POST /api/v1/persons/{id}/guardians  → link a parent to a child (the parent tier's join)
GET /api/v1/me/children → your own household
```

Guards worth knowing: a director cannot grant or revoke `admin` (the rank
ceiling — otherwise `member.grant` is a ladder to the top of the org), an
organization cannot lose its last admin (nobody left could grant one back), and
`POST /members` only changes what someone **already in the org** may do. Bringing
an account that signed in on its own into somebody else's club is an
**invitation flow, and there is not one yet** — it has to be tied to Sign in with
Apple rather than to an address someone typed. That is the next piece of the
parent tier.

## Quick start

```bash
export ENV="development"    # required, and it has no default: it selects whether the
                            # development-only escape hatches below are available
export DATABASE_URL="postgresql://postgres:postgres@localhost:5432/soccerkit?sslmode=disable"
export JWT_ACCESS_SECRET="dev-access-secret"
export DEV_APPLE_BYPASS=true   # or set APPLE_CLIENT_ID to the app's bundle id

make run                    # migrations apply on boot; docs at /docs
make seed                   # a coach who signs in with Apple sub "dev-coach"
make test                   # needs TEST_DATABASE_URL
```

Interactive docs at `/docs`; raw spec at `/openapi.yaml`.

## Project layout

```
cmd/
  api/main.go              # entrypoint: config, migrate, serve, graceful shutdown
  seed/main.go             # sample coach/team/roster/evaluations
internal/
  authz/                   # roles, capabilities, the permission matrix (pure, no DB)
  config/                  # env configuration
  database/
    database.go            # pgx pool + embedded migration runner
    migrations/*.sql        # the whole-castle schema (source of truth for sqlc)
  store/                   # sqlc-generated queries & models (DO NOT EDIT)
  api/
    server.go              # chi router, middleware, route mounting
    auth.go                # JWT, auth middleware, org/role resolution, capability checks
    dto.go                 # API response types + mapping from store models
    handlers_*.go          # auth · people · teams · forms (the engine) · members (roles)
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
