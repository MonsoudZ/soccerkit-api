# SoccerKit API — codebase audit

Audit of `monsoudz/soccerkit-api` at `7875847`. Scope: the whole repository —
~4,600 hand-written lines of Go across `internal/api`, `internal/config`,
`internal/database`, `cmd/`, plus the SQL in `db/queries` and the migrations.
`internal/store` is sqlc-generated and was read but not reviewed as authored code.

Every finding marked **confirmed** below was reproduced against a live server
(`httptest` + Postgres, the project's own test harness). The transcripts are
quoted verbatim. Findings without that marker are read-only observations.

Baseline health: `go vet`, `go build` and `go test ./...` all pass clean.

> **Status.** C1, C2, H1–H4 and M1–M7 are fixed in the commits following this report.
> Every reproduction below was re-run against the fixed code and now fails closed. The
> findings are left described in the present tense as a record of what was wrong and
> why. **L1–L7 are still open.**
>
> Three of the fixes changed behaviour deliberately, and each is worth knowing about:
> `POST /persons` replaces `asPlayer` with `role` and always creates a membership;
> `DELETE /teams/{id}` and `DELETE /sessions/{id}` tombstone instead of hard-deleting,
> so the deletion reaches sync clients; and migration `0005` deletes existing refresh
> tokens, so everyone signs in once more.

---

## Summary

| # | Severity | Finding |
|---|----------|---------|
<!-- ✅ = fixed; see Status above -->
| C1 ✅ | **Critical** | Sync push writes across tenants — any row can be overwritten and stolen by id |
| C2 ✅ | **Critical** | `GET /persons/{id}` (+ `/instances`, `/aggregate`) has no authorization — minors' medical PII |
| H1 ✅ | High | `GET /templates/{id}` and `GET /form-instances/{id}` have no authorization |
| H2 ✅ | High | `POST /form-instances` does not scope template or subject to the caller's org |
| H3 ✅ | High | `POST /teams/{id}/roster` accepts any Person id, from any org |
| H4 ✅ | High | `PATCH /games/{id}` type confusion silently nulls fields |
| M1 ✅ | Medium | REST endpoints ignore the sync `deleted` tombstone |
| M2 ✅ | Medium | `DELETE /teams/{id}` hard-deletes with no tombstone |
| M3 ✅ | Medium | Form answers are not validated against their field's kind or config |
| M4 ✅ | Medium | No rate limiting, no request body cap, no sync batch cap |
| M5 ✅ | Medium | Refresh tokens stored in plaintext; no reuse detection |
| M6 ✅ | Medium | Apple sign-in links accounts by email without checking `email_verified` |
| M7 ✅ | Medium | Unknown `kid` triggers a JWKS fetch on every request |
| L1 | Low | CORS `AllowedHeaders` omits `X-Organization-ID` |
| L2 | Low | OpenAPI spec is missing `/auth/apple` and `/sync` |
| L3 | Low | Login timing side channel enables user enumeration |
| L4 | Low | `.env.example`'s secret is not in the `placeholderSecrets` list |
| L5 | Low | Default org selection is non-deterministic |
| L6 | Low | Registration race returns 500 instead of 409 |
| L7 | Low | The "isolation" tests only cover reads |

The two Critical and four High findings share one root cause, described next.

---

## The structural problem

The codebase has two write paths into the same tables, and they disagree about
who is allowed to write.

**The REST path is careful.** `resolveOrg` resolves an org from the caller's
memberships, `requireCoach` checks the role, and `teamInOrg` / `optionalTeamInOrg`
/ `handleGetSession` each re-verify that the row they loaded actually belongs to
the caller's org. That pattern is correct and consistently applied — for teams,
sessions and drills.

**The sync path has no such check**, and **the person/template/instance readers
have none either**. The result is that three of the five "load-bearing seams" in
the README — persons, the evaluation engine, and the org boundary itself — are
enforced only for the resources that happen to have gone through `teamInOrg`.

Concretely, `internal/api/handlers_people.go`, `handlers_forms.go` and the
`Sync*` queries in `db/queries/sync.sql` never compare anything to `oc.orgID` or
to `sync_account_id`. Authorization in this service is a per-handler convention,
not an invariant, and six handlers do not follow it.

---

## Critical

### C1 — Sync push writes across tenants (confirmed)

`db/queries/sync.sql`, all seven `SyncUpsert*` statements.

Every projected upsert is keyed on the client-supplied primary key with no
ownership predicate:

```sql
-- name: SyncUpsertTeam :exec
INSERT INTO teams (id, organization_id, sync_account_id, name, ...)
VALUES ($1, $2, $3, $4, ...)
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name, ...,
    sync_account_id = EXCLUDED.sync_account_id,   -- <-- reassigns the owner
    payload = EXCLUDED.payload, deleted = false, seq = nextval('sync_seq');
```

`ON CONFLICT (id) DO UPDATE` has no `WHERE teams.sync_account_id = $3`. So a
`POST /sync` upsert naming an existing row's id does not create a row — it
rewrites that row, whoever owns it, and sets `sync_account_id` to the caller.
The victim's data is both corrupted and transferred: it now appears in the
attacker's `GET /sync` delta and becomes tombstonable by them, because
`SyncTombstoneTeam`'s `WHERE id = $1 AND sync_account_id = $2` guard is now
satisfied for the attacker.

This is what migration `0002_sync.sql` explicitly promises will not happen:

> Scope: sync rows are owned by the pushing account (a Person). Rows created via
> the REST API have a NULL `sync_account_id` and are invisible to sync — the two
> write paths stay cleanly separated.

The intent is documented; the SQL does not implement it.

Affects `SyncUpsertTeam`, `SyncUpsertDrill`, `SyncUpsertSession`,
`SyncUpsertPerson`, `SyncUpsertPlayer`, `SyncUpsertEvent`, `SyncUpsertDiagram`.
`SyncUpsertDocument` is **not** affected — its conflict target is
`(sync_account_id, type, id)`, which scopes it correctly. That is the shape the
other seven should have.

`SyncUpsertPerson` is the worst of the seven: it rewrites `display_name`,
`emergency_contact_name`, `emergency_contact_phone` and `medical_notes` on any
`persons` row in the database.

**Reproduction.** Two freshly registered coaches in separate personal orgs.
Victim creates a team via REST; attacker pushes an upsert for its id:

```
POST /api/v1/teams              (victim)  -> 201  id 48ef266c-…
POST /api/v1/sync               (attacker)
  {"upserts":[{"type":"Team","id":"48ef266c-…","payload":{"name":"PWNED"}}]}
                                          -> 200 {"cursor":null,"conflicts":[]}

GET  /api/v1/teams              (victim)  -> 200
  [{"id":"48ef266c-…","organizationId":"58ab3bbf-…","name":"PWNED",…}]

GET  /api/v1/sync?since=0       (attacker) -> 200
  {"records":[{"type":"Team","id":"48ef266c-…","payload":{"name":"PWNED"}}],…}
```

The victim's team is renamed in the victim's own org, and now streams to the
attacker's device. No error, no conflict reported.

**Fix.** Add an ownership predicate to the conflict clause of all seven, e.g.

```sql
ON CONFLICT (id) DO UPDATE SET ...
WHERE teams.sync_account_id = EXCLUDED.sync_account_id
```

That makes a push against someone else's row a silent no-op rather than a
takeover. To surface it to the client instead, return the affected row count and
populate the `conflicts` array in `syncPushResponse` — which is currently always
returned empty (`handlers_sync.go:130`), so the wire format already has the slot.
Note the predicate also blocks adopting a REST-created row (`sync_account_id IS
NULL`), which is exactly what migration 0002 says should happen.

### C2 — Person reads have no authorization (confirmed)

`internal/api/handlers_people.go:100-161`.

`handleGetPerson`, `handleListPersonInstances` and `handlePersonAggregate` parse
the path UUID and query it directly. None of them calls `resolveOrg`, and none
compares anything to the caller's org, roster or guardianships:

```go
func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	...
	person, err := s.store.GetPerson(r.Context(), id)   // no scoping whatsoever
	...
	writeJSON(w, http.StatusOK, personDTO(person))
}
```

`personDTO` returns the full record — `birthdate`, `email`, `phone`,
`emergencyContactName`, `emergencyContactPhone`, `medicalNotes`. These are, per
the project's own comment in `handlers_account.go`, "minors' PII we are legally
required to erase (COPPA/GDPR)."

**Reproduction.** Victim coach registers an athlete; an unrelated attacker
account reads it:

```
POST /api/v1/persons  (victim)
  {"displayName":"Kid Athlete","birthdate":"2015-04-01",
   "medicalNotes":"severe peanut allergy","emergencyContactPhone":"+15550001111"}
                                -> 201  id 611d42ff-…

GET /api/v1/persons/611d42ff-…  (attacker, different org) -> 200
  {"displayName":"Kid Athlete","birthdate":"2015-04-01",
   "emergencyContactPhone":"+15550001111",
   "medicalNotes":"severe peanut allergy",…}
```

`/instances` and `/aggregate` behave identically — the athlete's full evaluation
history and score aggregates are readable by any authenticated account.

Exploiting this requires knowing the Person UUID, which is not enumerable. It is
still a missing access-control check on the most sensitive data in the system,
and H3 below hands out exactly those UUIDs.

**Fix.** Introduce a `personVisibleTo(ctx, oc, personID) error` helper — a person
is visible if they share an org membership with the caller, are rostered on a
team in the caller's org, or are the caller's own Person (later, a guardianship).
Call it from all three handlers before responding. `POST /persons` already does
the right thing by scoping the created membership to `oc.orgID`; the readers just
never grew the matching check.

---

## High

### H1 — Template and instance reads have no authorization (confirmed)

`handlers_forms.go:89` (`handleGetTemplate`) and `:342` (`handleGetInstance`)
follow the same pattern as C2: parse id, fetch, return. Neither calls
`resolveOrg`. Any authenticated caller can read any org's custom evaluation
template — including the field set that constitutes the product's differentiator
— and any form instance, i.e. any athlete's scored answers.

Confirmed: an attacker account fetched another org's seeded `pre_game` template
by id and received the full 10-field definition (200 OK).

Note the list endpoint next door is scoped correctly (`ListFormTemplates` filters
on `organization_id`/`author_person_id`), so this is an inconsistency between
list and get, not a missing concept.

### H2 — `POST /form-instances` is not scoped to the caller's org (confirmed)

`handlers_forms.go:225-340`. The handler calls `resolveOrg` and checks the role,
then never uses `oc.orgID` again. It does not verify that:

- the `templateId` belongs to the caller's org,
- the `subjectPersonId` is someone the caller may evaluate,
- the `subjectTeamId` is a team in the caller's org.

So an attacker can write permanent evaluation records about another org's
athletes, and poison the aggregate that `README.md` calls "the moat, concretely":

```
POST /api/v1/form-instances  (attacker)
  {"templateId":"<victim's template>","subjectPersonId":"<victim's person>",
   "answers":[{"key":"sleep","numericValue":999999}]}     -> 201

GET /api/v1/persons/<victim's person>/aggregate?context=pre_game   (victim)
  [{"key":"sleep","label":"Sleep quality","samples":1,
    "average":999999,"minimum":999999,"maximum":999999}]
```

The victim's own readiness dashboard now reads 999999. Combined with M3 (no
range validation) a single request can permanently skew an athlete's trend line.

