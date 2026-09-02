# SoccerKit API — fifth audit

Audit of `monsoudz/soccerkit-api` at `1c7be59`: the three fixes from
[`docs/AUDIT-4.md`](AUDIT-4.md), the two-release split of the `password_hash` drop, the
sync pull page limit that closed AUDIT-2's M1, and the iOS contract tests.

Narrow again, and pointed at the same place the work was. Six of the seven commits since
AUDIT-4 touch sync or the deploy sequence, so this looks hardest at the new paging code
and at what a cursor now means, rather than sweeping the codebase a fifth time. Three
findings, all reproduced against a live server. None is in the class the first three
reports were about — nobody's data is reachable by anyone who should not have it — but
two of them lose or re-read a coach's data, which is the class this one is about.

Baseline health: `go vet`, `go build` and `go test ./...` pass clean, `sqlc generate`
produces no drift, and the repro tests were run and then removed; the tree is unmodified.

> **Note on the baseline.** The test database refused to migrate on the first run:
> `0008_require_apple_identity.sql` raised its exception over one leftover `roles@e.com`
> row from a pre-removal test fixture. That is the guard doing exactly what it was written
> to do, on the first database it ever met that had a stranded account in it. Recorded
> because it is the only unprompted evidence in these five reports of a guard firing in
> anger, and because it means the check has now been exercised rather than merely
> reasoned about.

---

## Round four: everything holds

All three AUDIT-4 findings are fixed, each with a regression test that names the defect
it pins, and all pass:

| Fix | Still true |
|---|---|
| M1 irreversible `0008` | Split into `0008` (check-only) and `0009` (the drop), shipped a release apart. The rollback story is written into both files |
| L1 `/health` says nothing | `/ready` pings the pool under its own 2s timeout; the spec now says which probe answers which question |
| L2 racing address collision 500s | `isUniqueViolation` → 409 `EMAIL_ALREADY_REGISTERED`, and `TestConcurrentSignInsAtOneAddressStayATypedConflict` holds it there |

AUDIT-2's **M1** (`GET /sync` has no page limit) is closed — that is what `2187a62` did,
and the paging is correct: `TestSyncPullIsPagedAndLosesNothing` drains 1200 records and
gets each exactly once, which is the property that matters. **M1 below is not a
reopening of it.** The delta is bounded now. What is not bounded is the read that
produces it.

Still open and unchanged from AUDIT-2: **L1** (`/docs` loads `swagger-ui-dist@5` from
unpkg — a floating major, no SRI), **L2** (`select` answers unchecked against options),
**L3** (`PATCH /games/{id}` cannot clear `kickoffAt` — `kickoff_at = COALESCE(narg, kickoff_at)`
overloads NULL as "leave alone", so there is no value that means "clear it"; verified
still true, and verified it does *not* also wipe the field on an unrelated PATCH),
**L4** (refresh tokens never reaped), **L5** (the `conflicts` existence oracle), **L6**
(`sync_documents.type` unbounded), and **P2–P4**, which still wait on the club/invite
feature. `CreateMembership` still has three call sites and none puts a second
account-holding person in an existing organization, so the precondition still holds.

AUDIT-4 flagged L4 as worth more than its severity now that a refresh token is the only
credential this service stores. `0009` has since dropped `password_hash`, so that is now
literally true: after this release the only secret in the database is a refresh token
hash.

---

## Summary of new findings

| # | Severity | Finding |
|---|----------|---------|
| M1 | Medium | A sync pull reads up to 500 full payloads to return 2 MiB, and above ~4.2 KiB per record a full drain becomes quadratic |
| M2 | Medium | A cursor this server never issued is treated as authoritative, so a database restore silently drops a window of records per device |
| L1 | Low | Tombstoned rows in the seven projected tables keep their payload and their PII forever |

---

## Medium

### M1 — the page bounds the response, not the read (confirmed)

