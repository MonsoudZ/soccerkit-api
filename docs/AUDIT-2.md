# SoccerKit API — second audit

Audit of `monsoudz/soccerkit-api` at `aa924d4`, the tip of the remediation work that
followed [`docs/AUDIT.md`](AUDIT.md). Two questions: did the twenty findings from that
report actually get fixed, and what is wrong now that was not looked at then.

Every finding marked **confirmed** was reproduced against a live server (`httptest` +
Postgres, the project's own harness). Transcripts are quoted verbatim. The repro tests
were run and then removed; the tree is unmodified.

Baseline health: `go vet`, `go build` and `go test ./...` all pass clean.

> **Status.** C1, C2 and H1 — everything in this report reachable today by a single
> account — are fixed in the commits following it, and so is P1, the latent finding whose
> cost rises once clubs ship. Each reproduction was re-run against the fixed code and now
> fails closed. They are left described in the present tense as a record of what was
> wrong and why. P2–P4 and the Mediums and Lows are open.
>
> Two of the fixes change behaviour deliberately:
>
> - **`ENV` is now required and has no default**, so a process that does not set it
>   refuses to boot. `docker-compose.yml`, `.env.example` and the test harness already
>   name it; a deploy config that does not will fail at startup with an error saying so,
>   which is the point.
> - **Apple sign-in now refuses to provision onto a `persons` row it did not create**,
>   returning 409 instead of 200. No legitimate flow reaches that branch, but a
>   pre-claimed id now denies sign-in for that Apple ID until the row is removed — which
>   is the correct trade against silently handing the user an account on someone else's
>   row, and is logged so it is diagnosable. Any row planted before this fix is still
>   there: see the note at the end of C2.
>
> P1 adds `organizations.owner_person_id`, backfilled from the admin membership. It is a
> new column rather than a behaviour change — account deletion selects the same set of
> orgs it does today — but it is the column the invite endpoint will need, and adding it
> after that endpoint ships means adding it during an incident.

---

## Round one: all twenty hold

Re-read against the current code, every fix from the first audit is real and in place.

| Was | Now |
|---|---|
| C1 sync cross-tenant writes | All seven upserts carry `WHERE <table>.sync_account_id = EXCLUDED.sync_account_id`, are `:execrows`, and a zero-row result is surfaced in `conflicts` |
| C2 unauthorized person reads | `visiblePersonFromPath` → `personVisibleTo` → `PersonVisibleInOrg` on all three readers |
| H1 template/instance reads | `templateFor`; instances gated on their *subject*, which is the right axis |
| H2 unscoped instance writes | Template, subject person and subject team all checked against `oc.orgID` |
| H3 roster accepts any person | `personVisibleTo` before `AddRosterMembership` |
| H4 `PATCH /games` type confusion | Per-key typed decode; `Set*` flags only after a successful decode; unknown keys rejected explicitly since `DisallowUnknownFields` is a no-op on a map |
| M1 REST ignores tombstones | `AND deleted = false` across the REST-facing queries |
| M2 hard `DELETE /teams` | `DeleteTeam`/`DeleteSession` tombstone and bump `sync_seq` |
| M3 unvalidated answers | `validateAnswer` + duplicate-key rejection |
| M4 no limits | `limitBody`, `maxSyncBatch`, per-IP token bucket on `/auth` |
| M5 plaintext refresh tokens | SHA-256 at rest, family revocation on replay, 30s retry grace |
| M6 Apple links unverified email | `email_verified` required before linking |
| M7 JWKS fetch per unknown kid | `attemptedAt` cooldown, stamped before the request |
| L1–L7 | CORS header, OpenAPI paths, dummy-hash timing, placeholder literal, org tie-break, 409 on the registration race, write-direction isolation tests — all present |

The added tests are good ones: `TestSyncPushCannotWriteAnotherAccountsRow`,
`TestSyncCannotAdoptRESTCreatedRow`, `TestForwardingHeadersCannotChooseTheBucket` and
`TestReplayedRefreshTokenRevokesTheFamily` each pin the exact behaviour that was wrong.

One loose thread, not worth its own finding: `GetPerson` is still
`SELECT * FROM persons WHERE id = $1` with no `deleted = false`. Every caller reaches it
through `personVisibleTo`, which does check the tombstone — except on the
`personID == caller` short-circuit, so a person tombstoned through sync can still read
themselves back. Harmless today; it is the last query in the REST set that does not
carry the predicate.

---

## Summary of new findings

| # | Severity | Finding |
|---|----------|---------|
| C1 ✅ | **Critical** | `DELETE /me` fails permanently once the account has filed one evaluation |
| C2 ✅ | **Critical** | Sign in with Apple adopts a `persons` row another account already owns |
| H1 ✅ | High | `ENV` defaults to `development`, so every deployed-only guard is opt-in |
| M1 | Medium | `GET /sync` has no page limit |
| L1 | Low | `/docs` loads Swagger UI from unpkg, unpinned and without SRI |
| L2 | Low | `select` answers are not checked against the field's options |
| L3 | Low | `PATCH /games/{id}` cannot clear `kickoffAt` |
| L4 | Low | Revoked and expired refresh tokens are never reaped |
| L5 | Low | The sync `conflicts` array is a cross-tenant existence oracle |
| L6 | Low | `sync_documents.type` is an unbounded, unvalidated namespace |

And separately, four findings that share one precondition:

| # | Severity | Finding |
|---|----------|---------|
| P1 ✅ | **Critical**\* | `DELETE /me` deletes every personal org the caller is *a member of* |
| P2 | High\* | `POST /sync` writes into the org with no role check, bypassing `requireCoach` |
| P3 | Medium\* | Sync-owned rows in a shared org are hard-deleted by FK cascade |
| P4 | Medium\* | Evaluations about a shared athlete are readable by every org they belong to |

\* Not reachable through today's API. See "The precondition" below.

---

## Critical

### C1 ✅ — `DELETE /me` fails permanently once the account has filed one evaluation (confirmed)

`internal/api/handlers_account.go` is correct. The schema underneath it is not.

`form_templates.organization_id` is `ON DELETE CASCADE`, so deleting the caller's
personal org drops its templates. `form_instances.template_id` is a plain
`REFERENCES form_templates (id)` — no action, i.e. `RESTRICT`. So any form instance that
outlives the org deletion blocks it:

```
ERROR: update or delete on table "form_templates" violates foreign key constraint
       "form_instances_template_id_fkey" on table "form_instances" (SQLSTATE 23503)
```

Two ordinary cases produce exactly such an instance:

1. **The subject is the caller.** `handleDeleteMe` deletes the orgs in phase 2 and the
   caller's own Person in phase 3, so an instance about the caller is still present when
   the templates go. A coach's self-review does this; so does a player submitting their
   own pre-game check-in, which is the product's core loop.
2. **The subject is a shared athlete.** `SelectOrphanedAthletePersonIDs` deliberately
   spares an athlete still linked to an org outside the delete-set — that is the
   documented, tested behaviour (`TestDeleteMeSparesSharedAthlete`) — and the spared
   Person keeps their `form_instances` rows alive.

The whole handler runs in one transaction, so the failure is total: nothing is erased,
the response is a bare 500, and the caller's next attempt fails identically. There is no
sequence of API calls that gets the account deleted.

**Reproduction.** One account, three requests, no second party:

```
POST /api/v1/auth/register                      -> 201  person 9639bfdd-…
GET  /api/v1/templates?context=pre_game         -> 200  template 2593ce61-…
POST /api/v1/form-instances
  {"templateId":"2593ce61-…","subjectPersonId":"9639bfdd-…",
   "answers":[{"key":"sleep","numericValue":4}]}
                                                -> 201

DELETE /api/v1/me                               -> 500
  {"error":{"code":"INTERNAL","message":"An unexpected error occurred."}}
GET    /api/v1/me                               -> 200   # account fully intact
```

The shared-athlete variant behaves the same way, and it defeats the exact test that
covers it: `TestDeleteMeSparesSharedAthlete` shares an athlete but never files an
evaluation about them, so the suite passes while the path is broken.

This is the App Store account-deletion requirement the handler's doc comment is written
around, and the GDPR/COPPA erasure the first audit called "the most carefully reasoned
code in the repo". The reasoning is still right. The foreign key is wrong.

**Fix (applied).** Migration `0006_form_instances_cascade.sql` moves
`form_instances.template_id` to `ON DELETE CASCADE`. That matches what the rest of the
engine already does — `form_fields` cascade from the template and `form_answers` cascade
from the field, so a deleted template destroys every answer regardless, and keeping the
instance would leave a husk with no fields and a dangling template id. It was the only
foreign key in the schema with no `ON DELETE` clause at all, which is what made it an
omission rather than a decision.

`TestDeleteMeSparesSharedAthlete` now files an evaluation about the shared athlete before
the deletion, and `TestDeleteMeAfterSelfEvaluation` covers the single-account path with
nothing written directly to the database. Both were confirmed to fail against the old
constraint (500, account intact) and pass against the new one; the second also asserts
the erasure actually happened, since a 204 over a rolled-back transaction would be worse
than the 500 it replaced.

### C2 ✅ — Apple sign-in adopts a `persons` row another account already owns (confirmed)

`handlers_apple.go:157` provisions the coach's Person through `CreatePersonWithID`:

```sql
INSERT INTO persons (id, display_name, email) VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET display_name = EXCLUDED.display_name, updated_at = now()
RETURNING *;
```

The id is `derivePersonID(sub)` = `UUIDv5(coachPersonNamespace, apple_sub)`, and
`coachPersonNamespace` is a hard-coded constant in this repo, shared verbatim with the
client. So the id is a pure function of the Apple subject, computable by anyone.

The upsert adopts whatever row is already there. It does not check ownership, and in
particular it does not clear `sync_account_id`. Meanwhile `POST /sync` lets any
authenticated account insert a `persons` row at an id of its choosing — the ownership
guard on `SyncUpsertPerson` only governs *conflicts*, and there is no conflict when the
row does not exist yet.

So an attacker who learns a victim's Apple sub before the victim's first sign-in can
pre-create the row and own the victim's account Person forever after.

**Reproduction.**

```
POST /api/v1/auth/register                (attacker) -> 201  person d04aadb9-…
POST /api/v1/sync                         (attacker)
  {"upserts":[{"type":"Person","id":"bf5d837a-…",       # = UUIDv5(ns, victim's sub)
    "payload":{"name":"pre-claimed","medicalNotes":"attacker text"}}]}
                                                      -> 200 {"conflicts":[]}

POST /api/v1/auth/apple                   (victim)    -> 200
  {"token":"…","personID":"bf5d837a-…"}

  persons row bf5d837a-…:  display_name="victim"  sync_account_id=d04aadb9-…
                                                   ^ the attacker

POST /api/v1/sync                         (attacker)
  {"upserts":[{"type":"Person","id":"bf5d837a-…","payload":{"name":"OWNED"}}]}
                                                      -> 200
  persons row bf5d837a-…:  display_name="OWNED"
```

The victim signs in successfully and notices nothing. From then on the attacker can
rewrite the victim's `display_name`, `emergency_contact_name`,
`emergency_contact_phone` and `medical_notes` at will, can tombstone the row
(`{"deletes":[{"type":"Person","id":"bf5d837a-…"}]}` → `persons.deleted = true`, which
hides the victim from `PersonVisibleInOrg` and so from their own club's rosters), and
receives the row in their own `GET /sync` delta:

```
GET /api/v1/sync?since=0   (attacker) -> 200
  {"records":[],"deletes":[{"type":"Person","id":"bf5d837a-…"}],"cursor":"2"}
```

**Precondition, stated honestly.** The attacker needs the victim's Apple `sub` — an
opaque per-app identifier that is not published — and has to act before the victim's
first sign-in. That is a real constraint and it is what keeps this out of "trivially
exploitable". It is not a control: the namespace is public, the derivation is documented
in `0003_person_sync.sql`, and a sub leaks through any client-side log, crash report,
support ticket or shared device.

**Fix (applied).** `CreatePersonWithID` is now `ON CONFLICT (id) DO NOTHING RETURNING *`,
and `provisionAppleIdentity` treats the resulting `pgx.ErrNoRows` as a refusal: it logs
the derived id as a security event and returns 409. The transaction rolls back whole, so
no account, organization or membership is built onto the squatted row, and the row itself
is not written to — the old `DO UPDATE` rewrote `display_name` on its way to adopting it.

Refusing outright rather than trying to qualify the existing row (unclaimed? no
`user_accounts` reference?) is the simpler and stricter reading, and nothing legitimate
loses by it: authenticating as this coach requires `/auth/apple`, so their own sync push
cannot precede their own provisioning, and provisioning is one transaction that commits
whole or not at all. There is no benign way for that row to exist. The reconciliation
`0003_person_sync.sql` describes runs in the other direction — the app pushes the id the
server already minted, `SyncUpsertPerson` adopts it — and is unaffected;
`TestPersonSyncReconcilesCoachIdentity` still passes.

The cost is a denial of service: a pre-claimed id now blocks that Apple ID's sign-in
until the row is deleted. That is the right side of the trade — a loud, diagnosable
failure beats a silent takeover — and it carries the same precondition as the attack.

`TestAppleAuthRefusesAPreClaimedPersonID` covers it end to end, including that the
attacker's row is left untouched and that sign-in succeeds once the row is removed. It
was confirmed to fail against the old statement (200, account provisioned onto the
attacker's row).

**Not covered by the fix.** A deployment that has been running the old code may already
have poisoned rows, and nothing distinguishes one from a legitimate Person after the
fact. Worth a one-off query before deploying: `persons` rows whose `sync_account_id` is
some *other* person and which a `user_accounts` row also points at should not exist.

---

## High

### H1 ✅ — `ENV` defaults to `development`, so every deployed-only guard is opt-in (confirmed)

`config.IsDeployed` documents itself as fail-closed:

> It fails closed: anything that is not explicitly development or test counts as
> deployed, so a typo'd or unset-in-CI ENV cannot silently unlock the development-only
> escape hatches below.

The predicate is fail-closed. The value it reads is not:

```go
Env: getenv("ENV", "development"),
```

An unset `ENV` is not "unrecognised" — it is `development`, the most permissive value in
the set. A deployment that never sets the variable gets every escape hatch:

```
ENV unset, JWT_ACCESS_SECRET=secret, DEV_APPLE_BYPASS=true
  -> config.Load() succeeds
     Env="development"  IsDeployed=false  DevAppleBypass=true  secretLen=6
     rate limiter mounted: false
```

`secret` is in `placeholderSecrets` and is six bytes; `DEV_APPLE_BYPASS` skips Apple's
signature, issuer, audience and expiry checks entirely, which is a complete
authentication bypass; and the credential endpoints are unthrottled. All three guards
exist, are correct, and never run.

`.env.example` — the file people copy — ships `ENV=development` and
`DEV_APPLE_BYPASS=true`, which makes the omission the natural mistake rather than an
unlikely one.

**Fix (applied).** `ENV` is required and has no default: `config.Load` returns an error
naming it, and saying what it selects, when it is unset. Defaulting to `production`
instead would also close the hole, but requiring it puts the choice where it is made —
and the compose file, `.env.example` and the test harness all named it already, so the
only configurations this breaks are the ones that were relying on the accident.
`TestUnsetEnvRefusesToBoot` replaces `TestUnsetEnvIsDevelopment`, which asserted exactly
the behaviour that was wrong; `TestTestEnvIsNotDeployed` pins the harness's own case. The
README quick start and `.env.example` were updated to match.

Still open in the same area: `CORS_ORIGINS=*` in a deployed environment is only a log
line, not a boot failure.

---

## The precondition

Four further findings all become live at the same moment, and it is worth stating why
they are not live now.

**Nothing in the current API can put two accounts in one organization.**
`CreateMembership` has exactly three call sites: `handleRegister` and
`provisionAppleIdentity`, which create the caller's own new personal org, and
`handleCreatePerson`, which creates a *brand-new* Person and puts that Person in the
caller's org. There is no invite, no join, no "add an existing coach". So today every
organization has exactly one account-holding member, and the org boundary is enforced as
much by that absence as by any check.

That is the next feature — clubs, directors and assistant coaches are seam 1 of the
schema and the whole point of `organizations`. The four findings below are what the
codebase does the day it lands, and each was reproduced by inserting the membership row
that a future invite endpoint will insert.

### P1 ✅ — `DELETE /me` deletes every personal org the caller belongs to (confirmed)

`ListPersonalOrgIDsForPerson` selects orgs by membership and `kind = 'personal'`, on this
stated reasoning:

> A personal org is created with its owner as sole member (see handleRegister), so
> "member of a personal org" == "owns it".

Nothing enforces that. `organizations` has no owner column, and membership is the only
link. So a second member of someone's personal org deletes that org — with its teams,
drills, sessions, templates, games, rosters and orphaned athletes — by deleting their own
account:

```
POST   /api/v1/auth/register     (coach A)   -> 201, org 4f04caa1-…
POST   /api/v1/auth/register     (player B)  -> 201
  [B gains a 'player' membership in A's org]

DELETE /api/v1/me                (player B)  -> 204
GET    /api/v1/teams             (coach A)   -> 403
  {"error":{"code":"FORBIDDEN","message":"you do not belong to any organization"}}
```

Coach A's club no longer exists. This is the most destructive thing in the report: it is
irreversible, it is triggered by a routine action taken in good faith, and the actor has
no indication anything unusual happened.

**Fix (applied).** Migration `0007_organization_owner.sql` adds
`organizations.owner_person_id`, set at creation by both provisioning paths and
backfilled from the admin membership — which is what `handleRegister` and
`provisionAppleIdentity` give the creator, and is unambiguous today because every
existing personal org has exactly one member. `handleDeleteMe` now selects on that
column.

The query is renamed `ListPersonalOrgIDsForPerson` → `ListOwnedPersonalOrgIDsForPerson`
rather than quietly reworded: the old name said "for person" and meant "belonging to",
and the whole defect was that reading it as "owned by" was wrong. Renaming makes every
call site restate which one it means.

Two deliberate choices worth knowing:

- **`ON DELETE SET NULL`, not `CASCADE`.** Whether deleting a club owner's account should
  destroy the club is a product question, and a cascade would answer it silently. The
  explicit, documented deletion of the caller's *personal* orgs stays in the handler.
- **Club orgs remain excluded even when the caller owns one.** That matches today's
  behaviour — orphan it, leave the data — and the owner column is now there to make the
  decision with when it is time.

`TestDeleteMeSparesAnOrgTheCallerOnlyBelongsTo` covers it, and also asserts that
registering sets an owner at all, since an org created without one could never be deleted
by the person who made it. It was confirmed to fail against the membership-based
selection ("the owner's org must survive a member's account deletion, found 0"). The
backfill was separately run against a reconstructed pre-migration database — an org with
an admin plus a second member — and picks the admin.

### P2 — `POST /sync` bypasses the coach role (confirmed)

`handleSyncPush` calls `resolveOrg` for the owning organization and then never looks at
`org.roles`. Every projected upsert writes into that org. The REST equivalents all gate
on `requireCoach`:

```
POST /api/v1/teams   (player)   -> 403 {"code":"FORBIDDEN","message":"only coaches can do that"}
POST /api/v1/sync    (player)
  {"upserts":[{"type":"Team","id":"1111…","payload":{"name":"Created via sync"}}]}
                                -> 200 {"cursor":null,"conflicts":[]}

GET  /api/v1/teams   (coach)    -> 200
  [{"id":"1111…","organizationId":"4f04caa1-…","name":"Created via sync",…}]
```

The same holds for `Drill` and `Session`. This is the shape of the first audit's
structural finding — two write paths into one set of tables that disagree about who may
write — surviving in the authorization dimension after being fixed in the tenancy
dimension.

**Fix.** Require the coach roles in `handleSyncPush` for the projected types that land in
org-scoped tables, or scope a non-coach's push to `sync_documents` only.

### P3 — sync-owned rows in a shared org are hard-deleted by cascade

`teams.sync_account_id` (and `drills`, `sessions`, `persons`, `players`, `events`,
`diagrams`) is `REFERENCES persons (id) ON DELETE CASCADE`. Combined with P2, a team
pushed by one coach into a shared org is *row-deleted* when that coach deletes their
account — taking `games` and `roster_memberships` with it via their own cascades, and
leaving no tombstone, so other devices holding the team are never told it is gone. That
is precisely the failure mode M2 fixed for `DELETE /teams`, reintroduced through the FK.

**Fix.** `ON DELETE SET NULL` on `sync_account_id` plus a tombstone pass, so the row
survives as unsynced org data and the deletion still reaches clients.

### P4 — evaluations about a shared athlete are readable by every org (confirmed)

`handleListPersonInstances` and `handlePersonAggregate` correctly gate on
`personVisibleTo`, but `ListInstancesForPerson` and `AggregateScoresForPerson` filter on
`subject_person_id` alone — no org, no template scope. So visibility of the *athlete*
grants visibility of every evaluation anyone has ever filed about them:

```
POST /api/v1/persons        (club A) -> 201  athlete 88cc30e4-…
  [athlete also holds a membership in club B]
POST /api/v1/form-instances (club A) -> 201  {"key":"sleep","numericValue":1}

GET /api/v1/persons/88cc30e4-…/instances (club B) -> 200
  [{"id":"e3539a7d-…","context":"pre_game","templateName":"Pre-Game Check-In",…}]
GET /api/v1/persons/88cc30e4-…/aggregate (club B) -> 200
  [{"key":"sleep","label":"Sleep quality","samples":1,"average":1,…}]
```

A player who moves clubs carries their old club's private assessments with them. The
aggregate is described in the README as the product's analytical core; it is also the
thing a rival coach would most want to read.

**Fix.** Join `form_instances` to `form_templates` and filter on the template's
`organization_id` (with the personal-template case handled as `templateFor` does), so an
org sees the evaluations it authored.

A related note in the same area: `handleSubmitInstance` admits the `parent` and `player`
roles, and its subject check is `personVisibleTo` — org-wide. In a club org that lets any
player file scored evaluations about any other player, which is not what the role names
suggest. The `guardianships` table exists for exactly this distinction and is never
consulted by any authorization path.

---

## Medium

### M1 — `GET /sync` has no page limit

`ListSyncChangesSince` returns every row with `seq > cursor` across eight sources with no
`LIMIT`, and `handleSyncPull` accumulates all of it into one response. Pushes are capped
at `maxSyncBatch = 1000`, but nothing caps how many pushes an account makes, so an
account can grow its own delta without bound and then make every full pull (`since=0` —
what a reinstall sends) an unbounded allocation on the server.

**Fix.** `LIMIT` the query to a page and return the high-water `seq` as the cursor. The
protocol already supports it: the client loops until the cursor stops moving. Nothing
about the wire format changes.

---

## Low

**L1 — `/docs` loads Swagger UI from unpkg, unpinned.** `docs.go` pulls
`https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js` — a third-party CDN, a
floating major version, no `integrity` attribute. Whatever that URL returns runs on your
origin. Pin the exact version with an SRI hash, or vendor the two files.

**L2 — `select` answers are unvalidated.** `validateAnswer` range-checks `scale` against
its config but only requires that a `select` answer carry *some* `textValue`. A select
field's config is where its options live; not consulting it means a select column
accumulates arbitrary strings, which is the same class of defect M3 fixed for scales.

**L3 — `PATCH /games/{id}` cannot clear `kickoffAt`.** `optionalString` and
`optionalInt32` both treat explicit `null` as "clear it", and `UpdateGame` honours that
for `opponent`, `home_away` and the scores. `kickoffAt` is decoded with a bare
`json.Unmarshal` into a string, so `null` yields `""` and then fails RFC3339 parsing with
a 400; the SQL is `COALESCE(narg('kickoff_at'), kickoff_at)`, which cannot write NULL
either way. A cancelled fixture's kickoff time cannot be unset.

**L4 — refresh tokens are never reaped.** `RevokeRefreshToken` and
`RevokeRefreshTokensForAccount` stamp `revoked_at`; nothing deletes. `refresh_tokens`
grows by one row per login and per refresh, forever. A periodic
`DELETE FROM refresh_tokens WHERE expires_at < now() - interval '30 days'` is enough —
note it must keep recently-revoked rows, since replay detection reads them.

**L5 — the `conflicts` array is an existence oracle.** A push naming an id the account
does not own returns that record in `conflicts`; a push naming a free id returns nothing.
That distinguishes "this UUID exists somewhere in the database" from "it does not",
across every tenant. UUIDs are not enumerable so the practical value is low — but it is
the same signal `personVisibleTo` deliberately withholds by answering 404 instead of 403,
and the two should agree.

**L6 — `sync_documents.type` is an unbounded namespace.** Any `rec.Type` outside the
seven projected types becomes a `sync_documents` row with that type verbatim. Nothing
validates the type against a known set, bounds its length, or caps rows per account; the
only limit is the 4 MiB body cap and 1000 records per push. A known-types allowlist would
also catch client/server drift, which is the more likely everyday problem.

---

## What is done well

- **The remediation itself.** Twenty findings, twenty fixes, each with a comment
  explaining what was wrong and why the new shape is right — and the comments are
  accurate, which is rarer. The `clientIP` middleware replacing `RealIP` is the standout:
  it names the attack, explains why chi's own helper was the wrong tool, and fails closed
  in the unconfigured case with a warning that says what the consequence is.
- **`applied()` and the `:execrows` conversion.** Turning "did this write land" into a
  value the handler must handle, rather than an assumption, is what makes C1's fix
  structural instead of seven separate `WHERE` clauses.
- **The refresh-token grace window.** Reuse detection that cascades is standard; noticing
  that an offline-first phone app retries a refresh whose response it lost, and that the
  cascade would then log every device out over a dropped connection, is not.
- **`personVisibleTo` returning 404 rather than 403**, with the reasoning written down.
- **Test naming.** `TestSyncCannotAdoptRESTCreatedRow`,
  `TestForwardingHeadersCannotChooseTheBucket`, `TestLoginDoesNotRevealWhichEmailsExist` —
  each names the invariant rather than the mechanism, so a future change that breaks the
  invariant fails a test whose name explains what it broke.
- **The CI `docker build` step**, added with a comment explaining that `setup-go` and the
  `golang` image disagree about `GOTOOLCHAIN` and that the disagreement is otherwise
  invisible. That is a real bug caught and then fenced.

---

## Suggested order of work

1. ~~**C1** — one migration on `form_instances.template_id`, plus the two regression
   cases.~~ Done.
2. ~~**H1** — require `ENV`.~~ Done.
3. ~~**C2** — make Apple provisioning refuse to adopt a claimed row.~~ Done.
4. ~~**P1** — add an owner to `organizations` **before** the invite endpoint ships.~~ Done.
5. **P2, P3, P4** — the rest of the multi-member org work, alongside that feature.
6. **M1** and the Lows as ordinary hardening.

Nothing reachable today by a single account is left open, and the one latent finding
whose cost rose with time is closed. What remains (P2–P4) is work that belongs with the
club feature itself: it is not dangerous until that feature exists, and it is cheap to do
alongside it rather than ahead of it.