**Fix.** After loading the template, require
`template.OrganizationID == oc.orgID || template.AuthorPersonID == callerID`, and
run both subject ids through the same `personVisibleTo` / `teamInOrg` checks used
elsewhere.

### H3 — Roster accepts a Person from any org (confirmed)

`handlers_teams.go:97-148`. `handleAddRoster` correctly checks the *team* is in
the caller's org (`teamInOrg`) and correctly checks the person *exists* — but
never checks the person is in the caller's org:

```go
if _, err := s.store.GetPerson(r.Context(), personID); errors.Is(err, pgx.ErrNoRows) {
	writeError(w, errBadRequest("personId does not reference an existing person"))
	return
}
```

Existence is not authorization. Any coach can attach any Person UUID to their own
team, after which `GET /teams/{id}` returns that person through `ListActiveRoster`:

```
POST /api/v1/teams/<attacker team>/roster  (attacker)
  {"personId":"<victim's athlete>"}                       -> 201

GET  /api/v1/teams/<attacker team>         (attacker)     -> 200
  {"roster":[{"personId":"ec2c299f-…","displayName":"Minor Athlete",
              "email":"parent@home.test","birthdate":"2016-01-02",…}]}
```

Two consequences beyond the disclosure:

1. It leaks the Person UUIDs that C2 and H1 need, turning an
   ID-guessing problem into a two-request chain.