> **Status.** All three parts of the fix have shipped, in the two commits following this
> one. Left in the present tense as a record of what was wrong.
>
> Part (3), the per-record payload cap, landed at **256 KiB**, with a 32 KiB watch-log so
> the real size distribution becomes visible without rejecting anything. That bounds one
> page's read to `maxSyncPage × 256 KiB` = 128 MiB and the over-scan ratio to ~62×,
> against ~2 GiB and ~1000× before. It is deliberately generous: an offline-first client
> retries the batch it failed to push, so a cap that rejects a record a coach already has
> on their phone stops that device syncing, and too low is a worse failure than the one it
> prevents. **Tightening toward 16–32 KiB is what would make the drain close to linear,
> and that still wants `pg_column_size(payload)` percentiles over a real database first.**
>
> One consequence worth knowing: the cap makes an oversized *stored* record unreachable
> through the API, so `TestSyncPullAlwaysMakesProgress` now plants its heavy row rather
> than pushing it. The oversized-first-row rule still has to hold, because rows written
> before the cap existed are still in the table and a page that cannot get past one is a
> device that never syncs again.
>
> The allocation blowup is closed. Allocation is now flat in the payload size instead of
> growing with it, and the delivered counts are unchanged, so this costs nothing on the
> wire:
>
> | payload | delivered | allocated before | after |
> |---|---|---|---|
> | 4 KiB | 500 | 20.1 MiB | 20.1 MiB |
> | 64 KiB | 31 | 52.1 MiB | 18.9 MiB |
> | 256 KiB | 7 | 145.4 MiB | 18.1 MiB |
> | 1 MiB | 1 | 513.3 MiB | 10.1 MiB |
>
> A page of 200 tombstones holding 50 MiB of payload between them now allocates 0.3 MiB
> and weighs 6 kB on the wire, and arrives in **one** page rather than being cut off after
> a handful — pinned by
> `TestSyncPullDoesNotSpendTheByteBudgetOnTombstones`.
>
> **The quadratic drain is not fixed and part (3) is what fixes it.** Above ~4.2 KiB per
> record the byte bound still binds before the row bound, so a page still delivers fewer
> rows than the window it scans, and draining *N* records still costs about *N²/2r* row
> reads. What changed is that those rows no longer cross the wire or land on the Go heap
> — which was the part that could take the process down.
>
> One implementation note worth keeping, because it will look like noise otherwise: the
> `::bigint` cast on `max_bytes` in the query is load-bearing. sqlc infers a parameter's
> type from the column it is compared against and cannot resolve one coming out of a
> derived table that carries window functions; without the cast, generation fails with
> `table alias "budgeted" does not exist`. The cast states the type outright.

`ListSyncChangesSince` takes `LIMIT 500` (`maxSyncPage`). `handleSyncPull` then walks the
returned rows and stops at 2 MiB (`maxSyncPageBytes`). Both limits are deliberate and the
comment explains why both are needed. The gap is *where* the second one is applied: the
byte cut happens in Go, after all 500 rows have been read off the wire and materialized.

The two caps are only in balance at one payload size — `maxSyncPageBytes / maxSyncPage`
= 2 MiB / 500 = **~4.2 KiB per record**. Below it the row cap binds and everything is
fine. Above it the byte cap binds first, and every page reads 500 rows to deliver fewer
than 500 — the rest is decoded, allocated, and dropped.

**Reproduction.** 500 records planted for one account, pulled with `since=0`, measuring
what the process allocated against what the client received, then drained to the end:

```
payload    4 KiB | delivered 500 rec/page | response 1.98 MiB | allocated  20.1 MiB | drain   2 pages,    ~500 rows read
payload   64 KiB | delivered  31 rec/page | response 1.94 MiB | allocated  52.1 MiB | drain  18 pages,  ~4,284 rows read
payload  256 KiB | delivered   7 rec/page | response 1.75 MiB | allocated 145.4 MiB | drain  73 pages, ~18,108 rows read
payload    1 MiB | delivered   1 rec/page | response 1.00 MiB | allocated 513.3 MiB | drain 500 pages, ~125,000 rows read
```

At 1 MiB payloads a single pull allocates **513 MiB to return one record** — 513× more
than it sends. Nothing caps a record's size except `maxBodyBytes`, so a payload may be
just under 4 MiB; 500 × 4 MiB is the worst case a single `GET /sync` can ask the process
to hold.

The drain cost is the other half. Delivering *r* records per page out of *N* means *N/r*
pages, each scanning up to 500 rows of the remaining tail — so total rows read grows as
*N²/2r*. For the 1 MiB account that is ~125,000 row reads, ~125 GiB moved, to deliver
500 MiB. Before `2187a62` the same delta was one unbounded read of 500 MiB. So for this
shape of data the change traded a bounded response for an unbounded *number* of reads,
and total work went up rather than down. That is worth stating plainly because it is the
opposite of what the commit set out to do, and it is invisible at the payload sizes the
tests use.

Two things make this reachable rather than theoretical. Nothing rejects a large record at
push time. And a tombstone in a projected table still carries its payload (L1 below), so
a page of deletes spends the whole byte budget on payloads that are never sent — the
response for one deleted 256 KiB Team is 100 bytes, and the query read 262,171 of them to
build it.

