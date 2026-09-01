# SoccerKit API — third audit

Audit of `monsoudz/soccerkit-api` at `373e83a`, the tip of the remediation that followed
[`docs/AUDIT-2.md`](AUDIT-2.md). Same two questions: did the second report's fixes hold,
and what is wrong now that was not looked at then.

Every finding marked **confirmed** was reproduced against a live server (`httptest` +
Postgres, the project's own harness). Transcripts are quoted verbatim. The repro tests
were run and then removed; the tree is unmodified.

Baseline health: `go vet`, `go build` and `go test ./...` all pass clean.

> **Status.** Every finding in this report is fixed in the commits following it. Each
> reproduction was re-run against the fixed code and now fails closed, and each is left
> described in the present tense as a record of what was wrong and why. The AUDIT-2
> leftovers (its M1 and L1–L6, and P2–P4) are untouched and still open.
>
> Three of the fixes change behaviour deliberately, and the first is client-visible:
>
> - **`POST /auth/apple` no longer signs a caller into an existing account that merely
>   shares its email address.** It answers `409 EMAIL_ALREADY_REGISTERED` instead, and
>   linking moved to the authenticated `POST /api/v1/me/apple-link`. Checked against the
>   client: `SoccerCoachKit/Networking/BackendAPI.swift` calls only `/v1/auth/apple` and
>   `/v1/auth/refresh` — the app has no password registration or login at all — so no
>   legitimate app flow can reach the new 409 today. It reaches it exactly when somebody
>   has pre-registered that address through the API, which is the attack. The app still
>   renders that as a generic `APIError.http(status: 409)`, so a message naming the
>   remedy is worth adding when the password flow ships.
> - **An Apple address Apple has not marked verified is treated as absent**, so the
>   account is provisioned under the synthesized `apple_<sub>@…` address rather than the
>   claimed one. `user_accounts.email` is UNIQUE and is what both credential endpoints
>   key on; an unverified address written into it is the thing that made C1 possible.
> - **A numeric answer is now bounded** (`maxAnswerMagnitude`, 1e12). A template author
>   who was relying on a `number` field accepting anything gets a 400. Nothing legitimate
>   records 1e12 goals, and the alternative was leaving the aggregate breakable.
>
> Two notes on what the fixes do *not* do. `POST /auth/register` still discloses which
> addresses have accounts (L5): closing that properly means verifying addresses at
> registration, which needs mail infrastructure this project does not have, and which
> would also be the root fix for C1 rather than the containment C1 got. And the numeric
> bound cannot reach rows already written, so `AggregateScoresForPerson` was separately
> made overflow-proof — validation added today does nothing about yesterday's data.

---

## Round two: the four fixes hold

Re-read against the current code, and the regression tests each fix shipped with all
pass.

| Was | Now |
|---|---|
| AUDIT-2 C1 `DELETE /me` blocked by a form instance | `0006_form_instances_cascade.sql` moves `form_instances.template_id` to `ON DELETE CASCADE`; `TestDeleteMeAfterSelfEvaluation` and the rewritten `TestDeleteMeSparesSharedAthlete` cover both paths |
| AUDIT-2 C2 Apple adopts a pre-claimed `persons` row | `CreatePersonWithID` is `ON CONFLICT (id) DO NOTHING`; `provisionAppleIdentity` reads `pgx.ErrNoRows` as a refusal, logs it and returns 409 |
| AUDIT-2 H1 `ENV` defaults to `development` | `config.Load` returns a named error when `ENV` is unset; `TestUnsetEnvRefusesToBoot` pins it |
| AUDIT-2 P1 `DELETE /me` deletes orgs by membership | `organizations.owner_person_id` (migration `0007`), set by both provisioning paths and by `cmd/seed`; `ListOwnedPersonalOrgIDsForPerson` selects on it |

The precondition P2–P4 rest on is still true: `CreateMembership` has exactly three call
sites — `handleRegister`, `provisionAppleIdentity` and `handleCreatePerson` — and none of
them puts a second *account-holding* person in an existing org. P2 (sync bypasses
`requireCoach`), P3 (sync-owned rows hard-deleted by FK cascade) and P4 (evaluations
readable by every org the athlete belongs to) are unchanged and still latent.

Also still open, exactly as described: AUDIT-2 **M1** (`GET /sync` has no page limit —
`ListSyncChangesSince` still has no `LIMIT`) and **L1–L6**. L6 has a number now: a push
with a 100 000-byte `type` and a 100 000-byte `id` is accepted, and the next pull returns
200 078 bytes for that one record.

The loose thread AUDIT-2 noted is also still there: `GetPerson` is `SELECT * FROM persons
WHERE id = $1` with no `deleted = false`, reachable only through `personVisibleTo`'s
`personID == caller` short-circuit.

---

## Summary of new findings

| # | Severity | Finding |
|---|----------|---------|
| C1 ✅ | **Critical** | Registering an address you do not own hands you the account of whoever signs in with Apple at that address |
| M1 ✅ | Medium | Two ordinary answers permanently 500 the athlete aggregate, and nothing can remove them |
| M2 ✅ | Medium | Refresh rotation is check-then-act, so one refresh token redeems many times and the replay cascade never fires |
| M3 ✅ | Medium | A token whose account was deleted gets a 500 from `/me`, so the client cannot tell "signed out" from "server broken" |
| L1 ✅ | Low | A password over bcrypt's 72-byte input limit is a 500, not a 400 |
| L2 ✅ | Low | Two template fields sharing a `key` is a 500, not a 400 |
| L3 ✅ | Low | Games outlive their team's tombstone and stay readable and patchable |
| L4 ✅ | Low | A `\u0000` anywhere in a sync payload is a 500, and the client will retry it forever |
| L5 | Low | `POST /auth/register` says which addresses have accounts; `POST /auth/login` deliberately does not |
| L6 ✅ | Low | The HTTP server sets `ReadHeaderTimeout` and nothing else |

---

## Critical

### C1 ✅ — registering an address you do not own hands you that person's account (confirmed)

Two facts, each defensible alone:

1. **`POST /auth/register` never verifies the address.** There is no verification mail,
   no confirmation token, no `email_verified` column. `handleRegister` checks the address
   is unused and shaped like an email, and creates the account.
2. **`POST /auth/apple` merges a first-time Apple identity into any account with the same
   address** (`handlers_apple.go`, the `case err == nil:` branch), and returns tokens for
   *that* account and *that* Person.

Together they are an account pre-hijack. The attacker registers the victim's address
before the victim's first Apple sign-in; the victim then signs in with Apple, Apple
asserts the address is verified, and the server hands them the attacker's account.

AUDIT-2's M6 fix closed the mirror image of this — an Apple identity *claiming* an
address it has not verified — and the reasoning it wrote down is right as far as it goes:

> Merging an account on an unverified email is a standard takeover primitive.

Both halves of a merge have to be trustworthy. Apple's half now is. The local half never
was: `user_accounts.email` is an address somebody typed.

**Reproduction.** Two parties, four requests, no special knowledge beyond the victim's
email address:

```
POST /api/v1/auth/register   (attacker, email = victim@example.com, password = password123)
                                        -> 201  person 2b7f3701-…

POST /api/v1/auth/apple      (victim, sub = victim-apple-sub, email = victim@example.com,
                              email_verified = true)
                                        -> 200
  {"token":"…","refreshToken":"…","personID":"2b7f3701-…"}
                                                  ^ the attacker's Person

POST /api/v1/teams           (victim)   -> 201  {"id":"eb32a1fb-…","name":"Victim U12",
                                                 "organizationId":"1dceaeac-…"}

POST /api/v1/auth/login      (attacker, password123)  -> 200
GET  /api/v1/teams           (attacker) -> 200
  [{"id":"eb32a1fb-…","name":"Victim U12","organizationId":"1dceaeac-…", …}]

  SELECT count(*) FROM user_accounts
   WHERE email = 'victim@example.com'
     AND apple_sub IS NOT NULL AND password_hash IS NOT NULL;   -> 1
```

That last row is the finding in one line: one account, holding the attacker's password
and the victim's Apple identity, permanently. Nothing rotates the password, nothing
notifies anyone, and no endpoint exists to unlink an Apple sub.

**What the attacker gets.** Everything the victim does from then on, in an app whose
whole point is storing children's records: `medical_notes`, `emergency_contact_name`,
`emergency_contact_phone`, `birthdate` and `email` for every athlete the victim adds, plus
every scored evaluation about them. Read continuously, from an ordinary password login,
with no anomaly to detect — the attacker is a legitimate credential-holder on that
account. They can also delete all of it (`DELETE /me`), which erases the victim's account
and its athletes' Person rows.

**Why the victim notices nothing.** The link branch keeps the existing row — `person =
linked` — so `/me` returns the display name and org name the *attacker* chose. A victim
signing in for the first time sees a set-up account with a plausible club name, which is
what a first sign-in looks like anyway.

**Precondition, stated honestly.** The attacker must act before the victim's first Apple
sign-in: once `user_accounts.apple_sub` is set, `GetUserAccountByAppleSub` short-circuits
at branch 1 and the merge branch is never reached. That is a real constraint and it is
what separates this from a bug that works against an established user base. It is also
the weakest possible version of that constraint — every user of a newly shipped app is
in exactly that state, the input is an email address rather than an opaque Apple subject
(AUDIT-2 C2's precondition), and `POST /auth/register` will tell an attacker which
addresses are still free (see L5).

The same unverified-address hole has a quieter second effect: registering someone's
address denies them registration, permanently, with a 409.

**Fix.** An address nobody proved control of must not be a merge key. Two ways, and the
second is the cheap one:

- Verify the address at registration. This is the root fix and it needs mail
  infrastructure this project does not have yet.
- Stop merging silently. When a first Apple sign-in matches an existing account, return a
  distinguishable 409 — "an account already exists for this address; sign in with your
  password, then link Apple" — and add an authenticated `POST /me/apple-link` that sets
  `apple_sub` for the *already-authenticated* account. Proof of control then comes from
  the password, which is the one thing the attacker's account and the victim's Apple
  identity cannot both have.

The cost is one extra step for a genuinely dual-method user, once. Note that
`TestAppleAuthLinksExistingEmailAccount` asserts the vulnerable behaviour directly, so
the fix has to rewrite that test — it is worth reading first, because it is the reason
this looks deliberate rather than overlooked.

**Fix (applied).** The merge branch is gone: a first Apple sign-in whose address already
has an account returns `409 EMAIL_ALREADY_REGISTERED` — its own code, because the client
has to tell it from the other 409 on this endpoint (a pre-claimed Person id, which needs
support rather than a retry). Linking moved to `POST /api/v1/me/apple-link`, which runs
behind `requireAuth`, so the proof that the two identities are the same person is the
session the caller already holds. `LinkAppleSub` is `:execrows` guarded on
`apple_sub IS NULL`, and one Apple ID still belongs to at most one account.
`appleEmail` also stopped trusting an unverified address, for the reason the address was
dangerous in the first place.

`TestAppleSignInWillNotTakeOverAnExistingAddress` is the reproduction above, inverted:
the attacker pre-registers, the victim's sign-in is refused, and nothing is linked.
`TestAppleLinkRequiresTheAccountsOwnSession` covers the replacement end to end, including
that the endpoint is unreachable without a token, that re-linking is idempotent, and that
neither an account nor an Apple ID can hold two of the other.
`TestAppleAuthLinksExistingEmailAccount` — which asserted the vulnerable behaviour — is
gone, and `TestAppleAuthRefusesToLinkUnverifiedEmail` became
`TestAppleAuthIgnoresAnUnverifiedAddress`, since there is no longer a link for an
unverified address to be refused *from*.

**Not covered by the fix.** A deployment that has been running the old code may already
have merged accounts, and nothing distinguishes an honest merge from a takeover after the
fact — the old branch was the *only* way an Apple identity ever reached a password
account, so every such row went through it without proof. That set is exactly:

```sql
SELECT id, email FROM user_accounts
 WHERE password_hash IS NOT NULL AND apple_sub IS NOT NULL;
```

Worth reviewing before deploying. Going forward the same query means something else —
an account that linked deliberately through `/me/apple-link` — so it is worth running
now, while the answer is unambiguous.

---

## Medium

### M1 ✅ — two ordinary answers permanently break the athlete aggregate (confirmed)

`validateAnswer` bounds a `scale` answer against its field config (AUDIT-1 M3) and
rejects NaN and ±Inf. A `number` field has no config and no bounds, so `1e308` is a valid
answer. `AggregateScoresForPerson` then runs `avg(fa.numeric_value)` over them, and two
such answers overflow `double precision` inside the sum:

```
POST /api/v1/form-instances  {"key":"goals","numericValue":1e308}   -> 201
POST /api/v1/form-instances  {"key":"goals","numericValue":1e308}   -> 201

GET  /api/v1/persons/{id}/aggregate            -> 500
  {"error":{"code":"INTERNAL","message":"An unexpected error occurred."}}
```

It is not confined to that key or that template. The aggregate is one `GROUP BY` over
every answer about the athlete, so an ordinary pre-game check-in filed afterwards is
unreadable too:

```
POST /api/v1/form-instances  {"key":"sleep","numericValue":4}       -> 201
GET  /api/v1/persons/{id}/aggregate                     -> 500
GET  /api/v1/persons/{id}/aggregate?context=pre_game    -> 200  [{"key":"sleep", …}]
GET  /api/v1/persons/{id}/instances                     -> 200
```

Only the context-scoped call still answers, because it filters the poisoned instances
out. And there is no repair: the API has no endpoint that deletes a form instance or an
answer. Once written, the athlete's unscoped aggregate is dead until someone runs SQL
against production.

This is the query the README calls the product's analytical core, and the reason
`form_answers` is normalized rather than jsonb. It is broken by a number that a fat
finger produces as easily as an attacker — `1e308` is what a client sends if it ever
serializes a `Double.greatestFiniteMagnitude`.

**Fix.** Bound `number` the way `scale` is bounded. A field-level `min`/`max` in
`config` is the consistent shape; a blanket sanity range (say ±1e12) applied in
`validateAnswer` is the one-liner. Either way the aggregate should also be made to
survive bad rows already in the table — `avg` over a filtered range, or a
`double precision` guard — because validation added today does nothing about rows written
yesterday.

**Fix (applied).** Both halves. `validateAnswer` bounds every numeric answer at
`maxAnswerMagnitude` (1e12) — far outside anything a coach records and far inside
float8's ceiling, which is the property that matters: the sum of any realistic number of
them cannot overflow. And `AggregateScoresForPerson` now accumulates the average in
`numeric` rather than float8, because the bound cannot reach rows already written and
those rows are unreachable through the API. `min`/`max` need no such treatment; they
never sum.

`TestNumericAnswersAreBounded` covers the validation, `TestAggregateSurvivesUnboundedLegacyAnswers`
plants two out-of-range answers directly in the table — the way the old code would have
accepted them — and requires the aggregate to still answer.

### M2 ✅ — refresh rotation is check-then-act, so one token redeems many times (confirmed)

`handleRefresh` reads the row, decides it is live, and then revokes it in a separate
unconditional statement:

```go
stored, err := s.store.GetRefreshToken(ctx, hashRefreshToken(req.RefreshToken))
…
if err := s.store.RevokeRefreshToken(ctx, stored.ID); err != nil {
```

with `RevokeRefreshToken` being `UPDATE refresh_tokens SET revoked_at = now() WHERE id =
$1` — no `revoked_at IS NULL` predicate, no row count consulted, no transaction around
the pair. Concurrent presentations of the same token all read it before any of them
writes, and all of them mint a fresh family:

```
32 concurrent POST /api/v1/auth/refresh, one token:
  round 0: 6 of 32 returned 200
  round 1: 2 of 32 returned 200
  round 2: 2 of 32 returned 200
```

Six independent live refresh chains from one single-use token. The design AUDIT-1 M5
built rests on the opposite invariant — one token, one use, and a second use is evidence
of a leak — and this is the gap through which a second use is not evidence of anything.
The practical consequence is that reuse detection is a coin flip in exactly the case it
was written for: an attacker holding a stolen token who refreshes while the legitimate
client refreshes gets a working chain, and *neither* party trips
`RevokeRefreshTokensForAccount`, so nobody is signed out and nothing is logged.

**Fix.** Make the revoke the guard rather than a follow-up: `UPDATE refresh_tokens SET
revoked_at = now() WHERE id = $1 AND revoked_at IS NULL` as `:execrows`, and treat zero
rows the way the handler already treats an already-revoked row — the grace-window check
moves inside that branch. `applied()` in `handlers_sync.go` is the same pattern; this is
the last place in the codebase that asks a question and then acts as though the answer
cannot have changed.

**Fix (applied).** `RevokeRefreshToken` is now `:execrows` with
`AND revoked_at IS NULL`, and `handleRefresh` treats zero rows the way it treats a replay
inside the grace window — refused, no cascade, because microseconds apart is the retry
case rather than the theft case. The write is the arbiter; nothing is decided by a read
that can go stale. `applied()` in `handlers_sync.go` was already this shape.

`TestOneRefreshTokenRedeemsOnce` fires 32 simultaneous redemptions and requires exactly
one to win, then requires that one to still rotate — refusing all 32 would satisfy a
count alone and would log a coach out for refreshing twice. Against the old code it fails
in most runs (2, 3, 4, 6 and 20 of 32 across six runs) and passes in some, which is what
a race looks like and is why the assertion is on the invariant rather than on a number.

### M3 ✅ — a deleted account's token gets a 500 from `/me` (confirmed)

`handleDeleteMe` is explicit that the access token outlives the row it names:

> The access token (a JWT) outlives the row it points at, so a call whose Person is
> already gone must succeed too.

The delete path honours that. The read path does not: `handleGetMe` calls `GetPerson` and
hands `pgx.ErrNoRows` straight to `writeError`, which has no case for it and returns a
500.

```
DELETE /api/v1/me                    -> 204
GET    /api/v1/me   (same token)     -> 500 {"error":{"code":"INTERNAL", …}}
GET    /api/v1/sync?since=0          -> 200 {"records":[],"deletes":[],"cursor":"0"}
```

For up to `JWT_ACCESS_TTL` (15 minutes by default) after the deletion the app's own
"who am I" call reports a server fault. An iOS client cannot distinguish that from an
outage, so the reasonable thing for it to do — keep the session and retry — is the wrong
thing, and the account-deletion flow that the 204 completed looks half-finished. The
right answer is 401: the token is valid and identifies nobody.

`handleLogin` and `handleRefresh` have the same shape (`GetPerson` after finding an
account) but cannot reach it — `user_accounts.person_id` cascades — so this is `/me`
alone.

**Fix.** Map `pgx.ErrNoRows` from `GetPerson` in `handleGetMe` to
`errUnauthorized("this account no longer exists")`. Worth considering more generally:
`writeError` could translate a bare `pgx.ErrNoRows` to a 404 rather than a 500, since
every handler that means something else by it already intercepts it.

**Fix (applied).** `handleGetMe` maps `pgx.ErrNoRows` to 401 with a message that says the
account is gone, so the client signs out instead of retrying.
`TestMeAfterAccountDeletionIsUnauthorized` covers it. Deliberately narrow: `writeError`
could translate every bare `pgx.ErrNoRows` into a 404, but every handler that means
something specific by it already intercepts it, and a blanket rule would turn genuine
"this should exist" bugs into quiet 404s.

---

## Low

**L1 ✅ — a long password is a 500.** `bcrypt.GenerateFromPassword` returns
`ErrPasswordTooLong` above 72 bytes, and `handleRegister` passes it to `writeError`:

```
POST /api/v1/auth/register  (116-byte passphrase)  -> 500
  {"error":{"code":"INTERNAL","message":"An unexpected error occurred."}}
```

Nothing is silently truncated — x/crypto refuses rather than truncating, which is the
safe behaviour — but a passphrase or a password-manager string is a routine input and
the caller is told the server is broken. Validate the length next to the existing
`len(req.Password) < 8` check and return a 400 that says 72 bytes.

**L2 ✅ — a duplicate field key is a 500.** `POST /api/v1/templates` with two fields both
keyed `speed` reaches `UNIQUE (template_id, key)` and returns 500. `handleCreateTemplate`
already loops the fields to validate `key` and `kind`; the same loop should reject a
repeat, exactly as `handleSubmitInstance` does for duplicate answers.

**L3 ✅ — games outlive their team's tombstone.** `DeleteTeam` tombstones the team, and
`GetTeam` filters `deleted = false`, so the team and its game *list* disappear. `games`
has no `deleted` column and `GetGame` checks only the organization, so the game itself
stays live by id:

```
DELETE /api/v1/teams/{id}          -> 200
GET    /api/v1/teams/{id}          -> 404
GET    /api/v1/teams/{id}/games    -> 404
GET    /api/v1/games/{gameId}      -> 200  {"teamId":"5d61f118-…", …}
PATCH  /api/v1/games/{gameId}      -> 200  {"ourScore":3,"opponentScore":1, …}
```

You can still record the result of a match for a team that no longer exists. This is
AUDIT-1 M1/M2 — REST and sync disagreeing about what a deletion means — with `games`
missed because it has no tombstone at all. Give `games` the same treatment, or have
`gameInOrg` join the team and require it live.

**L4 ✅ — a `\u0000` in a sync payload is a 500.** `"text":"hello\u0000world"` is legal JSON
and illegal in `jsonb`, so the insert errors and the whole push fails:

```
POST /api/v1/sync  {"upserts":[{"type":"Note","id":"note-1",
                                "payload":{"text":"hello\u0000world"}}]}   -> 500
POST /api/v1/sync  (an ordinary later push)                                -> 200
```

The account is not wedged server-side, but the client is: an offline-first app retries
the batch it failed to push, and this one fails identically every time, so that device
stops syncing until the record is edited or deleted locally. Strip or reject `\u0000` in
`applyUpsert` and return a 400 naming the record, so the client can drop it.

**L5 — registration says which addresses have accounts.** `handleRegister` answers 409
`"an account with that email already exists"` on a pre-flight lookup.
`handleLogin` goes to real trouble not to leak the same fact —
`burnPasswordComparison`, a whole `dummyPasswordHash` — and `TestLoginDoesNotRevealWhichEmailsExist`
pins it. Registration is a weaker oracle (an attacker learns the address is taken, not
whose it is), but it is the reconnaissance step for C1: it says which addresses are still
free to claim. Rate-limiting `/auth` bounds enumeration and is already in place; the
residual leak is worth a line in the docs rather than a redesign, unless C1's fix
introduces mail verification, in which case both endpoints can go quiet.

**L6 ✅ — the HTTP server has one timeout.** `cmd/api/main.go` sets `ReadHeaderTimeout: 10s`
and leaves `ReadTimeout`, `WriteTimeout` and `IdleTimeout` at zero.
`middleware.Timeout(30s)` bounds handler *execution*, not how long a client may take to
dribble out a request body or read a response, so a slow-body client holds a connection
and a goroutine indefinitely. `limitBody` caps the size, not the duration. Set
`ReadTimeout` and `WriteTimeout` (30s each is generous for this API) and `IdleTimeout`.

---

**Fixes applied.** L1 validates the password length next to the existing minimum, naming
bcrypt's 72-byte limit. L2 rejects a repeated field key by name, the way the answer side
already did. L3 makes `gameInOrg` resolve the game's team through `teamByIDInOrg`, so a
game is exactly as reachable as the team it belongs to. L4 maps Postgres `22P05` to a 400
naming the record, so an offline-first client can drop the record instead of retrying it
forever. L6 sets `ReadTimeout`, `WriteTimeout` and `IdleTimeout`, with `WriteTimeout`
deliberately longer than the router's 30s handler timeout so the timeout response can
still be written. Each has a regression test named for the invariant it pins.

---

## What is done well

- **The AUDIT-2 remediation reads as one piece of work.** Each fix carries a comment that
  states what was wrong, not just what the code now does, and `0007_organization_owner.sql`
  goes further: it explains why the column is worth adding *before* the feature that needs
  it. That is the argument the fix exists to win, written down where the next person will
  find it.
- **Renaming `ListPersonalOrgIDsForPerson` to `ListOwnedPersonalOrgIDsForPerson`.** The
  defect was that "for person" was read as "owned by" when it meant "belonging to".
  Changing the name forced every call site to restate which one it meant, and a quiet
  reword would not have.
- **`CreatePersonWithID`'s comment.** It documents a refusal path — `DO NOTHING`, zero
  rows, treat as hostile — and then explains why no legitimate flow can reach it. Refusals
  that are not explained get "fixed" later by someone who cannot see why they are there.
- **`cmd/seed` sets `owner_person_id` after the coach exists**, with a comment saying why
  the order matters. The seed is the easiest place for a new invariant to be quietly
  broken, and it was not.
- **The `avg` overflow in M1 is the only place in the codebase where a query can fail on
  its own data.** Everything else — `applied()`, the typed PATCH decode, the per-answer
  validation — pushes failure into a value the handler has to handle. The gap is
  conspicuous because the surrounding standard is high.

---

## Suggested order of work

1. ~~**C1** — stop merging on an unverified address. Everything else in this report is
   recoverable; this one hands over a live account and there is no way to take it back.~~
   Done. The iOS app reaches the new 409 only on the attack path today (it has no
   password flow), so nothing is blocked on client work — but whenever a password sign-in
   does ship, it has to handle that code and call `POST /me/apple-link`.
2. ~~**M2** — one predicate and an `:execrows`, and it restores a control that is
   currently probabilistic.~~ Done.
3. ~~**M3** and **L1**, **L2**, **L4** — four unmapped errors surfacing as 500s.~~ Done.
4. ~~**M1** — bound `number`, and decide what to do about rows already written.~~ Done:
   bounded going forward, and the query made overflow-proof for what is already there.
5. ~~**L3**, **L6**~~ Done. **L5** is left open on purpose: closing it means verifying
   addresses at registration, which needs mail infrastructure — and that, not the 409,
   is the root fix for C1. It is the next thing worth building in this area.
6. The AUDIT-2 leftovers (its M1 sync page limit and L1–L6) remain ordinary hardening,
   and **P2–P4** remain work for the club/invite feature, unchanged.