2. It quietly defeats account deletion. `SelectOrphanedAthletePersonIDs`
   deliberately spares a Person "still linked to any org OUTSIDE the delete-set"
   — the shared-athlete case. An attacker-created roster row is exactly such a
   link, so when the victim deletes their account the athlete's PII is
   *preserved* rather than erased, breaking the COPPA/GDPR guarantee
   `handlers_account.go` is written to provide.

**Fix.** Require the person to hold a membership in `oc.orgID` (or be rostered on
another team in it) before creating the roster row — `HasMembership` already
exists in `db/queries/identity.sql` and is currently unused.

### H4 — `PATCH /games/{id}` type confusion nulls fields (confirmed)

`handlers_games.go:119-192`. The handler decodes into `map[string]any` and then
uses a two-step "present?" / "right type?" pattern where only the first step
guards the write:

```go
if _, ok := raw["homeAway"]; ok {
	params.SetHomeAway = true                 // set on *presence*
	if v, ok := raw["homeAway"].(string); ok {
		if !validHomeAway[v] { ...400... }
		params.HomeAway = &v                  // assigned only on *type match*
	}
}
```

When the type does not match, `SetHomeAway` is already `true` and `HomeAway` is
still `nil`, so `UpdateGame`'s `CASE WHEN set_home_away THEN narg('home_away')`
writes NULL. The enum validation is skipped entirely — it lives inside the branch
that a wrong type never enters. The same shape affects `opponent` and the
`ourScore`/`opponentScore` pair.