**Fix.** No single change closes both halves; they want different things.

1. **Stop shipping payloads that will not be sent.** Free, and it fixes the delete case
   outright — select `NULL` for tombstoned rows, since the response only ever carries
   `{type, id}` for them.
2. **Make the byte cut in SQL**, so the discarded rows never cross the wire:

   ```sql
   WITH page AS (
       SELECT delta.*,
              row_number() OVER (ORDER BY delta.seq)                            AS rn,
              sum(pg_column_size(delta.payload)) OVER (ORDER BY delta.seq)      AS running
         FROM ( ...the existing union... ) delta
        ORDER BY delta.seq
        LIMIT sqlc.arg('lim')
   )
   SELECT type, id, CASE WHEN deleted THEN NULL ELSE payload END, deleted, seq
     FROM page
    WHERE rn = 1 OR running <= sqlc.arg('max_bytes');
   ```

   `rn = 1` keeps the existing and correct rule that an oversized first record is
   returned alone rather than stalling the cursor. This kills the allocation blowup. It
   does not fix the quadratic drain: Postgres still reads 500 rows per page.
3. **Cap a record's payload at push time** — the piece that actually bounds the worst
   case. With a cap of *c*, the row limit and the byte budget stay in the same
   neighbourhood, pages come back full, and the drain stays linear. This is the only one
   of the three that is a wire change, so it needs the app to agree and it needs a chosen
   number; 256 KiB would keep the worst-case read at 128 MiB and still be far above any
   record the app writes today. Worth measuring the real distribution of
   `pg_column_size(payload)` before picking it.

(1) and (2) are local to this query and safe to ship now. (3) is the one to decide
deliberately, and the number should come from data rather than from this report.

---

### M2 — a cursor this server never issued is taken at its word (confirmed)

> **Status.** Fixed in the commit following this one, and the reproduction was re-run
> against the fixed code. Left in the present tense as a record of what was wrong.
>
> **What shipped differs from what this section recommends, in one deliberate way.** The
> fix below proposes a 400 for any cursor the server could not have issued. That is right
> for an unparseable or negative cursor — only a client bug produces one — and that is
> what ships. It is wrong for a cursor *ahead of the sequence*, which is not the client's
> fault at all: it is the fingerprint of a restore, and answering it with an error would
> stop sync dead on every device in the field until the iOS app learned to handle a new
> error. Those cursors are instead rewound to 0 and resynced, and logged server-side with
> the remedy in the message. Devices converge on their own, with no app change.
>
> **And the check is narrower than this section claims.** It compares the cursor against
> `sync_seq`, so it can only recognise a stale cursor *while the sequence is still below
> it*. Once post-restore writes carry the sequence back over a device's cursor, that
> cursor is indistinguishable from a legitimate one and a device reconnecting after that
> point still skips the window silently. The server cannot do better unaided — a restore
> rewinds every record of what was issued along with the data, leaving nothing to compare
> against. `TestSyncCannotSeeAStaleCursorOnceTheSequenceCatchesUp` pins that edge so it is
> not mistaken for a closed hole.
>
> **So the restore runbook now carries the other half, and it is the cheaper half:**
> after restoring, set `sync_seq` above the pre-restore high-water mark. Every cursor in
> the field is then below the sequence again, new writes land above all of them, and no
> device skips anything — no code and no wire change. The bounds check is what catches
> the restore where nobody did that.

`handleSyncPull` seeds its high-water mark with the cursor the client sent
(`high := since`) and raises it only over rows it actually delivered. That rule is right,
and AUDIT-4 and the paging work both lean on it. What is missing is any check that the
cursor was one this server could have issued.

Two consequences, both silent:

```
since=abc            -> 200, 3 records, cursor "3"           # unparseable: full resync, no error
since=1e3            -> 200, 3 records, cursor "3"           # same
since=-1             -> 200, 3 records, cursor "3"           # same
since=99999999999    -> 200, 0 records, cursor "99999999999" # past the sequence: echoed back
```

An unparseable cursor becomes 0 and triggers a full resync — which, after M1, is not the
cheap accident it used to be. A cursor past the end of `sync_seq` comes back *unchanged*
alongside an empty page, and that pair is precisely the client's drain-stop condition: a
cursor that did not move means there is nothing more. The client concludes it is up to
date.

