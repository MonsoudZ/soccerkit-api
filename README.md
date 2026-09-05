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
| **me** | `GET /me` — the authenticated person + their org memberships, `DELETE /me` (full account erasure), `GET /me/teams` — the teams you are rostered on, with jersey number, position and dates |
| **persons** | `POST /persons` (add an athlete), `GET/PATCH /persons/:id`, `GET /persons/:id/instances`, `GET /persons/:id/aggregate`, `GET/POST /persons/:id/guardians`, `DELETE /persons/:id/guardians/:personId` |
| **teams** | `GET/POST /teams`, `GET/PATCH/DELETE /teams/:id`, `POST /teams/:id/roster`, `DELETE /teams/:id/roster/:personId`, `GET/POST /teams/:id/staff`, `DELETE /teams/:id/staff/:personId`. `GET /teams` is role-scoped: an admin or director sees the organization, a coach the teams they staff, a player the teams they are on, a parent their children's |
| **organizations** | `PATCH /organizations/:id` — rename the club you are acting in. `kind` is not editable: it decides whether account deletion destroys the org or orphans it. `GET /organizations/:id/members`, `PATCH/DELETE /organizations/:id/members/:personId` — who is in the club and what they hold. Joining is by invitation: `GET/POST /organizations/:id/invitations`, `DELETE .../invitations/:invitationId`, and on the invitee's side `GET /me/invitations`, `POST /invitations/:invitationId/accept|decline` |
| **roles** | `admin`, `director`, `coach`, `parent`, `player`, stored one per membership row so a person may hold several. Follows the app's own permission matrix (`Models/Permissions.swift`): **manageOrg** is admin alone, **standardizeTemplates** and **seeEveryTeam** admin/director, **runSessions**/**movePlayers**/**seeSharedLibrary** staff, and a **parent** sees only the children they are a recorded guardian of while a **player** sees only themselves. Granting is capped at your own highest role, you cannot change someone who outranks you, and an org can never be left without an admin. Nobody joins without accepting: an invitation names an address and is matched against the verified Apple address on the invitee's account, so there is no token to hold or forward, and an address with no account yet is invited anyway — the offer waits for their first sign-in. Invitations expire after 14 days |
| **notifications** | `POST /me/devices`, `DELETE /me/devices/:token` — register an APNs device token (idempotent; re-registering moves the token to whoever signed in last). Creating an invitation pushes to the invitee's devices. So does a fixture the squad has to answer: scheduling a game or a team's training session, moving a kickoff, or calling a match off pushes to the active roster and their recorded guardians — minus whoever made the change, and minus anyone with no device registered, because the queue is bounded and a season's fixtures entered in one sitting would otherwise crowd it with deliveries that have nowhere to go. A corrected opponent or a scoreline at full time does not push: a notification per edit teaches a squad to ignore the two that matter. The kickoff instant rides in the payload rather than the text — there is no timezone on a club, so a rendered time would be the server's UTC and the wrong day for half the world. That first push is one shot, so a background sweep chases what it missed: every 15 minutes it takes the fixtures and team training starting within 24 hours that nobody has chased yet, and pushes to whoever still owes an answer — the unanswered players and their guardians, never the ones who already replied. Each fixture is chased exactly once, and the claim is the same statement that selects it, so a service running several instances sends one push per squad rather than one per replica; a rescheduled fixture re-arms, because it was chased at a time that is no longer when it happens. The sweep runs only where `APNS_*` is configured — with push off it would mark a whole fixture list as chased and deliver none of it. Delivery is queued off the request path, so a club's invitation is created whether or not Apple is reachable, and a token Apple reports as `Unregistered` or `BadDeviceToken` is pruned. Set `APNS_*` to enable; unset means push is simply off |
| **evaluation** | `GET/POST /templates`, `GET /templates/:id`, `POST /form-instances`, `GET /form-instances/:id` |
| **content** | `GET/POST /drills`, `GET/POST /sessions`, `GET/PATCH/DELETE /sessions/:id` (sessions carry ordered blocks that can reference drills). `PATCH` moves, renames and re-notes a session — it was the one scheduled thing that could be created and deleted but never edited, so moving Tuesday training to Thursday meant deleting it and rebuilding, which now throws away the register too. A changed time pushes to the squad, the same as a moved kickoff; a rename does not. Blocks are not editable there: replacing the plan is a different operation from editing the session, and a caller sending none would erase one. Changing the team is refused once the register has been answered — those replies are about a specific squad's training and cannot be carried onto another's |
| **game day** | `GET/POST /teams/:id/games`, `GET/PATCH /games/:id` (record kickoff, status, and result); post-game reports attach via a form instance's `contextRef` |
| **attendance** | Who is coming, and who came — the same register for both scheduled things: `GET /games/:id/attendance` and `GET /sessions/:id/attendance` (one line per person, plus the squad's tally), `PUT /games/:id/rsvp` and `PUT /sessions/:id/rsvp` (a player replies for themselves, a parent for a child they are a recorded guardian of, staff for a nine-year-old whose reply arrived by text), `PATCH /games/:id/attendance/:personId` and `PATCH /sessions/:id/attendance/:personId` (staff record `present`/`absent`/`late`/`excused`, or explicit `null` to untick a line). Two vocabularies on purpose: an RSVP is what a family predicted, a status is what the club observed, and one set of words would make "going" mean both. Role-scoped like the roster — the lines are narrowed to what the caller may see while the counts stay the squad's — and a line survives its player leaving the team (`onRoster: false`), because a register is the record of a fixture rather than of the current squad. `GET /teams/:id/attendance` reads the same register down the season instead of across one fixture — one line per rostered player over the team's games and training, filterable by `from`/`to` (inclusive days, UTC) and `type`, with `noShows` (said going, did not turn up, which no single sheet can show) and a `rate` of present+late over present+late+absent. `notRecorded` is reported separately because a squad nobody registered and a squad that all turned up are otherwise the same numbers, and `excused` counts in neither half of the rate: an approved absence is not a player letting the team down. Cancelled matches drop out — nobody attends a match that was called off |
| **iOS sync** | `GET/POST /sync` — opaque `{type,id,payload}` delta-sync for the offline-first app. A **projection over the domain tables**: projected types (`Team`, `Drill`, `Session`) land in their real table (columns projected from the payload, full payload retained); other types round-trip losslessly via a generic `sync_documents` store until they graduate. A shared `seq` sequence is the cursor; rows are scoped per account (Person). **REST writes are written twice** — into the projected columns and into the `payload` a pull returns — so a team, drill or session created or edited by a web client reaches the phone instead of being invisible to it or overwritten by its next push. A record missing a key the app decodes as non-optional loses the *whole* record on the device, so each payload carries exactly what that model requires: a session's `date` is supplied by the server (seconds since 2001, the way Swift encodes a `Date`), while a drill's category and duration are left out rather than invented — the app defaults them, and a coaching choice nobody made is worse in a coach's library than a blank |

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