```
PATCH /api/v1/games/{id}  {"ourScore":3,"opponentScore":1}
  -> {"opponent":"Rivals FC","homeAway":"home","ourScore":3,"opponentScore":1}

PATCH /api/v1/games/{id}
  {"opponent":12345,"homeAway":true,"ourScore":"x","opponentScore":"y"}
  -> 200 {"opponent":null,"homeAway":null,"ourScore":null,"opponentScore":null}
```

A malformed client request returns 200 and destroys the recorded result of a
match. There is no undo.

Secondary defect in the same handler: `decodeJSON` sets
`DisallowUnknownFields()`, which is a **no-op when decoding into a map**. So
`PATCH` silently accepts junk fields while every other endpoint rejects them —
`{"totallyUnknownField":1}` returns 200. The strict-decoding guarantee the rest
of the API relies on is absent exactly where the hand-rolled parsing needs it
most.

**Fix.** Decode into a struct of `*json.RawMessage` (or `map[string]json.RawMessage`)
and unmarshal each present key into its typed target, returning 400 on any type
mismatch. Set the `Set*` flag only after a successful typed decode.

---

## Medium

### M1 — REST ignores the sync tombstone (confirmed)

Migrations 0002–0004 add `deleted boolean NOT NULL DEFAULT false` to `teams`,
`drills`, `sessions`, `persons`, `players`, `events` and `diagrams`. Only
`ListSyncChangesSince` reads it. Every REST query — `ListTeamsInOrg`, `GetTeam`,
`ListDrillsInOrg`, `GetDrill`, `ListSessionsInOrg`, `GetSession`, `GetPerson`,
`ListActiveRoster` — omits `AND deleted = false`.