**How a device gets a cursor ahead of the sequence: the recovery path `0009` documents in
its own header — "the way back is a database restore."** A restore rewinds `sync_seq` to
the backup's position while every device in the field still holds a cursor from after it.

**Reproduction.** One account, one device, a restore to `seq 3`:

```
device syncs a,b,c,d,e,f and stores cursor "6"

restore: rows above seq 3 are gone, sync_seq restarts at 4, the device is untouched

coach keeps working    -> g,h,i take seq 4,5,6
pull(since=6)          -> 200, 0 records, cursor "6"    # "you are up to date"
coach keeps working    -> j,k take seq 7,8
pull(since=6)          -> 200, 2 records, cursor "8"    # resumes as if nothing happened

exists on the server (a device installing today): a,b,c,g,h,i,j,k   (8 records)
this device ever received:                        a,b,c,d,e,f,j,k   (8 records)
never delivered to this device:                   g,h,i
```

The device is not stuck — it recovers the moment the sequence passes its cursor, which is
what makes this bad. It steps over the window silently and then syncs normally forever
after, so nothing will ever surface the gap. It is simultaneously **missing g,h,i**,
which exist, and **still holding d,e,f**, which the restore removed. Every pull returned
200. Every device in the fleet loses a *different* window, because each holds a different
cursor, and the divergence is permanent short of a reinstall.

This is not exotic. `0009` is a one-way door whose documented remedy is a restore, and
AUDIT-4 accepted that trade knowingly. The trade is still the right one — but the cost
was recorded as "restore the database", and the actual cost is "restore the database, and
every device that had synced past the backup point silently loses whatever was written
into the seqs it had already passed".

**Fix.** The cheap, correct half is to stop confirming a cursor the sequence cannot
account for. `last_value` of `sync_seq` is one cheap query:

```go
// A cursor past the end of the sequence is not a position this server ever issued.
// Answering "you are up to date" to it is how a restored database silently strands
// every device that had synced past the restore point.
if since > s.currentSyncSeq(ctx) {
    writeError(w, errValidation(
        "cursor is ahead of this server's sync sequence; resync from 0"))
    return
}
```

A 400 that names the remedy turns a silent per-device gap into one visible error the
client can act on, and the app already has the from-zero path. The same guard makes the
unparseable case honest: reject it rather than quietly resyncing.

The thorough half, if sync is ever load-bearing enough to want it, is an epoch — a value
stamped alongside the cursor that changes when the database is restored, so a stale
cursor is rejected by construction rather than by comparison. That is a wire change and a
bigger decision; the bounds check is most of the value for none of the cost.

Worth writing down for whoever runs the restore: **after any restore, every client cursor
above the restored `last_value` is invalid.** With the guard above they are told. Without
it, they are not.

---

## Low

> **Status.** Fixed in the commit following this one, on both halves: the tombstone
> statements now clear, and `0010_scrub_tombstoned_rows.sql` backfills the rows already
> tombstoned — which is the half that actually removes data somebody has already asked to
> be rid of. The REST tombstone paths (`DeleteTeam`, `DeleteSession`) clear too, so which
> endpoint a coach deleted through does not decide whether the data stays.
>
> The rule it settled on is **a tombstone clears exactly what its upsert sets**. That is
> what keeps it reversible — re-pushing the record restores every cleared field, because
> they are the same fields — and it is why `persons.email`, `phone`, `birthdate` and the
> given/family names are left alone: REST owns those, `SyncUpsertPerson` never writes
> them, and clearing what nothing would put back turns a sync delete into permanent loss
> of data it does not own. The four `NOT NULL` display columns take `''` rather than NULL.
>
> The backfill deliberately does not touch `seq`. Every one of those rows has already
> been delivered to every device as a tombstone; bumping `seq` would re-deliver all of
> them at once, which for a long-lived account is the largest pull it has ever made, for
> no change any client can observe. Verified against a planted legacy row: content
> cleared, `seq` unchanged.
>
> One thing this trades, stated where it will be found: the server can no longer
> reconstruct a record from a delete it has applied. No read path returned a tombstoned
> row and there is no undelete endpoint, so nothing loses a capability it had — but a
> mistaken delete is now the client's to recover from.

**L1 — a tombstone keeps everything it was.** `SyncTombstoneDocument` nulls the payload
when it tombstones a row. The seven projected tables do not: `SyncTombstoneTeam`,
`SyncTombstonePerson` and their siblings set `deleted = true` and a fresh `seq`, and leave
every column alone.

