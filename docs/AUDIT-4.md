# SoccerKit API — fourth audit

Audit of `monsoudz/soccerkit-api` at `00c9469`, the tip of the `audit-3` branch: the ten
fixes from [`docs/AUDIT-3.md`](AUDIT-3.md) plus the removal of password authentication
that followed them. That branch has not been pushed or deployed, which matters for M1.

A deliberately narrow pass. AUDIT-3 closed everything it found and the removal deleted a
whole authentication path, so this looks at what those changes did rather than sweeping
the codebase again: the new shape of `/auth/apple`, the schema change, and whether
anything that used to be true stopped being true. Three findings, all reproduced against
a live server or a live deploy sequence, and none of them in the class the first three
reports were about — nobody's data is reachable by anyone who should not have it.

Baseline health: `go vet`, `go build` and `go test ./...` pass clean, `sqlc generate`
produces no drift, and the repro tests were run and then removed; the tree is unmodified.

> **Status.** All three are fixed in the commits following this one, and each reproduction
> was re-run against the fixed code. They are left described in the present tense as a
> record of what was wrong.
>
> M1 is the one that changes how this ships. The column drop moved out of the removal
> commit into its own migration (`0009`) and its own release, and `0008` became a
> check-only migration that refuses to boot a build with password authentication removed
> while any account still depends on it — which is where that check belonged, because
> removing the endpoints is what locks those accounts out, not dropping the column.
>
> The split buys exactly one thing, and it is worth being precise about which: **the
> release carrying the behaviour change is now reversible.** Verified — deploy it, roll
> the binary back one release against the same database, and returning users still sign
> in. The drop that follows is still a one-way door, because the release before it reads
> the column; that is inherent to dropping a column that anything still selects, and it
> is why the drop now ships alone, later, and small.

---

## Round three: everything holds

Re-read against the current code, all ten AUDIT-3 fixes are in place and their regression
tests pass. The removal did not weaken any of them:

| Fix | Still true |
|---|---|
| C1 no merge on a shared address | The refusal survived the removal and is now belt-and-braces: `/auth/register` is gone, so nothing can plant an address at all |
| M1 bounded numeric answers + numeric-accumulating average | Untouched |
| M2 single-use refresh rotation | Untouched, and now load-bearing for the whole product — see the note on L4 below |
| M3 401 for a deleted account | Untouched |
| L1 long passwords | Moot: there are no passwords |
| L2, L3, L4, L6 | Untouched |
| L5 registration enumeration | Closed by deletion rather than by defence |

The AUDIT-2 leftovers are unchanged and still open: its **M1** (`GET /sync` has no page
limit), **L1–L6**, and **P2–P4**, which still wait on the club/invite feature. The
precondition P2–P4 rest on also still holds — `CreateMembership` has three call sites and
none of them puts a second account-holding person in an existing organization.

Two things whose weight changed even though their code did not:

- **AUDIT-2 L4 — refresh tokens are never reaped.** With passwords gone, a refresh token
  is the only credential this service stores, and the only thing standing between a
  stolen row and an account. The table still grows by one row per sign-in and per
  refresh, forever, and revoked rows are never removed. Reaping matters more now than
  when it was written down as a Low.
- **AUDIT-2 L5 — the sync `conflicts` array as an existence oracle.** Unchanged, but
  worth noting alongside it that the sync cursor is `nextval('sync_seq')` from a single
  global sequence, so a coach's cursor advances with every other tenant's writes too. It
  is a weak volume oracle and nothing more; it is listed here so the next person does not
  have to rediscover that the cursor is not per-account.

---

## Summary of new findings

| # | Severity | Finding |
|---|----------|---------|
| M1 | Medium | Deploying migration `0008` cannot be rolled back: the previous release 500s on every sign-in |
| L1 | Low | `/health` answers liveness only, so a process whose database is gone still reports OK |
| L2 | Low | Concurrent sign-ins at one address return 500 where the same request sequentially returns a typed 409 |

---

## Medium

### M1 — deploying `0008` cannot be rolled back (confirmed)

`0008_drop_password_hash.sql` guards carefully against stranding an *account*. It does not
consider the other thing a column drop strands: the release you were running an hour ago.

Migrations here are forward-only — there are no down-migrations, and `database.Migrate`
applies anything unapplied at boot. sqlc expands `SELECT *` into an explicit column list
at generate time, so the previous binary's `GetUserAccountByAppleSub` reads

```sql
SELECT id, person_id, email, password_hash, apple_sub, created_at, updated_at
FROM user_accounts WHERE apple_sub = $1
```

against a table that no longer has that column. That is branch 1 of `/auth/apple` — the
returning-user path every existing coach takes — so the failure is not confined to new
sign-ups.

**Reproduction.** One database, two releases, in the order a rollback happens:

```
# the new release boots, migrates through 0008, and works
POST /api/v1/auth/apple  (00c9469)                  -> 200
SELECT count(*) … column_name='password_hash'       -> 0

# roll the app back one commit; the schema stays where it is
POST /api/v1/auth/apple  (aceab37, returning user)  -> 500
  {"error":{"code":"INTERNAL","message":"An unexpected error occurred."}}
```

Nothing recovers that except restoring a database backup or rolling forward again. For
the fifteen minutes it takes to notice, every coach is locked out — and a rollback is
usually what you reach for *because* something else is already wrong.

This is the first destructive migration in the repo. `0005` deleted refresh-token rows,
which costs everyone one sign-in; `0006` and `0007` added a constraint and a column. A
column drop is a different shape, and the reason it is worth a finding rather than a note
is that the guard already in the file makes it look like the risk was considered.