A coach who deletes a team in the iOS app sees it disappear locally, sync
tombstones it, and the REST API keeps serving it:

```
POST /api/v1/sync {"upserts":[{"type":"Team","id":"1111…","payload":{"name":"Synced"}}]}
POST /api/v1/sync {"deletes":[{"type":"Team","id":"1111…"}]}
GET  /api/v1/teams        -> 200 [{"id":"1111…","name":"Synced",…}]   # still there
GET  /api/v1/teams/1111…  -> 200 {"team":{…}}                          # still there
```

Deleted athletes remain visible through `ListActiveRoster` too, which is the same
PII-retention problem as H3 from a different direction.

**Fix.** Add `AND deleted = false` to the REST-facing queries. Worth doing as one
sweep so the two paths can't drift again.

### M2 — `DELETE /teams/{id}` leaves no tombstone

`DeleteTeam` is `DELETE FROM teams WHERE id = $1` — a hard delete. Sync clients
learn about deletions only from `deleted = true` rows in `ListSyncChangesSince`,
and a row that no longer exists produces no delta. A client holding the team will
never be told it is gone, and its next push will resurrect it via
`SyncUpsertTeam`'s insert branch. The hard delete also cascades away
`roster_memberships`, `games` and `form_instances.subject_team_id` with no record.

This is the mirror of M1: sync deletes are invisible to REST, REST deletes are
invisible to sync. Making `DELETE /teams/{id}` set the tombstone (and bump `seq`)
for sync-owned rows resolves both directions.

### M3 — Answers are not validated against their field (confirmed)

`handleSubmitInstance` looks a field up by key and then writes whatever value
arrived, without consulting `field.Kind` or `field.Config`:

```go
field, ok := fieldByKey[a.Key]
if !ok { ...400... }
q.CreateFormAnswer(..., NumericValue: a.NumericValue, BoolValue: a.BoolValue, TextValue: a.TextValue)
```

Three distinct defects, all confirmed in one request:

```
POST /api/v1/form-instances
  {"answers":[{"key":"sleep","numericValue":-4200.5},
              {"key":"warmed_up","numericValue":77,"textValue":"not a bool"},
              {"key":"sleep","textValue":"duplicate key, no numeric"}]}   -> 201

GET /api/v1/persons/{id}/aggregate
  [{"key":"warmed_up","label":"Warmed up","samples":1,"average":77,…}]
```

1. **Kind is not enforced.** `warmed_up` is a `bool` field; it accepted
   `numericValue: 77` *and* a `textValue`, and 77 now appears in the score
   aggregate. A boolean is being averaged.
2. **Config range is not enforced.** `sleep` is declared `{"min":1,"max":5}` in
   its seeded config; `-4200.5` was accepted.
3. **Duplicate keys silently destroy data.** `CreateFormAnswer` is
   `ON CONFLICT (instance_id, field_id) DO UPDATE`, so the second `sleep` entry
   overwrote the first with `numeric_value = NULL`. `sleep` vanished from the
   aggregate entirely — the API returned 201 and echoed both answers as if both
   were stored.

The aggregate query is described in the README as the product's analytical core.
It currently averages unvalidated client input.

