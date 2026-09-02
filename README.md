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
club library), and the per-role surfaces that roles and invitations now give a
place to hang from.

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
`POST /members` only changes what someone **already in the org** may do.

## Invitations

Getting *into* somebody else's organization is the invitation flow. It has to
exist, because every other route in is wrong: `POST /persons` makes a Person with
no login, `POST /members` refuses an id it has no consent for, and Sign in with
Apple always lands a person in their **own** personal org — nowhere near the club
their child plays for.

```
POST /api/v1/invitations   { role, email?, note?, childPersonIds? }
  → 201 { …invitation, token: "skinv_…" }     ← the only time the token is returned

# the invitee signs in with Apple on their own, then:
POST /api/v1/invitations/preview { token }    → the club, the role, the children
POST /api/v1/invitations/accept  { token }    → membership + guardianships + their access
GET  /api/v1/invitations                      → what we sent and what came of it
DELETE /api/v1/invitations/{id}               → kill an outstanding link
```

The token is a credential and is treated like one: 32 bytes of entropy, stored
only as a SHA-256 (like refresh tokens), never returned again, `skinv_`-prefixed
so a scanner or a bug report can spot one, and carried in a request **body** —
preview is a POST because a token in a URL ends up in the request log. It expires in 14 days and is
**single-use by the write** — acceptance is a conditional `UPDATE`, so two
devices opening the same forwarded link produce one membership and one 409,
rather than two of everything.

The membership goes to **the account that redeemed it**, never to an id in the
request. That is the whole security model: the club says who it is inviting, the
invitee proves it is them by holding a token only they were sent and signing in
themselves.

- **Ceiling.** An invitation may never reach further than a direct grant, or
  "invite yourself as admin, then accept" is a one-step takeover. Admin and
  director invite exactly as far as they may grant; a **coach** — who staffs
  nobody — is capped strictly below their own rank, which is the parents and
  players of their own athletes, the invitations only they are positioned to send.
- **Children.** `childPersonIds` on a parent invitation is what makes the parent
  tier work: a parent membership with no guardianship sees nothing. The coach who
  knows which child this is names them; redemption writes the link.
- **Email binding** is optional on purpose. It turns a leaked link into a dead
  one — but Apple's Hide My Email means an invitee who hides their address signs
  in with a relay that will never match what the club typed, and a bound
  invitation would lock out exactly the person it was for.

One consequence worth knowing: your **default organization** (the one used when
you send no `X-Organization-ID`) is the org you *own*, not the oldest one you
belong to. Ordering was fine while nobody could be in two orgs; once you can
accept an invitation, a club founded before you signed up would otherwise become
the org every unheadered request silently acted in.

**Still open:** an invitation links an account to a club, it does not merge it
with a Person record the club already created. A teenager whose athlete record
already holds their evaluations gets a `player` membership on their *own* Person,
not that one — closing that needs either a Person merge or an identity alias, and
both are bigger decisions than this flow.

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
    handlers_*.go          # auth · people · teams · forms (the engine) · members · invitations
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