**Fix.** Split it across two releases, which is the standard shape for a destructive
schema change and costs one extra deploy:

1. **Release N** removes password authentication and leaves the column alone. It stops
   *writing* `password_hash` — that change is already written; it is `CreateUserAccount`
   dropping the column from its insert. It does still *read* it, because sqlc expands
   `SELECT *` from the schema and the column is still in the schema; the value goes into
   a struct field nothing looks at. That is enough: the schema is untouched, so the
   previous release runs against it unchanged and this release is reversible.
2. **Release N+1** carries the drop, once N has been up long enough that rolling back
   past it is not something you would do.

Being precise about what this does and does not buy: N+1 is still a one-way door, because
N reads the column. Making N+1 reversible too would mean release N naming its columns
explicitly to omit `password_hash`, which in sqlc means a bespoke row type per query and
conversions at every call site — a lot of machinery for a release that exists for one
deploy. The trade taken here is that the release carrying all the behaviour and all the
risk is reversible, and the one that is not is a single migration shipped on its own.

If that is more ceremony than this service wants, the alternative is to accept it
knowingly: deploy `0008` on its own, at a time when a rollback would not be needed for
some other reason, and write down that the way back is a database restore. What should
not happen is discovering it during an incident. The branch is unpushed, so this is still
a free decision.

Worth stating for later: every future column drop has this property, and the two-release
split is the general answer, not a special case for this one.

---

## Low

**L1 — `/health` only proves the process is running.** `handleHealth` writes
`{"status":"ok"}` unconditionally; it never touches the pool. The OpenAPI spec labels it
"Liveness probe", which is the correct name for what it does — the gap is that nothing
answers the other question, and `/health` is the path a load balancer reaches for by
default.

```
# database up
GET /health       -> 200 {"status":"ok"}

# database dropped out from under the running process
GET /health       -> 200 {"status":"ok"}
POST /auth/apple  -> 500 {"error":{"code":"INTERNAL", …}}
```

A process that cannot reach Postgres keeps receiving traffic and fails every request,
with nothing in the platform's control loop able to tell. `database.Connect` pings at
boot, so the only case this misses is the database going away afterwards — a failover, a
rotated credential, an exhausted pool — which is exactly the case a readiness check
exists for. Add `pool.Ping` under a short context timeout, either behind a separate
`/ready` or inside `/health`, and say in the spec which one a balancer should use.

**L2 — a racing sign-in at one address gets a 500 instead of the 409.** The
concurrent-provisioning recheck added with the AUDIT-3 C1 fix asks `GetUserAccountByAppleSub`
who owns the subject, and signs the caller in when the answer is "the winner built your
account". When the collision is on the *address* rather than the subject, the winner holds
a different subject, the recheck finds nothing, and the raw unique-violation falls through
to `writeError`, which has no case for it:

```
4 concurrent POST /auth/apple, four subjects, one address:
  round 0: map[200:1 500:3]      … and identically in rounds 1-5

the same request made sequentially:
  POST /auth/apple (second subject) -> 409 code=EMAIL_ALREADY_REGISTERED
```

Not reachable through Apple: two Apple IDs do not share a verified address, and the
synthesized fallback address embeds the subject, so it cannot collide either. What makes
it worth writing down is what it is an instance of. AUDIT-3's M3, L1 and L2 were all one
defect — an unmapped error reaching the caller as a 500 where a typed answer existed —
and the fix for C1 put a fresh instance of it back three commits later. This is now the
only place in the codebase where a raw pgx error can reach `writeError` from a path that
already knows the right answer.

**Fix.** Route the unique violation to the answer the sequential path gives, before
falling through:

```go
_ = tx.Rollback(ctx)
if s.signedInToConcurrentlyProvisioned(w, r, identity) {
	return
}
if isUniqueViolation(err) {
	writeError(w, errEmailAlreadyRegistered())
	return
}
writeError(w, err)
```

---

## What is done well

- **The removal is the strongest thing in these four reports.** C1 and L5 were a
  containment and an accepted leak; deleting the endpoints made both disappear, and it
  was available only because somebody checked what the client actually called rather than
  assuming the API surface was load-bearing. Reading `BackendAPI.swift` before deciding
  is the whole reason that option was on the table.
- **`0008`'s guard.** Refusing to strand an account, at boot, with the count in the error
  and the remedy in the file — and the note that the guard only holds because
  `database.Migrate` wraps each file in a transaction, which is exactly the kind of
  dependency that is invisible until it fails. That the file misses the *other* stranding
  case (M1) does not diminish it.
- **`signedInToConcurrentlyProvisioned` asks the right question.** "Who owns this subject
  now" separates a race from an attack without weakening C2's refusal, because a squatted
  Person row has no account behind it. The reasoning is written down where the next reader
  will need it.
- **The seed still signs in.** Removing password auth could easily have left `make seed`
  producing data nobody could reach; instead it plants an Apple subject and prints how to
  use it.

---

## Suggested order of work

1. **M1** — decide before pushing, because it is free now and expensive later. Splitting
   into two releases is the safe answer; accepting it deliberately is a fine answer;
   discovering it during an incident is not.
2. **L2** — three lines, and it closes the last of the unmapped-error class.
3. **L1** — a readiness check, whenever the deployment story gets attention.
4. Unchanged from AUDIT-3: the AUDIT-2 leftovers (its M1 sync page limit and L1–L6) as
   ordinary hardening, with L4's token reaping now worth more than its severity suggests,
   and **P2–P4** alongside the club feature.