**Fix.** Switch on `field.Kind` and require the matching value column (rejecting
the others); range-check `scale` against its config; reject duplicate keys in the
request with a 400 rather than letting the upsert eat one.

### M4 — No rate limiting, no body cap, no batch cap

- `/auth/login` and `/auth/register` have no throttling. bcrypt at
  `DefaultCost` makes login a ~60ms server-side operation, so this is a cheap
  resource-exhaustion lever as well as an unmetered credential-stuffing target.
- No `http.MaxBytesReader` anywhere. `decodeJSON` will read a body of any size.
- `handleSyncPush` iterates `req.Upserts` with no cap, inside one transaction —
  an arbitrarily large batch holds a Postgres transaction open for as long as it
  takes.

`middleware.Timeout(30 * time.Second)` is mounted, which bounds the damage but
does not prevent it. Adding `httprate` on the auth routes, a global
`MaxBytesReader`, and a batch limit in `handleSyncPush` are three small,
independent changes.

### M5 — Refresh tokens are stored in plaintext, with no reuse detection

`refresh_tokens.token` holds the token verbatim, and `GetRefreshToken` looks it
up by that value. Anyone with read access to the database — a backup, a log, a
SQL-injection elsewhere, an over-broad support query — gets working credentials
for every account. Storing a SHA-256 of the token and looking up by hash costs
nothing and removes the whole class.

Rotation itself works correctly (confirmed: replaying a rotated token returns
401). What is missing is *reuse detection*: a replay of an already-rotated token
is a strong signal of theft, but the legitimate chain stays valid afterwards
(confirmed). Standard practice is to revoke the entire token family on reuse.

Related, lower stakes: `/auth/logout` is mounted outside `requireAuth` and revokes
purely on possession of the token string. That is defensible for a logout
endpoint, but it means anyone who observes a refresh token can revoke sessions.

### M6 — Apple sign-in links by email without checking `email_verified`

`handleAppleAuth` case 2 links a new Apple identity to an existing
password account whenever the emails match:

```go
existing, err := q.GetUserAccountByEmail(ctx, email)
case err == nil:
	q.LinkAppleSub(ctx, ...)   // takeover of the existing account
```

`identityFromClaims` extracts only `sub` and `email`; it ignores Apple's
`email_verified` claim. Apple normally only issues verified addresses, so this is
a hardening gap rather than a live exploit — but "trust an IdP's unverified email
to merge accounts" is a well-known takeover primitive and the claim is right there
in the token. Parse it and refuse to link when it is false.

### M7 — Unknown `kid` triggers a JWKS fetch per request

`apple.go:93` — `keyForKID` calls `refreshKeys` whenever the requested key id is
not in the cache. The cache is only populated with keys Apple returns, so a token
carrying an arbitrary `kid` misses every time and forces an outbound HTTPS request
to `appleid.apple.com`. Unauthenticated callers control that header, so
`POST /auth/apple` is an outbound-request amplifier: N requests to the service
become N requests to Apple, each held open for up to 10s.

Add a negative-result cooldown (refresh at most once per interval regardless of
outcome) so a miss cannot drive a fetch.

---

## Low

**L1 — CORS blocks the multi-tenancy header.** `server.go:51` allows only
`Authorization` and `Content-Type`. `X-Organization-ID` — the documented way to
act in a specific org — is not in the list, so any browser client is unable to
send it; preflight will reject. The iOS app is unaffected. Also
`CORS_ORIGINS` defaults to `*`, which `validateDeployed` does not cover even
though it covers the other deployment footguns.

**L2 — OpenAPI spec is missing two shipped endpoints.** `internal/api/openapi.yaml`
documents 23 paths but not `POST /api/v1/auth/apple` or `GET`/`POST /api/v1/sync`.
Both are in the README's endpoint table, and both are the *primary* iOS surfaces.
Since the spec is stated to be the source for the generated Swift client, the two
features the client needs most cannot be code-generated. (`DELETE /me` is
documented correctly.)