```
Team tombstoned via POST /sync:
  teams row     -> deleted=true, payload still 262,171 bytes
  pull response -> 100 bytes    (a delete carries only {type, id})

Note tombstoned via POST /sync:
  sync_documents row -> payload 0 bytes   (SyncTombstoneDocument sets it NULL)
```

Two costs. The one M1 cares about is that those bytes are read and charged against the
page's byte budget to produce a 100-byte answer. The one that outlives this report is
`persons`: `SyncTombstonePerson` retains `display_name`, `emergency_contact_name`,
`emergency_contact_phone`, `medical_notes` and the full payload, indefinitely, with
nothing that ever removes them. A coach who deletes an athlete from the app has not
deleted that athlete's medical notes or their emergency contact's phone number — those
sit in the database for as long as the row does, and no code path revisits them.

Reachability is *not* the problem, and it is worth saying so: `visiblePersonFromPath`
goes through `PersonVisibleInOrg`, which checks `p.deleted = false`, so a tombstoned
Person is correctly a 404 over REST. `GetPerson` is the one persons query with no
`deleted` filter and that is right — `handleGetMe` needs your own row regardless. This is
retention, not exposure.

**Fix.** Null the payload and the PII columns when tombstoning, the way
`SyncTombstoneDocument` already does. A tombstone needs `id`, `deleted` and `seq` to do
its job; it does not need the medical notes. `sync_documents` is the precedent and the
seven projected tables should match it.

---

## What is done well

- **The contract tests are the most valuable thing in this round.** They pin the one
  boundary nothing else could see — two repos with no linkage, kept in step by hand —
  and they are written against the *app's* types rather than this package's, which is the
  detail that makes them work. Pinning that Swift writes `Date` as a Double, and that the
  prefs id is the literal string `"prefs"` rather than a UUID, are both the kind of thing
  that is obvious once broken and invisible before.
- **`1c7be59` reasons about the option it did not take.** The commit spends most of its
  length on why *advancing* the cursor on push loses data silently and cross-device, which
  is the fix a reader would reach for first. Writing down the rejected option is what
  stops it being re-proposed in six months.
- **The two-release split shipped as described.** `0008` became a check, `0009` carries
  one statement, and both files explain the rollback story rather than assuming the next
  reader reconstructs it. The `IF EXISTS` in `0009` covering databases that ran the
  earlier build of the set is the kind of detail usually discovered in production.
- **The paging correctness tests test the right property.** Not the page size — the
  invariant that draining yields every record exactly once, which is what a page boundary
  can actually break. `TestSyncPullAlwaysMakesProgress` covers the oversized-record stall
  that a byte cap invites.
- **`0008`'s guard has now fired in anger**, on this audit's own test database, and the
  error said what was wrong and how to fix it.

---

## Suggested order of work

1. ~~**M2's bounds check**~~ — done, with the two deliberate departures recorded in the
   status note under M2: an ahead-of-sequence cursor resyncs rather than erroring, and
   the check only catches a stranded device while the sequence is still below its cursor.
   **The remaining half is a runbook line, not code: after any restore, bump `sync_seq`
   past the pre-restore high-water mark.** That is what makes every cursor in the field
   valid again, and it should be written into the restore procedure `0009` points at.
2. ~~**M1 (1) and (2)**~~ — done; see the status note under M1. Allocation is flat in the
   payload size now, and a page of deletes costs what a delete weighs.
3. ~~**L1**~~ — done, including the backfill for rows already tombstoned; see the status
   note under L1.
4. ~~**M1 (3), the per-record payload cap**~~ — shipped at **256 KiB**, with a 32 KiB
   watch-log so the real distribution becomes visible. That bounds one page's read to
   128 MiB and the over-scan ratio to ~62x, against ~2 GiB and ~1000x before. It does
   **not** make the drain linear; tightening toward 16–32 KiB would, and that still wants
   `pg_column_size(payload)` percentiles over a real database first. The cap was set
   generously on purpose: an offline-first client retries the batch it failed to push, so
   a cap that rejects a record a coach already has on their phone stops that device
   syncing, and too low is a worse failure than the one it prevents.
5. ~~The AUDIT-2 leftovers~~ — **L1, L2, L3, L4 and L6 are done** (see AUDIT-2's own
   status notes). **L5 is closed as a knowing trade** rather than fixed, with the
   reasoning recorded above `handleSyncPush`: the conflict *is* the feature, and the only
   real fix is re-keying seven tables on `(sync_account_id, id)`, which is out of all
   proportion to 122 bits of UUID entropy. **P2–P4** still wait on the club feature and
   their precondition still holds.