**L3 — Login timing enables user enumeration.** `handleLogin` returns as soon as
`GetUserAccountByEmail` misses, skipping bcrypt entirely; a hit spends ~60ms
comparing. The difference is trivially measurable. Compare against a fixed dummy
hash on the miss path.

**L4 — `.env.example`'s secret is not in the placeholder list.**
`placeholderSecrets` in `config.go` catches `dev-access-secret` but the file ships
`dev-access-secret-change-me`, which is not a member. It is caught anyway by the
32-byte minimum (it is 26), so the guard holds — but by accident rather than by
the check that was written for it. Add the literal.

**L5 — Default org selection is non-deterministic.** `resolveOrg` falls back to
`memberships[0]`, ordered by `o.created_at ASC`. Orgs created inside one
transaction share a `now()` timestamp, so for a person in several such orgs the
"default" org is whatever Postgres returns first. Tie-break on `o.id` for
stability.

**L6 — Registration race returns 500.** `handleRegister` checks for an existing
email and then inserts; two concurrent requests both pass the check and one hits
the unique constraint, surfacing as a 500. The sequential path correctly returns
409 (confirmed). `isUniqueViolation` already exists in `respond.go` and is used by
`handleAddRoster` — wrap the insert with it here too.

**L7 — The isolation tests only cover reads.** `TestTeamIsolatedByOrg` asserts a
cross-org `GET` and `DELETE` are 403. `TestSyncIsolatedPerAccount` asserts Bob
cannot *read* Alice's records. Neither tests a cross-tenant *write*, which is why
C1 has been able to sit in a suite that passes. Adding the write-direction case to
both is the single highest-value test change in the repo.

---

## What is done well

Worth stating plainly, because these are the parts that should not be disturbed
while fixing the above.

- **`config.validateDeployed`** is genuinely good defensive design: it fails
  closed (`IsDeployed` treats an unrecognised `ENV` as deployed), it names the
  consequence in each error message, and it is well covered by
  `config_test.go`. The `DEV_APPLE_BYPASS` guard in particular closes what would
  otherwise be a total auth bypass.
- **No SQL injection surface.** Every query goes through sqlc-generated
  parameterised code, and CI verifies the generated output is in sync with
  `db/queries` (`git diff --exit-code -- internal/store`).
- **Migrations are transactional and idempotent**, tracked in
  `schema_migrations`, applied in lexical-numeric order, embedded in the binary.
- **`handleDeleteMe`** is the most carefully reasoned code in the repo. The
  three-phase cascade, the orphaned-athlete query with its multi-org guard, and
  the idempotency argument are all correct and well documented. H3 undermines it
  from outside, but the handler itself is sound.
- **Token design** — access tokens carry only `sub`, with org and role resolved
  per request from memberships, so a role change takes effect immediately rather
  than at the next login. That is the right trade.
- **The schema** delivers what the README claims: time-bounded rosters, persons
  separated from accounts, and a genuinely generic evaluation engine.
- **Deployment hygiene** — distroless runtime image, `USER nonroot`,
  `CGO_ENABLED=0`, `ReadHeaderTimeout` set, graceful shutdown wired to
  SIGINT/SIGTERM.

---

## Suggested order of work

1. **C1** — one `WHERE` clause on each of seven upserts. Smallest diff, largest
   risk reduction. Add the cross-tenant write test (L7) in the same change.
2. **C2, H1, H2, H3** — one `personVisibleTo` helper plus org checks in five
   handlers. These are the same bug four times; fixing them together keeps the
   pattern consistent.
3. **H4** — rewrite the `PATCH /games` decode. Self-contained.
4. **M1 + M2** — reconcile the tombstone in both directions as one change.
5. **M3** — answer validation.
6. Everything else as ordinary hardening.

Items 1–3 are what stand between this codebase and a multi-tenant deployment.
