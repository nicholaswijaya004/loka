## Day 2 — Reproduce Problem

**Goal:** Understand data races; build the smallest version of the double-booking bug.

### Counter — 1,000,000 increments

| Variant | Expected  | Observed                    | Result                                      |
| ------- | --------- | --------------------------- | ------------------------------------------- |
| Unsafe  | 1,000,000 | 947,231 / 951,880 / 943,102 | ~5% of increments lost, different every run |
| Mutex   | 1,000,000 | 1,000,000                   | Exact, every run                            |

Benchmark under `-cpu=8`: mutex `X ns/op` vs atomic `Y ns/op` <!-- TODO: fill in -->

### Inventory — 10 seats, 500 concurrent attempts

| Variant | Booked | Available | Result          |
| ------- | ------ | --------- | --------------- |
| Unsafe  | 12     | −2        | Overbooked by 2 |
| Safe    | 10     | 0         | Invariant held  |

```text
=== RUN   TestUnsafeInventoryOverbooks
    inventory_test.go:29: seats=10 succeeded=12 sold=12 available=-2 overbooked=2
    inventory_test.go:31: invariant available+sold == seats? true (-2 + 12 = 10)
--- PASS: TestUnsafeInventoryOverbooks (0.00s)
PASS
ok
```

### Understood

- `count++` is load/add/store, not one instruction — races live in the gap.
- Atomics fix single-value ops but can't span "check then act".
- Mutex throughput degrades as `-cpu` rises; that's the cost of exclusivity.

### Tomorrow

Data model + migrations. The lock moves into Postgres.

## Day 3 — Database Layer

**Goal:** Understand and create database on loka repository that will support the simulation of booking

## Query Verification

Following query will help to verify the availability of the inventory

- Query:

```sql
INSERT INTO inventory_units (name, available_units, total_units, currency, price_minor)
VALUES ('Deluxe Cabin', 10, 10, 'IDR', 150000000);
UPDATE inventory_units SET available_units = -1;
UPDATE inventory_units SET available_units = 50;
```

### Results

```text
ERROR:  new row for relation "inventory_units" violates check constraint "chk_availability"
DETAIL:  Failing row contains (4f5cc513-9559-4c95-880e-2579c910942e, Deluxe Cabin, null, -1, 10, IDR, 150000000, 1, 2026-09-07 16:15:54.910336+00, 2026-09-07 16:15:54.910336+00, 0).
loka=# UPDATE inventory_units SET available_units = 50;
ERROR:  new row for relation "inventory_units" violates check constraint "chk_availability"
DETAIL:  Failing row contains (4f5cc513-9559-4c95-880e-2579c910942e, Deluxe Cabin, null, 50, 10, IDR, 150000000, 1, 2026-09-07 16:15:54.910336+00, 2026-09-07 16:15:54.910336+00, 0).
```

## Day 4 — Naive Booking

Built the naive booking endpoint. Sequential behaviour is fully correct:
201 with computed total, availability 10 → 9, 409 when sold out, 404 on
unknown unit, 400 on bad input.

The flow is deliberately unsafe — four separate round trips, no transaction:
GetInventoryUnit → check availability → DecrementAvailability → InsertBooking
The gap between the check and the decrement is the same check-then-act race
as Day 2's UnsafeInventory, now at database scale.

**Prediction for Day 5** (500 concurrent requests against the 1-seat unit):

- successful bookings: \_\_\_
- constraint violations from chk_availability: \_\_\_
- bookings created vs seats actually sold: \_\_\_
  Unlike Day 2, available_units cannot go negative — the CHECK constraint
  blocks it. So I expect failures rather than corruption.

### Day 5 — Concurrency baseline (500 VUs, 10 seats, pool=50)

| Strategy                               | Bookings | available_units | Invariant   | Overbooked |
| -------------------------------------- | -------- | --------------- | ----------- | ---------- |
| `SET available = available - $1` (SQL) | 10       | 0               | 0+10=10 ✓   | 0          |
| `SET available = $1` (Go-computed)     | 500      | 8               | 8+500=508 ✗ | 490        |

Both run against identical code paths apart from one SQL statement.
The CHECK constraint fired 490 times in the safe run and zero times in
the unsafe run — an out-of-date value written absolutely stays within
legal bounds, so the constraint has nothing to reject.

## Day 6 — Idempotency

**Before:**
| | Sequential (2 identical requests) | Concurrent (500 VUs, one key) |
|---|---|---|
| bookings | 2 | 10 |
| available_units | 10 → 8 | 10 → 0 |

**After:**
| | Sequential | Concurrent |
|---|---|---|
| bookings | 1 | 1 |
| available_units | 10 → 9 | 10 → 9 |
| retry response | 200 + Idempotent-Replay, same booking id | 409 in-flight |

500 concurrent retries of one request now consume exactly one seat.

**Mechanism:** the claim is an INSERT, not a SELECT-then-INSERT. Postgres'
primary key lets exactly one through; the other 499 receive SQLSTATE 23505
and learn they lost. No application-level coordination is involved — the
same property that made the CHECK constraint hold on day 5.

**Split observed:** 499 conflicts, 0 replays — the winner was still in
flight when the burst arrived. The sequential test covers the replay path.

Service package coverage: 100% of statements.

## Day 8 — Transaction

**Goal:** Make the booking flow atomic. Day 5 showed the _decrement_ is safe on
its own; the _flow_ is not — decrement and insert were separate statements, each
committing independently.

**The gap, measured before the fix (macOS, Docker Desktop, 500 VUs, 10 seats):**

| Bookings | available_units | Invariant   | Seats lost |
| -------- | --------------- | ----------- | ---------- |
| 8        | 0               | 0 + 8 = 8 ✗ | **2**      |

Two requests decremented availability and then failed to insert. The decrement
had already committed as its own statement, so the seats were consumed with no
booking to show for them — unrecoverable without manual intervention.

**After wrapping the decrement and insert in one transaction:**

| Scenario                                 | Bookings | available_units | Invariant     | Errors |
| ---------------------------------------- | -------- | --------------- | ------------- | ------ |
| No injected failures                     | 10       | 0               | 0 + 10 = 10 ✓ | 0      |
| 30% failure injected after the decrement | 10       | 0               | 0 + 10 = 10 ✓ | 5      |

The second row is the demonstration. `FAIL_AFTER_DECREMENT=0.3` makes
`InsertBooking` fail _after_ the decrement has executed. All five rolled back:
the seats were returned and the requests failed cleanly. Before the transaction,
the same injection consumed seats permanently.

Idempotency still holds through the transactional path — 500 concurrent requests
sharing one key produced 1 booking and 499 conflicts, unchanged from day 6.

**The design problem.** The service needs to say "run these operations
together," so `booking.Store` needed a transaction method. But
`storage.Store.WithTx` takes `func(*storage.Store) error` — a concrete type —
and putting that in the interface would leak the implementation the interface
exists to hide.

Solved with an adapter at the composition root:

    type storeAdapter struct{ *storage.Store }

    func (a storeAdapter) WithTx(ctx context.Context, fn func(booking.Store) error) error {
        return a.Store.WithTx(ctx, func(tx *storage.Store) error {
            return fn(storeAdapter{Store: tx})
        })
    }

Struct embedding promotes every `*storage.Store` method automatically, so the
adapter only has to supply the one method whose signature differs. `storage`
never imports `booking`; `booking` never names `*storage.Store`. The only file
aware of both is `main.go`, which is where wiring belongs.

The re-wrapping in the closure matters: passing the bare `tx` would hand `fn`
something that does not satisfy `booking.Store`, since `*storage.Store` has no
correctly-shaped `WithTx` of its own. Wrapping it also means a nested `WithTx`
call reaches the type assertion and fails with a clear error rather than
silently doing the wrong thing.

**Understood:**

- `Begin` checks out a pool connection and holds it for the whole transaction,
  not per statement. That is where the latency cost comes from, and it is why
  the payment call in week 4 must sit _outside_ the transaction — a network
  round trip to a provider inside one would hold a connection for as long as the
  provider takes to answer.
- `defer tx.Rollback(ctx)` immediately after `Begin` is the only safe shape. My
  first attempt rolled back only when `Commit` failed, which leaks a connection
  on every `fn` error — and day 5 had 490 sold-out responses per run. Rollback
  after a successful commit returns `pgx.ErrTxClosed` and does nothing, so the
  unconditional defer is safe.
- The `DBTX` interface from day 4 is what made this a small change. Every
  storage method already accepted either a pool or a transaction, so none of
  them needed touching.

**Cost me time:**

- Failure injection hardcoded at 0.3 with no env guard, so three "clean" runs
  were silently polluted before I found it. Now opt-in via
  `FAIL_AFTER_DECREMENT`, defaulting to 0.
- Forgot `make reset` between runs twice; stale availability made a retry test
  look broken when it was not.
- Switched machines mid-project again — Docker, `golang-migrate` and a C
  toolchain all had to be installed on the Mac, and the earlier latency
  baselines are from a different host.

**Tomorrow:** `SELECT FOR UPDATE` as the second of four concurrency strategies,
measured on cost rather than correctness.

## Day 9

**Goal:** Implement `SELECT FOR UPDATE` as the second concurrency strategy and
measure what it costs. Since day 5 the question is no longer which approach is
correct — the single-statement decrement plus a CHECK constraint already
prevents overselling — but what each approach costs to get there.

**First, made the measurements reproducible.** Every benchmark this week needed
a manual reset, a killed process, or an env-var check, and three "clean" runs
on day 8 were silently polluted by a hardcoded failure-injection probability.
`scripts/bench.sh` now does the whole cycle in one command: kill anything on the
port, `make reset`, start the server with whatever env the caller passed, poll
`/healthz`, run k6, verify `available_units + count(bookings) == total_units`
directly against the database, and kill the server via a `trap` so cleanup
happens even on failure or Ctrl-C.

That was an hour well spent. Every number below came out of one command.

**Prediction before measuring:** at high contention the lock would be close to
free, because with one seat and 500 requests everyone is queueing anyway.

**Result:**

| Contention | Seats | single-statement p95 | `FOR UPDATE` p95 | Ratio     |
| ---------- | ----- | -------------------- | ---------------- | --------- |
| High       | 1     | 311 ms               | 4.26 s           | **13.7×** |
| Medium     | 10    | 619 ms               | 5.23 s           | **8.5×**  |
| Low        | 50    | 873 ms               | 5.60 s           | **6.4×**  |

Throughput at medium contention fell from 720 req/s to 82 req/s.

**The prediction was wrong, and the reason is the interesting part.** Without
the lock, the losers _do not queue_. A sold-out request reads availability
outside any transaction, fails the check in Go, and returns — it never acquires
a lock and never waits for one. That is 490 of 500 requests taking a path that
costs almost nothing.

`FOR UPDATE` removes that fast path. Every request must take the row lock before
it can even find out whether it is sold out, so all 500 serialise through a
single row. Pessimistic locking makes every reader pay for exclusivity that only
the ten eventual writers needed.

Note the ratio _shrinks_ as contention falls: 13.7× at one seat, 6.4× at fifty.
With more seats a larger share of requests are genuine writers who would have to
serialise anyway, so the lock's overhead is proportionally smaller.

**Limit of the finding.** This workload is 98% rejections. A workload where most
requests succeed would narrow the gap considerably, because the single-statement
strategy's advantage comes entirely from letting losers exit early. Worth
re-measuring with a larger inventory before generalising.

**Understood:**

- `FOR UPDATE` outside a transaction is useless. The lock is released the moment
  the implicit single-statement transaction commits, so it acquires and drops it
  in the same breath. Silent, and easy to get wrong — documented on the method.
- The minimum latency stayed low (182–248 ms) in every `FOR UPDATE` run. The
  first request through is fast; everyone behind accumulates the wait. Textbook
  queueing behaviour, visible in the gap between min and p95.
- Both strategies had to move the read _to different places_: single-statement
  reads outside the transaction and relies on the decrement being safe on its
  own; `FOR UPDATE` reads inside, because the whole point is that the value
  cannot change between reading it and acting on it.

**Cost me time:**

- A `:=` inside the `WithTx` closure shadowed the outer `booking` variable, so
  the function returned nil with no error. Legal Go, invisible to `go vet`, and
  it would have panicked in the handler. The `shadow` analyzer would have caught
  it; it is not in my golangci-lint config.
- `p95` for the same code varied 344–843 ms on the single-statement strategy and
  3.62–5.46 s on `FOR UPDATE`. Single runs on a laptop under Docker Desktop are
  not measurements. Medians of three from here on, with the range recorded.

**Tomorrow:** optimistic locking with the `version` column, and the retry rate
as contention rises.

## Day 10

**Goal:** Implement optimistic locking (version column + retry-on-conflict) as
the third concurrency strategy, and measure the retry rate as contention
rises — continuing from Day 9's single-statement and `FOR UPDATE` baselines.

**Prediction before measuring:** a version-based compare-and-swap should behave
like single-statement's losers — a cheap failure that doesn't queue — so at low
contention optimistic should land closer to single-statement than to `FOR
UPDATE`.

**Result — first pass, `maxOptimisticRetries = 5`, 50 seats / 500 VUs:**

| Metric           | Value      |
| ---------------- | ---------- |
| Bookings created | 9          |
| Failures         | 491 of 500 |
| `checks_failed`  | 98.20%     |
| p95              | 2.19 s     |
| Invariant script | passed     |

**The invariant passing was the trap.** It only checks `available + booked =
total`, which holds whether 9 or 50 bookings actually succeeded — it can't
distinguish "sold out" from "gave up." `bookings_created: 9` next to
`bookings_error: 491` is what exposed it: every one of the 491 losers hit
`maxOptimisticRetries` before ever landing a clean write, not because the
inventory ran out.

Added exponential backoff with full jitter (10ms → 20ms → 40ms → 80ms window,
randomised, cancellable via `ctx.Done()`) on the theory that lockstep retries
were recreating the same collision. Re-ran: still 491 failures, unchanged.
That ruled out synchronisation as the cause. Jitter controls _when_ contenders
retry, not _how many times_ they're allowed to — with 500 requests racing for
50 slots, 5 attempts was never enough regardless of spacing.

Raised `maxOptimisticRetries` to 50 and re-ran:

| Metric                | Value                      |
| --------------------- | -------------------------- |
| Bookings created      | 50                         |
| Sold out (legitimate) | 450                        |
| `checks_failed`       | 0%                         |
| p95, all requests     | 5.63 s                     |
| p95, winners only     | 3.07 s                     |
| Total retries logged  | 2,476 (~4.95 mean/request) |

Zero failures, and the invariant now genuinely holds. Cost: p95 landed within a
hair of `FOR UPDATE`'s 5.60s at the same contention level — tuned correctly,
optimistic isn't the cheap alternative it looked like at `retries=5`. It
converges to roughly the same price as blocking.

**The actual finding wasn't the failure count — it was the shape of the cost.**
Split by outcome, the 50 winners averaged p95=3.07s; the 450 losers pulled the
overall figure to 5.63s. Losing costs _more_ than winning. That inverts
single-statement, where losing was nearly free (a fast rejection before any
lock), and differs from `FOR UPDATE`, where everyone pays the same queueing
cost regardless of outcome. Under tuned optimistic locking, a request that's
told "sold out" pays for that answer by retrying and re-reading until it
personally observes availability hit zero.

**Limit of the finding.** Only 50 seats (low contention) has been re-run with
the tuned budget and real script output. 1 and 10 seats need the same
treatment — retries=50, split failure counters, median of three — before the
comparison table is trustworthy across all three contention levels.

**Understood:**

- A passing invariant check is not the same as a working system.
  `available + booked = total` is silent on _how_ that total was reached.
  Splitting `bookings_created` from `bookings_error` is what actually caught
  the bug.
- Backoff and jitter fix lockstep collision, not an undersized retry budget.
  They look identical from the failure count alone until you isolate them —
  adding jitter and seeing zero change is itself the isolation.
- A retry budget has to be sized to the real contention ratio (contenders per
  winning slot), not picked as a small round number. 5 was fine intuition for
  "a few rivals," wrong by an order of magnitude for 500:50.
- The expensive path under optimistic locking is losing, not winning — the
  reverse of what single-statement trained me to expect.

**Cost me time:**

- `TestServiceCreateOptimisticExhaustsRetries` hard-coded 5 canned
  `ErrVersionConflict` values. Raising `maxOptimisticRetries` to 50 didn't
  fail it loudly — the fake ran out of errors on attempt 6, fell through to
  its zero-value `nil`, and the test silently asserted success on a request
  that should have exhausted retries. Fixed by deriving the fixture length
  from the constant instead of a literal, so it can't drift again.
- No ceiling on the backoff formula (`1<<attempt`). Harmless at 5 attempts,
  but unbounded doubling means a request needing ~20+ attempts would sleep
  for hours on a single retry. Capped it now, before a higher future budget
  makes it a real problem instead of a theoretical one.

**Tomorrow:** `SERIALIZABLE` isolation as the fourth approach — no version
column, no explicit lock; trust Postgres's predicate-based conflict detection
(SSI) and retry on SQLSTATE `40001`. Open question: does it inherit
optimistic's loser-pays-more shape, or does it behave more like `FOR UPDATE`'s
flat cost? And does SSI's failure rate need the same retries=50-scale tuning,
or is it a different number entirely?

## Day 11

### What I built

A fourth booking strategy. The code is almost the same as single-statement: read
the unit, check availability, decrement, insert. The difference is that all of
it runs inside a `SERIALIZABLE` transaction. There's no row lock and no version
column. The isolation level does the work, and my job is to retry when Postgres
says no.

- `WithSerializableTx` opens the transaction with `pgx.TxOptions{IsoLevel: pgx.Serializable}`.
- `40001` becomes `storage.ErrSerializationFailure`, both in each statement and at `Commit()`.
- `createSerializable` wraps the whole transaction in the retry loop, with the same backoff as optimistic.

### Prediction before measuring

[fill in: what I guessed — more like pessimistic or more like optimistic?]

### What happened

Correct at all three levels: 1, 10 and 50 seats all sold completely, and the
invariant held.

p95 was 348 ms, 1.31 s and 2.78 s. That puts it between single-statement and
`FOR UPDATE`, very close to single-statement at high contention and drifting away
as seats go up.

### What I understand now

**It's neither pessimistic nor optimistic. It's both, depending on the request.**
Readers don't block, so if my snapshot says 0 seats I return sold out straight
away. That's the fast path `FOR UPDATE` killed. But if my snapshot still shows a
seat, my `UPDATE` waits for whoever holds the row, and when they commit, Postgres
aborts me with `40001` instead of letting me continue. So I wait _and_ retry.

**That explains why the gap to single-statement grows with seats.** More seats
means more requests start with a snapshot that shows availability, which means
more of them go through block → abort → retry. With `FOR UPDATE` it was the
other way round, because there every request queues, even the ones that will be
rejected.

**`40001` can come from `Commit()`.** Under `SERIALIZABLE`, Postgres can decide
at commit time that the transaction can't be serialised. If I only checked the
statement errors, some aborts would come back as generic 500s instead of being
retried.

**Rollback happens before the sleep.** The deferred `Rollback` runs when
`WithSerializableTx` returns, and the backoff happens after that. A sleeping
request isn't holding one of the 50 pool connections. If the sleep were inside
the transaction, retries would starve the pool.

**Losers aren't free.** Winners at 10 seats had p95 1.13 s, lower than the
overall 1.31 s. Requests that lose have to abort and retry before they even find
out they lost.

### Things that went wrong

**My retry counts were wrong.** `go run` compiles the binary and runs it as a
child. `$!` gave me the wrapper's PID, so `bench.sh` killed the wrapper and left
the server running. Its retry total only got printed when the _next_ run killed
it. The 334 I saw under the 1-seat run actually belongs to the 10-seat run, and
the 1-seat and 50-seat counts are just gone. That probably affects Day 10's
2,476 as well. Fix: build once, run `bin/api` directly.

**`make reset` failed randomly with `EOF`.** On a fresh volume, Postgres first
runs a temporary server for init that only listens on the Unix socket.
`pg_isready` checked the socket, said ready, and then `migrate` connected over
TCP and found nothing. Fix: `pg_isready -h 127.0.0.1`.

**Copy-paste in the retry loop.** The optimistic loop checks
`maxSerializableRetries` for its last attempt. Both are 50, so nothing changes
and the tests still pass. That's the lesson: the tests only see what they
assert, and equal defaults hide a wrong variable name.

**The last attempt used to sleep before giving up.** No run hit the retry limit,
so it never showed up in the numbers, but a request that has already decided to
fail shouldn't wait up to 2 s first.

### Questions I should be able to answer

- Why doesn't `SERIALIZABLE` block readers, and why does that matter for sold-out requests?
- What exactly happens when two `SERIALIZABLE` transactions update the same row?
- Why does single-statement under `READ COMMITTED` not abort in the same situation?
- Where can `40001` come from, and why does the retry have to wrap the whole transaction?
- Why is a retry budget a correctness setting and not only a performance setting? (Day 10)

### Still open

- Optimistic at 1 and 10 seats, so the four-way table has no gaps
- `SERIALIZABLE` medium ×3, plus retry counts at 1 and 50 seats
- The commit-time `40001` path has no automated test. The fake store never
  commits. That needs an integration test against real Postgres.

## Day 12

### What I built

Integration tests that run against a real Postgres started by the test itself.
Testcontainers launches `postgres:16-alpine`, the real migrations get applied, and
every test resets the tables and re-applies the seed. The setup lives in
`internal/testdb` so `storage` and `booking` share it, and everything is behind
`//go:build integration`, so `make test` still works without Docker.

Tests, in the order I wrote them:

- constraint and not-found mapping in `storage`
- `WithTx` and `WithSerializableTx`: rollback, commit, invisible until commit, nesting refused
- `SERIALIZABLE` block-then-abort
- `SERIALIZABLE` write skew, in both commit orders
- all four strategies with 50 goroutines on 10 seats
- 50 goroutines retrying one idempotency key

### Prediction before measuring

Write skew: I guessed the error would fire at T2's `COMMIT`, and that whichever
transaction commits first wins.

### What happened

Both parts were right. The loser always failed at `COMMIT`, in both commit
orders, 20 out of 20. No statement failed on either side.

My reason was wrong, though. I said it's because `SERIALIZABLE` throws everything
away, but that's what happens after the failure, not why it happens.

Everything else passed repeatedly under `-race`. No strategy oversold, and
concurrent retries of one key produced exactly one booking.

### What I understand now

**Coverage doesn't mean the code works.** I misspelled `"chk_availability"` on
purpose. `booking` stayed green at 100% coverage. The integration test failed with
a raw `SQLSTATE 23514`, which my handler would turn into a 500 instead of a 409.
The fake store returns whatever error I tell it to, so it can never check that I
recognise the error Postgres actually sends.

**Why write skew fails at commit.** Under `SERIALIZABLE`, every read leaves a
marker that doesn't block anyone. T1 wrote the row T2 had read, so T2 has to come
before T1. T2 wrote the row T1 had read, so T1 has to come before T2. That's a
cycle, and no serial order exists. Postgres doesn't abort when the cycle forms,
because nobody has committed yet. When one side commits, the other is marked
doomed and fails at its own `COMMIT`.

**Two different ways to get `40001`:**

- Same row, write vs write: the second `UPDATE` waits, then fails. First updater wins.
- Different rows, read vs write cycle: nobody waits, the loser fails at `COMMIT`. First committer wins.

My code can't tell them apart, which is why the retry wraps the whole transaction.
The second kind is also the only reason the commit-time mapping in
`WithSerializableTx` exists, and until today nothing tested it.

**The pool is where `FOR UPDATE` waits.** The pool only has `max(4, NumCPU)`
connections, not 50. That doesn't deadlock, because the transaction holding the
row lock already has its connection and will commit. The other goroutines wait in
`pgxpool.Acquire` instead of in Postgres. It also means my harness retry counts
aren't benchmark numbers.

**`TestMain` runs once per package, and I never call it.** `go test` calls it,
and `m.Run()` is what runs the tests. That's why the container is shared, and why
every test has to reset the data.

**Proving a goroutine is blocked.** I poll `pg_stat_activity` until the process
shows `wait_event_type = 'Lock'` instead of sleeping. With `sleep`, the test
would pass on my laptop and flake on a slow CI runner.

**A green test can still skip a branch.** The idempotency test passed ten times,
but the log said `replays=0` every time. All 49 losers arrived while the winner
was still working, so they all got in-flight. The replay path, which is the
"response got lost and the client retried" case, was never run. I only noticed
because of the log line. I added a late retry at the end to cover it.

### Things that went wrong

- My first `TestMain` had `testDB, err := ...`, which created a new local variable and left the package one `nil`.
- `defer` in `TestMain` never ran, because `os.Exit` skips deferred calls. Fixed by moving setup into a `run()` function that returns.
- `//go:embed` can't reach `migrations/` from `internal/storage`, since it only embeds files at or below the package directory. Switched to a `file://` path.
- I called `Restore` with no `Snapshot`, on a container variable I'd never assigned. Switched to truncate and reseed, which also avoids fighting the pool's open connections.
- My overbooking test used the 10-seat unit, so the second decrement just succeeded. It needs the 1-seat one.
- I printed the whole struct with `%d` instead of `unit.AvailableUnits`. That kind of bug only shows up when the test fails, which is exactly when the message matters.
- Neovim said "no packages found" for the test files. gopls ignores files behind a build tag unless it's told about the tag.

### Questions I should be able to answer

- Why can't a unit test with a fake store catch a misspelled constraint name?
- What are the two ways `SERIALIZABLE` produces `40001`, and where does each one fire?
- Why does Postgres wait until commit to abort the write-skew loser?
- Why doesn't `FOR UPDATE` deadlock when the pool is smaller than the number of goroutines?
- Why reset data between tests instead of starting a container per test?
- How do you prove in a test that a transaction is actually waiting on a lock?

### Still open

- Makefile target and CI job for the integration tests, and `go vet -tags=integration`
- Storage coverage number
- Why optimistic retries about 2.5× more than `SERIALIZABLE` in the same test

## Day 13

### What I built

- The integration tests now run in CI, in their own job.
- Makefile targets `test-integration` and `vet`.
- ADR-002, the concurrency-control decision.

### Prediction before measuring

[fill in: did I expect the one-session run to confirm the old ranking?]

### What happened

It didn't. Single-statement and `SERIALIZABLE` came out tied, 864 ms against
865 ms p95. Optimistic and `FOR UPDATE` form a slower tier, about 2–2.5× behind.

The old gaps came from comparing numbers from different days. Today's
`SERIALIZABLE` was 1.8× faster than Day 11's with the same code.

### What I understand now

**Only compare numbers from the same session.** The machine matters as much as
the code. The quickest check is the fastest request: on Day 11 even that took
495 ms, and today it took about 70 ms. If the floor moves, the conditions changed.

**Know your noise.** The same code 20 minutes apart differed by 19%, so any gap
smaller than that says nothing.

**The first run lies.** The first run of the session was more than twice as slow
as the next two. Do one warm-up run first.

**A test can measure something other than what you meant.** In the 1-seat
optimistic run, the winner finished before anyone else reached the database.
There was no contention, just 499 fast rejections.

**Why single-statement is enough.** "`available_units` never below 0" only
concerns one row, so a `CHECK` constraint can enforce it. The relative decrement
waits for the row lock, applies to the latest committed value, and either
succeeds or hits the constraint. `SERIALIZABLE` is for rules across several rows,
like a per-customer limit, where no single-row constraint can help.

**When performance ties, guarantees decide.** Since the fast tier is tied, the
choice between single-statement and `SERIALIZABLE` came down to what each can
guarantee, not speed.

### Things that went wrong

- I almost wrote the ADR using the cross-day table. It would have said single-statement was 2× faster than `SERIALIZABLE`, which isn't true.
- CI was pinned to Go 1.25 while I use 1.27 locally, and it had a Postgres service that nothing used.

### Questions I should be able to answer

- Why is one `UPDATE` enough for booking, but not for a per-customer limit?
- Why can't the saga rely on the isolation level for consistency?
- How do you know whether two benchmark numbers can be compared?
- What would make you revisit ADR-002?

### Still open

- Integration job duration in CI
- Why optimistic retries about 7× more than `SERIALIZABLE`
- `CHECK (available_units <= total_units)`, before compensation in week 4

## Day 14

### What I built

- Kafka running locally in Docker Compose, in KRaft mode, with no ZooKeeper.
- `cmd/kafkademo`, a small franz-go program that produces keyed events and consumes them in a consumer group, with manual commits and a switch to simulate a crash.

It's my first time using Kafka.

### Prediction before measuring

Is it safe to publish to Kafka inside the database transaction, before `COMMIT`?
[fill in: what I guessed]

### What happened

Three experiments, all run for real:

- The same key always went to the same partition, and the Go client agreed with the Java CLI on every key.
- Crashing before the commit meant the same messages came back on restart. The first restart also sat idle for about 45 seconds first.
- Adding a second consumer moved only one partition. Stopping a consumer cleanly handed its partitions over in about a second.

### What I understand now

**The six basics:**

- A topic is an append-only log. Reading doesn't remove anything.
- A topic is split into partitions, and order only holds within a partition.
- The key picks the partition, so keying events by `booking_id` keeps one booking's events in order.
- An offset is a position within one partition.
- In a consumer group, each partition goes to exactly one consumer, so the partition count sets the maximum parallelism.
- Committing an offset saves progress. Crash before committing and you get the message again.

**Kafka is at-least-once. Handling duplicates is my job.** The message came back
after the crash, and nothing marked it as a repeat. Later this week I'll build an
idempotent consumer, which is Day 6's idempotency keys again, applied to messages.

**A crash and a clean shutdown aren't the same even when no data is lost.** After
the crash, the partitions stayed idle until the dead consumer's session timed
out. After Ctrl-C, they moved in about a second. That's why handling the shutdown
signal and closing the client properly matters.

**A committed offset beats `--from-beginning`.** The reset setting only applies
to a group that has never committed.

**Why writing to the database and then publishing is broken:**

1. The database commits, then the publish fails or the process crashes. The event is lost forever and nothing retries it. Downstream never hears about a real booking, and nobody notices.
2. The publish succeeds, then the database rolls back. Consumers act on a booking that doesn't exist.

Publishing inside the transaction before `COMMIT` doesn't help, because the
commit can still fail afterwards, and a message can't be un-sent. It also holds
locks during a network call. There are two systems with no shared commit, so
every ordering can fail.

**The outbox fixes it by making it one system.** The event is written as a row
in the same transaction as the booking. A relay publishes it later and marks it
sent. If the relay crashes, it sends the row again. That's at-least-once, so
consumers deduplicate.

### What I got wrong

- I thought 5 consumers on 3 partitions meant 2 would work. It's the reverse: 3 work and 2 sit idle.
- I said a consumer restarts from where it failed. It restarts from the last committed offset, so it processes the uncommitted messages again.
- I said the key "keeps track" of the booking. The real point is ordering: same key, same partition, same order.
- I claimed franz-go gives more control over commits than kafka-go. Both support manual commits. The real differences are the API shape and franz-go's fuller protocol support (transactions, exactly-once), which Loka doesn't need.

### Things that went wrong

- `kafka:` was indented under `postgres:` in the Compose file, which gave `Additional property kafka is not allowed`. `docker compose config` catches this without starting anything.
- zsh read `(healthy)` in a pasted comment as a file pattern. Fixed with `setopt interactivecomments`.
- An empty line in the console producer crashed it, because every line needs the `key:` separator. End input with Ctrl-D.
- My first Go version was the franz-go README example with `package kafkademo`, a `WaitGroup` that was never marked done (so it hung), topic `foo`, and a typo `booking-event` that would have auto-created a stray topic.
- I first put the crash check at the start of `run()`, where it exits before consuming anything, so it proved nothing.

### Questions I should be able to answer

- Why does the partition count limit consumer parallelism?
- Why key events by `booking_id`?
- What does at-least-once mean, and where did I see it happen?
- Why did a crash delay the handover by about 45 seconds when Ctrl-C didn't?
- Why is "write to the database, then publish" broken in both orders?
- How does the outbox pattern avoid it, and why does it still need an idempotent consumer?

### Still open

- Check `outbox_events` against the outbox pattern: an event `id`, an ordering column, `aggregate_id` for the key, `published_at`, and a partial index on unpublished rows

## Day 15

### What I built

- **Booking writes its event.** Every booking now writes a `booking.created` event into `outbox_events` in the same transaction, through one helper that all four strategies share.
- **`cmd/relay`,** a separate program that picks up unpublished events, sends them to Kafka keyed by `booking_id`, and marks them published.
- **A comment in `idempotency.go`** explaining why a key that vanished mid-race maps to `ErrRequestInFlight`.

### Prediction before measuring

After the relay crashes between publishing and marking:

- `published_at` for the 3 events: [fill in]
- messages the consumer sees after the normal relay runs: [fill in]

### What happened

After the crash, all 3 events were still unpublished in the database, even
though Kafka already had them. The transaction rolled back. The normal relay
then sent them again: 6 messages in Kafka, 3 of them duplicates with the same
`event_id`. Nothing was lost.

### What I understand now

**The outbox turns two systems into one.** The booking and its event are rows in
the same database, written in the same transaction, so either both exist or
neither does. Only the relay talks to Kafka, and it can retry forever.

**Set `published_at` after publishing, not before:**

- Marking first and then crashing loses the event.
- Publishing first and then crashing duplicates it.

A duplicate can be handled; a loss can't.

**`FOR UPDATE SKIP LOCKED` is the job-queue pattern.** Two relays never take the
same rows, and neither waits for the other. A relay that dies holding locks
loses nothing, because its transaction rolls back and the rows are free again.

**Stop the batch at the first failure.** If event 2 fails and event 3 is for the
same booking, sending 3 anyway means a consumer could see "confirmed" before
"created". Stopping keeps the order, at the cost of one bad event blocking the
rest until it succeeds.

**A sequence number isn't commit order.** A slow transaction can get id 5 and
commit after id 6. A relay that asks for `id > last_seen` skips 5 forever. Mine
asks for `published_at IS NULL`, so 5 is picked up late but never lost.

**Go interfaces are implicit.** `KafkaPublisher` becomes a `Publisher` just by
having a `Publish` method. The compiler only checks where it's used as one,
which is why `var _ Publisher = (*KafkaPublisher)(nil)` is worth adding: it moves
the check next to the type, like Java's `implements`.

**`Query` vs `QueryRow`.** `QueryRow` is one row, so `Scan` can go straight on
it. `Query` is a cursor: check the error, `defer rows.Close()`, loop with
`Next`/`Scan`, then check `rows.Err()`. An unclosed `rows` inside a transaction
blocks the next statement on that connection.

**`jsonb` reorders keys and changes whitespace.** Consumers must not compare
payloads as raw bytes.

**`now()` is the time the transaction started.** That's why a batch shares one
`published_at`.

### Things that went wrong

- I passed `s.store` instead of `tx` to `insertBookingWithEvent` in all four strategies. The booking would have committed on its own connection, outside the transaction, which on a `SERIALIZABLE` retry means a duplicate booking. The unit tests passed anyway, because in the fake store the transaction _is_ the store. The integration test with an injected outbox failure is what catches it.
- My fake `InsertOutboxEvent` first reused the `InsertBooking` counter and error.
- The aggregate type was `"booking_created"`. It should be `"booking"`, the same for every event about a booking.
- After `make reset` the topic didn't exist, and franz-go doesn't auto-create it. The relay kept retrying and lost nothing, which turned into a good test. `make reset` now creates the topic.
- I read an event's `attempt_number = 0` as a bug in failure recording, but the database had been reset and it was a different event. The relay test settled it: recording works.
- In `main.go` I set an unexported field (`r.afterPublish`) from another package, which doesn't compile, so I added a setter. The crash hook also fired on empty polls until I added `len(sent) > 0`.

### Questions I should be able to answer

- Why is "write to the database, then publish" broken, and how does the outbox fix it?
- Why set `published_at` after publishing rather than before?
- What does `SKIP LOCKED` do, and why does the relay need it?
- Why can a relay using `id > last_seen` lose events?
- Why does the batch stop at the first failure?
- Where do duplicates come from, and what does a consumer need in order to handle them?
- Why can't the fake store catch the `s.store` vs `tx` bug?

### Still open

- Idempotent consumer (Day 16)
- Dead-letter handling for an event that fails forever
- Server-side re-claim when a released idempotency key is found
- Per-booking ordering with more than one relay

## Day 16

### What I built

- **`processed_events`,** a table that records which consumer has finished which event.
- **`ClaimEvent`,** which inserts that record and tells me whether the event is new or a duplicate.
- **`internal/consumer`,** the shared machinery: parse a Kafka record, claim it, run the service's handler in the same transaction, retry what might succeed, skip what never will, and commit the offset only after the database.
- **`internal/notifier` and `cmd/notifier`,** the first real consumer. It writes a `booking_confirmation` row to `notifications` for every `booking.created`.

### Prediction before measuring

Experiment 1 (relay duplicates):

- messages waiting for the notifier: [fill in]
- notifications at the end: [fill in]
- `duplicate skipped` lines: [fill in]

Experiment 2 (crash after the database commit):

- notifications right after the crash: [fill in]
- how long the restarted notifier waits before it sees the messages again: [fill in]

### What happened

- **Experiment 1:** 11 messages in Kafka, 8 notifications, and exactly 3 skipped: the 3 events the relay had sent twice.
- **Experiment 2:** the notifications survived the crash, nothing doubled after the restart, and the group ended at lag 0.

### What I understand now

**Kafka still holds duplicates. The consumer makes them harmless.** The relay
can't avoid them: Postgres and Kafka can't commit together, so a crash between
"sent" and "marked" means sending again. The choice is duplicates or losses.
You take duplicates, and deduplicate at the consumer. Producer at-least-once,
plus an idempotent consumer, gives effectively-once.

**Duplicates happen without crashes too.** A consumer can crash before
committing, a rebalance can hit in the middle of a batch, or a publish can time
out even though Kafka actually stored it. They're rare, but never impossible.

**A committed row in `processed_events` proves the work was committed.** The row
and the work go in one transaction, so both exist or neither does. Another
transaction can't see an uncommitted row, so any row it _can_ see means "done".
No status column needed.

**Claim with `INSERT`, not `SELECT`.** Two copies arriving at once would both
pass a `SELECT` check. With the primary key, the second `INSERT` **waits** for
the first transaction:

- if the first **commits**, the second gets a conflict, inserts 0 rows, and skips;
- if the first **rolls back**, the second inserts and does the work itself, which is correct, because the first one's work never happened.

It's the same as Day 6's idempotency keys: let a database constraint decide
who's first.

**Put the claim and the work in one transaction.** In separate transactions:

- claim first, then crash → the effect is lost;
- work first, then crash → the effect is duplicated.

**Commit the database before the Kafka offset.**

- Database first, then crash → the message is redelivered, and the claim makes it a skip.
- Offset first, then crash → the message is never redelivered, and the work is lost.

It's the relay's reasoning again: choose the order where a crash causes a
duplicate, not a loss.

**The primary key is `(consumer, event_id)`, not just `event_id`.** Otherwise
the notifier processing event 7 would make a voucher service skip event 7.

**How long to keep rows depends on Kafka retention, not on how fast processing
is.** A duplicate can only arrive while the message still exists (7 days). A
stuck partition or a deliberate replay can bring an old message back days
later. So keep rows for at least 7 days, plus a margin. I first said 45
minutes, then 24 hours, based on normal processing time, and both would have
sent duplicate emails.

**Skip what can never succeed; retry what might.** A missing header or a bad
payload is poison: log it with its location and move past it, or it blocks the
partition forever. A database outage is transient: retry the same record and
never move past it.

**A unique-constraint error aborts the whole Postgres transaction.** Catching it
and returning `nil` doesn't help: the commit becomes a rollback, the claim is
lost, and the event loops forever. `ON CONFLICT DO NOTHING` avoids the error
entirely.

**`TRUNCATE … RESTART IDENTITY` means every table that stores copied IDs must be
reset too.** Otherwise a leftover `('notifier', 1)` makes the next test's new
event 1 look like a duplicate.

**The handler is the service-specific part, and everything else is shared.**
It's the `http.Handler` pattern: the framework receives, and my code reacts to
one item. `Handle` gets `tx`, so its writes can't escape the transaction.

### Things that went wrong

- I started work on `main`, without a branch. Fixed with `git switch -c`, then `git branch -f main origin/main`.
- A commit "didn't happen". It was the pre-commit hook blocking on `gofmt` and the message scrolled past. The cause every time was **CRLF line endings** from my editor on WSL.
- I swapped the up and down migrations. `migrate` ran `DROP TABLE` going up.
- `ClaimEvent`, first version:
  - it didn't check the `Exec` error, so a database outage would have looked like a duplicate;
  - it returned a duplicate as an error;
  - it was lowercase, so the consumer package couldn't call it.
- I confused the relay's side with the consumer's side: I tried to build a `kgo.Record` in `parseEvent`, and used `storage.Outbox` as the consumer's event type.
- I mixed up Kafka retention (7 days) with the session timeout (about 45 s).

### Questions I should be able to answer

- Why can't the producer prevent duplicates, and what makes them harmless?
- Why claim with `INSERT` instead of `SELECT`? What does a second `INSERT` do while the first is uncommitted?
- Why must the claim and the work share one transaction? Why is the database committed before the offset?
- Why is the primary key `(consumer, event_id)`?
- How long must `processed_events` rows be kept, and why?
- Poison vs transient failures: what does each do to a partition?
- Why does catching a unique violation inside the transaction cause an endless loop?

### Still open

- A dead-letter topic, and a limit on retries
- The `processed_events` cleanup job
- Notifier handler tests
- The 16 s delay on the first event
- Editor saving LF on WSL

## Day 17

### What I built

- **`cmd/paymock`,** a fake payment provider. It's idempotent on `Idempotency-Key`, and has knobs to make it slow, decline, or succeed without answering in time.
- **`payment_pending`,** the allowed-transitions table, and `TransitionBookingStatus`.
- **ADR-003,** the payment saga design.

### What I understand now

**The hardest case in payments is a timeout that was actually a success.** The
money moved, but the answer never arrived. Retrying with the **same**
idempotency key is safe: the provider replays what already happened. The key is
chosen by the caller (here, the booking ID), not the provider.

**Idempotency records must survive restarts.** The paymock keeps keys in memory,
so after a restart it charged `test-1` again. A real provider stores keys
durably, for the same reason my `idempotency_keys` table is in Postgres.

**A decline is an answer; a timeout is not.** A decline returns `200` with
`status: declined`: the request was valid, and the answer is "no". Only a
malformed request is a `400`.

**Why `payment_pending` exists.** With only `pending`, an expiry job could cancel
a booking while its charge was succeeding. The customer pays, and the booking is
gone. Expiry only touches `pending`, so a booking with a charge in flight is safe.

**A transaction doesn't stop two processes contradicting each other.** Both can
read `payment_pending`, decide, and write. What stops it is putting the expected
status **in the `UPDATE`** (compare-and-set). Also: always check the `WHERE` of
an `UPDATE`. Without `booking_id`, it would change every booking in that state.

**Never cancel when you don't know if money moved.** Keep retrying with the same
key; after a threshold, escalate to a human, and leave the booking
`payment_pending`, which is the truthful state.

**Compensation is not rollback.** The seat decrement committed long ago. Giving
seats back is a new write that reverses its effect.

### Things that went wrong

- `curl -max-time` instead of `--max-time`: one dash is for single letters.
- A migration's `\d` showed the constraint as `= ANY (ARRAY[…])`, which is the same as `IN (…)`.
- Docker's daemon stopped several times after sleep or reboot.
- Lint flagged `checkTransition` as unused, because I hadn't written its test yet.

## Day 18

### What I built

The saga from ADR-003:

- a consumer that starts it;
- a worker that charges and confirms or compensates;
- the storage pieces they need (claim with a lease, record a payment, release seats);
- an HTTP client that turns responses into outcomes;
- `cmd/payments`, running both.

### Prediction before measuring

For the lost-response test:

1. The worker would log a retry after about 10 s: ✅
2. The booking would be `confirmed` right after that: ❌ It stayed `payment_pending`. After a timeout the worker knows nothing, so it records nothing.
3. It would be confirmed after about 2 s: ❌ It took 30.2 s: the lease has to run out before anyone tries again.
4. Two charge lines, the same `charge_id`, differing in `lost_response` and `replay`: ✅
5. One payment row: ✅

### What happened

- **Success:** confirmed about 0.9 s after `booking.created` reached Kafka.
- **Decline:** cancelled, with the seats going 9 → 8 → 9, released exactly once. `cancelled_at` and `failed_at` were identical to the microsecond, which proves one transaction.
- **Lost response:** the worker asked twice, the provider charged once, and the booking was confirmed after the lease.

### What I understand now

**The consumer name is who reads, not what is read.** Payments and the notifier
read the same topic, but each needs its own group (so each gets every event) and
its own `processed_events` key (so one doesn't skip the other's events).

**Small interfaces avoid adapters.** `storeAdapter` exists only because of
`WithTx`'s signature. `Transition` needs just one method, so it takes a
one-method interface, and `*storage.Store` fits directly.

**What `Handle` returns decides what `Run` does:**

- `nil` → done;
- poison → skip;
- anything else → retry forever.

So a conflict (the booking isn't pending any more) is `nil`, because retrying
can't make it pending again. A missing booking or an illegal transition is
poison.

**The row lock makes the second worker wait; the status condition makes it
fail.** Under `READ COMMITTED`, the waiting `UPDATE` re-checks its `WHERE` after
the first commits, and matches 0 rows.

**A lease instead of a lock.** A lock can't be held during an HTTP call without
holding a transaction open. Pushing `next_attempt_at` forward claims the booking
without a transaction. If the worker dies, the lease simply runs out. A
permanent "done" marker would strand the booking forever after a crash.

**Measure waiting from `updated_at`, not `created_at`.** Escalation is about time
in `payment_pending`. Claiming must not touch `updated_at`, or repeated claims
would hide a stuck payment.

**Classify HTTP answers by what they tell you about the money:**

| Answer                                           | Classification                                                                              |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------- |
| `200` succeeded / declined                       | a result                                                                                    |
| timeout, connection refused, `5xx`, `408`, `429` | unknown → retry with the same key. A `5xx` may come after charging, so it's never a decline |
| other `4xx`, or a `200` with an unknown status   | permanent → escalate, never cancel                                                          |

**A sentinel error marks a _kind_ of failure.** The caller checks with
`errors.Is`, through any wrapping, instead of matching message text.

**A decline must record everything in one transaction:** the transition,
releasing seats, the payment row, and the event. The transition comes first, so
a second worker fails fast with `ErrStatusConflict`. A lost race is not an error;
it means the work is already done.

**The unique index is the backstop, not the protection.** It only covers
successful payments. The compare-and-set protects everything in the transaction.
`chk_availability` can't catch a double release that stays under capacity.

**A batch must finish inside its lease.** Charging sequentially would outlive the
lease for later bookings, so the batch runs in parallel.

### Things that went wrong

- I named the payments handler `Notifier`, and kept the notifier's payload, method names and comments.
- I used `booking.StatusConfirmed` instead of `StatusPaymentPending` in the consumer. That would have poisoned every booking.
- I passed the type `*storage.Store` instead of the variable `tx`.
- I thought a timeout couldn't be fixed by retrying. It's exactly what the same key fixes.
- I thought a `500` meant "not charged". It can come after charging.
- I proposed cancelling after repeated failures, which could void a paid booking. The rule is to escalate.
- I thought transaction order affects atomicity. It doesn't: the order only decides how fast and how clearly a conflict fails.
- `errcheck` flagged `resp.Body.Close()`. Ignoring it with `_ =` is fine when I can say why the error doesn't matter.
- I ran the result queries for the previous booking's ID.

### Questions I should be able to answer

- Why does each consumer need its own group name?
- Why does a small interface let `*storage.Store` be passed without an adapter?
- What stops two workers confirming the same booking?
- Why a lease rather than a lock, and why not a "processed" flag?
- How is each HTTP response classified, and why is a `5xx` never a decline?
- Why is a lost race logged, not treated as an error?
- Why must a batch finish within its lease?
- Walk through the lost-response scenario: why exactly one charge?

### Still open

- Worker integration tests with a fake provider
- A provider lookup endpoint for recovery
- The expiry job
- An escalation alert
- The provider-down and worker-crash experiments

## Day 19

### What I built

- **Exponential backoff with full jitter for payment retries:** a
  `payment_attempts` column, and a new `SET` in `ClaimDuePayments`.
- **A circuit breaker around the payment provider:** `BreakerProvider`, a
  decorator using `sony/gobreaker`.
- **The outage experiment,** which proved both working together.

### Prediction before measuring

For the outage experiment:
[fill in: how many calls before the breaker opens; what the worker logs while
it's open; whether retry times are spread out; what happens when the 30 s ends;
how long recovery takes; how many charges per booking]

### What happened

- **The breaker opened after the 5th failure,** but 6 calls got through, since
  the batch had already started them in parallel.
- **While open, calls failed instantly,** without touching the network.
- **Every 30 s, one probe was sent.** It failed, and the breaker reopened.
- **Retries spread further apart each round.**
- **When the paymock came back,** the next probe succeeded, the breaker closed,
  and all 6 bookings were confirmed over 48 s. One charge each.

### What I understand now

**Jitter stops a herd.** Clients that fail at the same moment retry at the same
moment unless something randomises them.

**Retries are selfish.** Each one helps one caller, but adds load to a service
that's already struggling. Retrying at several layers multiplies it: 3 layers ×
3 retries = 27 calls. Loka retries a charge in exactly one place, the worker's
lease. `HTTPProvider` and the paymock don't retry.

**The jitter variants** (from Brooker's _Exponential Backoff And Jitter_), with
`temp = min(cap, base × 2^attempt)`:

- **full:** `random(0, temp)`;
- **equal:** `temp/2 + random(0, temp/2)`;
- **decorrelated:** based on the _previous_ sleep, not the attempt number.

I first mixed up full and decorrelated.

**Exponential backoff needs the attempt number,** and a stateless worker can't
remember it, so it lives on the booking. `next_attempt_at` alone only says
_when_, not _how many_.

**"The larger of the lease and the backoff" silently switched jitter off.** For
early attempts the lease always won, so a batch claimed together retried at the
exact same moment, the herd jitter exists to prevent. Adding the jitter **on
top of** the lease keeps the lease guarantee and spreads every retry. Lesson:
when combining two rules with max/min, check what each one does across the
whole range.

**The backoff cap must stay well below the escalation threshold.** Escalation
is only checked after an attempt, so a long gap between attempts would delay
the alert, not just the retry.

**In one `UPDATE`, every `SET` reads the old row,** so the backoff sees the
attempt count before this attempt's increment. `RETURNING` shows the new row.

**Two separate questions about every error:**

1. **Should it be retried?** The worker decides.
2. **Is the provider unhealthy?** The breaker decides.

|                                   | Retry?            | Unhealthy? |
| --------------------------------- | ----------------- | ---------- |
| declined                          | no, it's a result | no         |
| timeout, `5xx`                    | yes               | yes        |
| `400`, unrecognised response      | no, escalate      | no         |
| our shutdown (`context.Canceled`) | yes               | no         |

A breaker that counts declines would block good cards because of empty ones.

**One breaker per provider, shared by every call.** A per-goroutine breaker sees
one call and can never trip. At Traveloka, the Resilience4j breaker was a shared
bean too; goroutines are like threads.

**Decorator versus adapter:**

- An **adapter** makes a shape fit, and is _required_ (`storeAdapter`).
- A **decorator** keeps the same shape and _adds_ behaviour, and is _optional_
  (`BreakerProvider`, `failOutboxStore`).

Removing the breaker still compiles; it only loses the protection.

**`gobreaker` is the engine; `BreakerProvider` is the decorator.** The library
knows nothing about payments. It decides whether any function may run.

**The breaker and the retry schedule are separate clocks.**

- The breaker's open timeout decides _whether calls may go through_.
- Each booking's `next_attempt_at` decides _when that booking shows up_.

The half-open probe is simply the first call after the cooldown.

**In Go, an interface call hides which method runs.** `w.provider.Charge` runs
`BreakerProvider.Charge` because of what `main.go` stored in the field. A text
search won't find it; "Go to Implementations" will.

### Things that went wrong

- The first migration put `payment_attemps` (typo) on `payments` instead of
  `bookings`, with an invalid down migration (`DROP ALTER TABLE`).
- SQL written like Go:
  - `rand(0, x)` instead of `random() * x`;
  - `min(a, b)` (an aggregate) instead of `least(a, b)`;
  - adding a number to `now()` instead of converting to an interval.
- The tests failed with a syntax error, then passed with no SQL change. The
  earlier run compiled an unsaved or older file. `go test` reads the disk, not
  the editor.
- Put `&d.Attempts` (a pointer) in a `%d` error message, then a value that's
  meaningless after a failed `Scan`.
- A parameter named `cap` shadowed Go's built-in.
- `BackOffBase` next to `MaxBackoff`. Also: if `main.go` hadn't set them, the
  zero values would have silently disabled the backoff.
- VS Code on this Mac didn't know the `integration` tag, like Neovim on Day 12.
- Thought the breaker would read `next_attempt_at`, and that it would be one per
  goroutine.
- Counted unrecognised responses and `context.Canceled` as provider failures.

### Questions I should be able to answer

- Why does jitter matter, and why is retrying at several layers dangerous?
- Full, equal and decorrelated jitter: what's the difference?
- Why is the attempt count a column, and why is jitter added to the lease
  rather than taking the larger of the two?
- Why must the backoff cap stay below the escalation threshold?
- What are a breaker's three states, and what moves between them?
- Which errors count against the provider, and why not declines or
  cancellations?
- Why one breaker per provider, and not per call?
- Decorator versus adapter: which is `BreakerProvider`, and how do you know?
- In the outage run, why did 6 calls reach the provider before the breaker
  opened?

### Still open

- Validate the config at startup
- Skip claims while the breaker is open
- A failure-ratio trip
- Breaker metrics and alerting

## Day 20

### What I built

The saga's timeout path, skipped on Days 17–18:

- a partial index on `pending` bookings by `created_at`;
- `ExpirePendingBookings`: one query that claims and cancels a batch;
- `booking.Expirer`: cancels, releases seats per unit in a fixed order, and
  writes `booking.cancelled`, all in one transaction;
- the expirer as a third loop in `cmd/payments`, configured by
  `BOOKING_EXPIRE_AFTER`;
- the `booking.cancelled` payload moved into `booking`, shared with the worker.

I wrote the tests first this time, and watched them fail before the code
existed.

### Prediction before measuring

For the relay-outage experiment:
[fill in honestly: I didn't write predictions before running. What would I have
said for: the status of batch A and B at 70 s; available units; unpublished
outbox rows; what T4 logs when the relay comes back; payment rows at the end?]
I didn't write predictions before running this experiment, so there is
nothing to compare. Lesson: write them before the first command, not after
the results.

### What happened

- **Batch A expired at 60 s** and released its 3 seats, while the relay was
  still down. Its `booking.cancelled` events waited in the outbox.
- **When the relay came back,** batch A's `booking.created` events were
  published late. The consumer found the bookings already `cancelled`, backed
  off, and charged nothing: 0 payment rows for A.
- **Batch B, inside the deadline,** was confirmed normally.
- Seats ended at 7 = 10 − 3 confirmed.

### What I understand now

**The compare-and-set decides races, not the idempotency key.** I first said
the idempotency key stops the expirer and the consumer from both moving a
booking. It doesn't: Loka's keys stop duplicate API requests and duplicate
charges. What decides is `WHERE booking_status = 'pending'`: both `UPDATE`s hit
the same row, the second waits for the lock, re-checks the `WHERE` after the
first commits (READ COMMITTED), matches 0 rows, and gets `ErrStatusConflict`.

**Whoever makes a transition publishes its event, in the same transaction.**
I said that if the consumer wins, the expirer should publish "paid". Wrong
twice: `payment_pending` doesn't mean paid (the charge could still be
declined, and then there'd be a "paid" and a "cancelled" event for one
booking), and the expirer doesn't own that transition, so it has nothing to
say.

**A timeout is not an outcome.** I got "don't expire `payment_pending`" right,
but for a business reason (a lost sale, not our fault). The real reason is
correctness: in `payment_pending`, Loka doesn't know whether the customer was
charged. Cancelling means acting on a guess. If the guess is wrong, the
customer has paid for a seat someone else can now buy. Only a known outcome
may end a charge in flight. The same rule is why the worker never cancels on a
timeout.

**Compare timestamps with the clock that wrote them.** I wanted Go's
`time.Now()`. `created_at` comes from Postgres, so an app server's clock would
mix two clocks: a fast server expires bookings early, and two servers
disagree. The rule stays in Go (the duration); only the clock reading is
Postgres's.

**The `payments` table records facts about money.** A decline is a fact: a
charge was attempted, and the provider refused. An expiry isn't: no charge was
ever attempted. So `cancel()` inserts a payment row, and expiry must not.

**Lock order prevents deadlocks.** A batch spanning two units locks two
inventory rows. Two expirers taking them in opposite orders deadlock
(Postgres aborts one with `40P01`). Sorting by `unit_id` gives everyone the
same order. A Go map's iteration order is randomised on purpose, so "whatever
order the map gives" is no order.

**Batch the work, check the rule once.** One `UPDATE` for the whole batch, one
seat release per unit instead of per booking, and one `checkTransition` per
batch, because the pair `pending → cancelled` is the same for every row. The
per-row check is the `WHERE`.

**A partial index needs the literal.** The planner uses
`WHERE booking_status = 'pending'` only if the query says `'pending'` too, not
`$3`.

**Where a loop runs decides what goes down with it.** In `cmd/payments`, the
expirer stops when the consumer stops, so our own outage doesn't expire
bookings. That's a policy, and it has a cost: seats stay held during the
outage, and after a restart the backlog and the expirer race.

**Borrowed numbers need re-justifying.** 15–30 minutes at Traveloka is how
long a customer has to go and pay. Loka has no such step: the deadline here
means "how long we hold a seat while our own pipeline is stuck." Same number,
different meaning.

**Why test first.** A test that has never failed proves nothing: it might pass
whatever the code does. Writing it first shows it failing, turns the Block 1
decisions into checks before I build the thing, and tells me when I'm done.
Breaking the code on purpose (release outside the transaction, no
`SKIP LOCKED`) confirmed the right tests fail.

**A struct per query result, not a half-filled `Booking`.** Returning four
fields in a 13-field struct leaves zero values that look like data.
`ExpiredBooking`, like `DuePayment`, holds exactly what the caller uses.

### Things that went wrong

- Q2: chose Go's clock, then hesitated. I was on the right track.
- Q4: credited the idempotency key with deciding the race, and wanted the
  expirer to publish "paid" when it loses.
- Q6: mixed up payment retries with expiry ("retry when the server restarts").
- Q7: justified 15 minutes with Traveloka's number instead of Loka's.
- Forgot the `migrate create` command; ran migrations with Postgres not
  started (`connection refused`).
- Skipped writing predictions before the experiment, and didn't save T4's logs.
- Batch B was created 62 s after A instead of 30 s. It nearly expired too.

### Questions I should be able to answer

- Why does expiry only touch `pending`, never `payment_pending`?
- What guarantees exactly one winner when the expirer and the consumer move the
  same booking? What does each side do when it loses?
- Why must the transition, the seat release and the event be in one
  transaction? What breaks if each one is missing?
- Why does a decline insert a payment row, and an expiry doesn't?
- Why compare `created_at` with Postgres `now()`, not Go's `time.Now()`?
- `created_at + config` versus an `expires_at` column: what does each cost?
- How can two expirers deadlock, and how does sorting prevent it?
- Why is `checkTransition` called once per batch, and what protects each row?
- Why does the expirer run inside `cmd/payments`, and what is the cost of that?
- How did you choose 15 minutes, and what does the deadline mean in Loka?
- Why write the test before the code?

### Still open

- The chaos test (Days 21–22), with M2's new check: at least one booking actually expired
- Measure healthy insert → `payment_pending` at p99 under load
- An expiry metric and alert
- Customer cancel during `payment_pending`

## Days 21–22

### What I built

- **A ledger in paymock** (`GET /charges`), so the test can check the
  provider's side: Loka's tables can look perfect while a customer was
  charged twice.
- **An automated chaos test:** the four real binaries, paced load, SIGKILLs on
  a schedule, a drain, and 15 invariants checked against Postgres and the
  ledger. It runs at 1k by default and at 10k with two environment variables.
- **A rewrite of the payment worker's loop,** from "claim a batch, wait for
  all of it" to "keep up to N charges in flight, refill each slot as it
  frees", plus the worker's first tests.

### Prediction before measuring

[fill in honestly, or write "not written before the runs": for the 1k run,
the drain time and whether any invariant would fail; for the 10k run, the
work left when the load ended, the drain time, and whether anything would
break or only slow down]

### What happened

- **The first 1k run passed, but one fault never happened.** The "relay down
  until 5 bookings expire" phase ended after 0.2 s, because 5 had already
  expired, after the first `cmd/payments` kill.
- **Every 1k run took about 5 minutes to drain after a 200 s load.** The
  worker managed about 1.9 charges/s while bookings arrived at 5/s.
- **After the worker fix,** the same run drained in 71–79 s: −75%, with the
  same 10-way concurrency.
- **10,000 bookings at 25/s passed on my Mac:** 6,312 confirmed, 2,543
  declined, 1,146 expired, 0 violations. The worker was still slower than the
  arrivals, and the backlog drained steadily in 318 s.

### What I understand now

**A crashed Kafka consumer still holds its partitions.** SIGKILL means it never
says "I'm leaving", so the broker waits for the session timeout (about 45 s)
before reassigning. My replacement was up in 5 s and consumed nothing for 40 s
more. So the expiry deadline must be well above _restart time + session
timeout_, not just restart time. 15 minutes is; 30 s isn't.

**Head-of-line blocking.** A batch that waits for all its members runs at the
speed of its slowest member. With 10% lost responses, about 65% of batches of
10 contained one, and that one held 9 idle slots for 10 s. The fix was the
scheduling, not more concurrency: slots, refilled one at a time. Same 10-way
concurrency, drain −75%.

**A buffered channel is a semaphore.** `make(chan struct{}, N)`: sending takes
a slot and blocks when all N are taken; receiving frees one. `struct{}` costs
nothing.

**Claim only as many as you can run.** A claimed booking's lease starts
ticking at the claim. Claiming more than the free slots would spend leases in
a queue, and an expired lease lets another worker take the same booking.

**Change one variable per experiment.** I kept `MaxInFlight` = the old
`BatchSize` (10), so the before/after measures the scheduling alone. Raising
concurrency is a different question: how much will the provider accept?

**Drain time = backlog ÷ throughput + the slowest single path.** My first
`drainTimeout` only had the second term. It was fine at 1k and would have
failed at 10k. A stall timeout ("has any work finished in the last 8
minutes?") fits a backlog of any size and still catches a stuck system.
Progress means beating the best so far, because recording an outcome briefly
_adds_ work (an event to publish and to process).

**A chaos test must prove the chaos happened.** M2 fails the run if there
wasn't at least one restart, lost response, replay, decline, expiry and
confirmation. Otherwise a green run could just mean the faults missed.

**Wait for states, not times.** Faults trigger at a share of the load, the
long outage ends when bookings have expired, and the harness waits for
readiness before going on. A slower machine makes the run longer, not flaky.
The same goes for `make up`: `--wait` for the healthchecks, or `migrate` hits
a Postgres that isn't ready (`EOF`).

**SIGKILL, not SIGTERM, for chaos.** SIGTERM runs my graceful shutdown, the
happy path. A crash gives no chance to commit offsets or finish a batch, and
that's what has to be safe.

**Fresh topic per run.** The reset restarts outbox ids at 1. On a reused
topic, old events with the same ids would make `processed_events` skip the
new ones.

**`t.Fatalf` only from the test goroutine.** It calls `runtime.Goexit`, which
stops the goroutine it's on. Load workers report results; the test decides.

**A client timeout is an unknown outcome too.** M1 accepts between "201s" and
"201s + transport errors", the lost-response problem on the client side.

**`count(column)`, not `count(*)`, after a `LEFT JOIN`.** The NULL-filled row
for "no match" counts as 1 in `count(*)`.

**The numbers explained themselves.** Lost responses = retries in every run
(each retried once with the same key, replayed, never charged twice).
Charges = confirmed + declined in every run (expired bookings never reached
the provider). Only successful new charges can lose their response, which is
why 6.8% were lost, not 10%.

### Things that went wrong

- `make up && make migrate-up` failed with `EOF`: `docker compose up -d`
  returns when containers start, not when Postgres is ready.
- Pressed Ctrl-C on a run that was just quiet between chaos phases.
- Phase 3 ended in 0.2 s on the first run: it counted total expiries, not new
  ones.
- The drain timeout was derived per booking and ignored the backlog.
- Started the paymock change on `main` and had to branch before committing.
- Planned 10k with 2,000 seats: it would have sold out. Caught before running.
- Wanted to write the load generator myself, then had it written instead.
- [fill in: predictions, if skipped again]

### Questions I should be able to answer

- Why does a chaos test need provider-side checks as well as database checks?
- What does M2 protect against?
- Why did bookings expire after a 5 s `cmd/payments` crash with a 30 s
  deadline? What does that mean for the production deadline?
- What is head-of-line blocking, and how did 1 lost response in 10 cut
  throughput to about 1.9/s?
- How does a buffered channel work as a semaphore?
- Why does the worker claim only as many bookings as it has free slots?
- Why keep `MaxInFlight` at 10 for the before/after?
- Why a stall timeout instead of a fixed drain deadline?
- Why SIGKILL and not SIGTERM? Why a fresh topic per run?
- At 10k, why was the worker still slower than the arrivals, and why is that
  not the same problem as before?

### Still open

- Tune `MaxInFlight` against the provider's concurrency limit
- Shorten consumer rejoin after a crash (static membership / session timeout)
- An expiry-rate metric and alert
- The 1k chaos test as a nightly CI job
- Customer cancel during `payment_pending`; the provider lookup endpoint

## Day 23

### What I built

- **Tracing across all four services:** the API, the relay, payments and
  paymock, viewed in Jaeger. One booking is one trace, from `POST /bookings`
  to `booking.confirmed`, including every retry.
- **Trace context that travels with the data:** in a JSONB column on the
  outbox and booking rows, in Kafka headers, and in the HTTP `traceparent`
  header to the provider.
- **Spans with meaning:** a charge attempt, a publish, a process, an expiry,
  each with the booking id, and an error status only when the system failed.
- **A fix for a consumer bug** that I found by looking at a trace.

### Prediction before measuring

[fill in honestly, or write "not written before the runs": the end-to-end
time of one booking and where it goes; how long paymock's span would be when
the caller times out at 10 s; whether paymock's span would show an error;
when the retry would start]

### What happened

- **A warm booking took 1.24 s from request to confirmation,** and about
  90% of that was waiting for polls: the relay, then the worker, then the
  relay again. The actual work after the API was about 90 ms.
- **A trace showed a gap of 10 s, then 34 s, then 28 s** before payments
  processed a booking after a restart. The timings matched "old process
  killed + 45 s" to within 0.3 s. The cause: `client.Close()` hangs with
  `BlockRebalanceOnPoll`, my second Ctrl-C killed the process, and it never
  left the consumer group. After `CloseAllowingRebalance`, the group is empty
  right after shutdown and a restart charges a booking within 6 s.
- **In the lost-response run,** attempt 1 failed after 10 s with no answer,
  attempt 2 succeeded 34 s later in 28 ms, and paymock logged the same
  `charge_id` both times. Paymock's own span said **200, no error, lost
  response**, while payments' client span said **error**.
- **The expirer tests failed on my first try:** the `booking.cancelled` rows
  had no trace context. The spans were right, the events weren't.

### What I understand now

**Context travels with the data.** Across an HTTP call the trace goes in a
header automatically. Across an async hop nothing carries it unless I store
it next to the data: in the outbox row for the relay, in the booking row for
the worker and the expirer, in the Kafka headers for the consumer. Whatever
reads the data reads the parent with it.

**Inject the span that did the work.** The relay puts **its own** span into
the Kafka headers, not the stored one, so the consumer appears under the
publish. The worker's outcome event and each expiry event carry their own
span, so the trace keeps going after them. One shared `ctx` for a whole batch
would put every booking's event in the wrong trace.

**A span's error means the system failed, not that the answer was no.** A
declined card is a correct answer: no error. No answer, a permanent error, or
an outcome that couldn't be recorded: error. Otherwise every decline would
look like an outage on a dashboard.

**End a span only when its outcome is known.** The expirer's spans start
inside the transaction and end after it, so a rollback is an error and never
a fake "expired".

**Telemetry is best effort.** With Jaeger down, bookings still returned 201
and the spans were dropped. Tracing must never be the reason a request fails.
In production a local Collector would buffer instead.

**Latency in an async pipeline is mostly waiting.** A hop through Kafka costs
about 5 ms, because the consumer is already waiting on the partition. A hop
through a polled table costs up to the poll interval. Three polled hops made
up 90% of a booking. `LISTEN/NOTIFY` would remove most of that.

**`BlockRebalanceOnPoll` changes how a consumer must close.** Leaving the
group needs a rebalance, and the option blocks rebalances until
`AllowRebalance`. A shutdown during a poll never calls it, so `Close` hangs.
`CloseAllowingRebalance` is the documented shortcut. A consumer that doesn't
leave holds its partitions for a full session timeout (45 s).

**A server's status code is what it would have sent, not what arrived.**
Paymock's handler returned without writing, and `otelhttp` recorded the
default 200. The client had already hung up. From the provider's telemetry
alone, the lost response looked like a success; from mine alone, like an
outage.

**The trace proves idempotency.** Two attempts, two calls, one `charge_id`:
the second call was a replay that took 2.4 ms. Money moved once.

**A broken hop shows up as orphan root traces downstream.** Before I restarted
payments, paymock's spans had no parent. Lots of root traces in a service that
is only ever called by others means a hop lost its context.

**A package-level tracer binds to the first global provider.** In tests, the
recording provider has to be installed once per test binary (`sync.Once`), or
later tests never see a span.

### Things that went wrong

- First `relay.go` draft called `Publish` twice and ended the span before the
  real publish.
- Added `trace_context` to the outbox `SELECT` but not to the `Scan`.
- Copy-pasted `"api"` as the relay's service name.
- A logging-only Setup fallback left `shutdown` nil, which would have
  panicked at the deferred flush.
- `telemetry.go` first draft: `AlwaysSample()` without the package prefix, a
  variable called `resource` shadowing the package, and `Inject` returning
  `{}` instead of `nil`.
- Forgot to click Find Traces again and thought Jaeger had no new traces.
- The expirer's `booking.cancelled` events had no trace context at first; the
  new tests caught it.
- Ran the lost-response experiment without restarting payments, so the
  Step 6 spans were missing.
- [fill in: predictions, if skipped again]

### Questions I should be able to answer

- What is in a `traceparent`, and what does each part do?
- How does the trace cross the outbox, Kafka and the booking row? Why not
  just pass `ctx`?
- Why does the relay inject its own span into Kafka, not the stored one?
- Choice A vs choice B for the worker's parent: what does each show, and
  which removes a dependency on the booking row?
- Head sampling vs tail sampling: what can each one keep?
- What happens to bookings when the tracing backend is down? Why?
- Why is a declined charge not an error span?
- Why does the expirer end its spans after the transaction?
- Where did a warm booking's 1.24 s go? What would you change first?
- Why did a restarted consumer wait 45 s, and how did one line fix it?
- In the lost-response trace, why is paymock's span 10 s and 200, while the
  client's is an error?
- How does the trace prove a retried charge didn't charge twice?

### Still open

- Step 7: the consumer re-parents the booking's trace (choice B)
- Worker trace tests
- `LISTEN/NOTIFY` instead of polling, then measure again
- An OpenTelemetry Collector; a sampling ratio for production
- One Setup-failure policy for every binary; tracing in the notifier
- Read Jaeger's `Warnings (1)`; verify the idle-reconnect explanation

## Day 24

### What I built

- **A steady-load test** for the normal path: 50 bookings/s spread over
  1,000 units plus 25 reads/s, an open-model k6 script, and a run script
  that starts every run from the same state (reset, seed, warm-up,
  checkpoint) and refuses to start when something is missing.
- **Text tools for Jaeger**: which seconds had slow requests, and one
  trace's spans with the gap.
- **A separate connection pool for `GET /bookings/{id}`**, so reads can't
  queue behind writes. It didn't move p99 on my machine (below).
- I skimmed this day. Claude wrote the scripts, the implementation and the
  tests; I ran the experiments and answered the design questions only in
  part.

### Prediction before measuring

- p99 at 50 bookings/s and the slowest span: **not written** (I skipped it).
- Block 2, written before looking: slow traces **cluster** (right); the
  extra time is a **gap** (wrong: it was inside a span); `pg_stat_statements`
  shows a ~1 s statement (wrong: max 221 ms).
- Block 2, written with the output in front of me, so they don't really
  count: whichever statement is in flight (right); a slow GET waits for a
  connection (right); checkpoints line up with the stalls (wrong).
- Diagnostic run: the in-VM probe stays fast (half right: 3 of 5
  episodes); Kafka spikes (half right: biggest CPU user, not aligned with
  the stalls).
- The fix: Claude predicted GET p99 under 100 ms (wrong: 796 ms median),
  at least one after-run over 500 ms (right), POST p99 unchanged (right).

### What happened

- **Baseline p99 was 281 ms median, range 79–1,071 ms**, on identical
  code. p50 was ~30 ms in quiet runs.
- **The slow requests came in bursts**: 1–1.7 s freezes, a few per two
  minutes, hitting whatever each request was doing, even a `BEGIN` that
  Postgres answers in 0.004 ms. No statement was slow inside Postgres and
  no checkpoint ran during the runs.
- **A slow GET waited 882 ms for a connection**, because frozen POSTs held
  all 50. So I gave GETs their own pool.
- **With the read pool, GET p99 didn't change**: 796 vs 739 ms median,
  ranges overlapping. POST p99 didn't change either.
- **Runs next to each other looked alike, whatever binary ran**: after-1
  and before-7 both stalled, after-2 and before-8 were both clean.

### What I understand now

**p99 is decided by very few requests.** In a 2-minute run at 75
requests/s, about 60 requests set p99. One 1-second freeze puts more than
that in the tail, so on this machine p99 mostly answers "was there a freeze,
and how long".

**One run proves nothing about p99.** Identical code gave 79 ms and
1,071 ms. A change only counts when the worst run after beats the best run
before.

**Alternate before and after runs.** The freezes came and went over about
10 minutes. Running all the "before" runs first and all the "after" runs
later would have credited, or blamed, the change for the machine's mood.

**An open load model, with enough VUs.** k6's arrival-rate executor keeps
sending even when the server is slow. With too few pre-allocated VUs it
dropped the requests that arrived during a freeze, and p95 looked 2.5×
better than it was.

**Time can hide in three places.** In the API outside any span (the gap),
inside a client span but not inside Postgres's execution (network, a
paused machine, the commit), or inside Postgres (`pg_stat_statements`).
The slow requests were the middle case: the API waited a second for an
`UPDATE` that Postgres executed in under 95 ms.

**`pg_stat_statements` doesn't count the commit.** 20,048 commits averaged
0.006 ms, which is impossible if the disk flush were included.

**A slow query looks different from a frozen machine.** A slow query is
the same statement in every slow trace. A freeze is whatever happened to
be in flight, including `BEGIN`.

**A bulkhead only helps if the bottleneck is behind it.** A separate pool
protects reads when writes hold all the connections while the database is
fine (row-lock contention, say). Here the freeze reached the reads
directly, so the bulkhead had nothing to protect them from. One trace
made me think otherwise.

**Same primary, not a replica.** A separate pool to the same database
keeps read-your-writes: a GET right after a 201 always finds the booking.

**"Usually N+1 or a missing index" didn't apply.** The data said: the
environment, amplified by a shared pool. That's a finding too.

### Things that went wrong

- Skipped the p99 prediction, and gave some later predictions together
  with the output.
- `scripts/seed-load.sql` was saved as an empty file, and
  `scripts/k6/load.js` wasn't saved at all; the first runs failed on that.
- The VU change didn't get saved either, so one more run dropped requests.
- Pasted a command with `<a slow id>` placeholders; zsh read `<` as a
  redirect and ran nothing.
- Skipped building `bin/after`, so three runs refused to start, and the
  before runs came out not alternated; redid the sequence.
- Re-ran before-1 before saving run 3's traces (no harm: run 1's stall was
  bigger).
- Claude's mistakes: too few pre-allocated VUs; a warm-up that swallowed
  every k6 error; a missing `BIN_DIR` in a command; a bash-4-only line and
  a jq version difference in `traces.sh`; "GET p99 is pool wait" concluded
  from one trace; claiming 10 round trips per POST instead of 8.

### Questions I should be able to answer

- What are p50, p95 and p99, and why can't one run measure p99?
- Why an open load model? What does k6 do with too few VUs during a stall?
- How do you tell a slow query from a frozen machine in traces?
- Where can a request's time hide besides a slow statement?
- Why doesn't `pg_stat_statements` show the commit's disk flush?
- What does a separate read pool protect against, and why didn't it help
  here?
- Why a separate pool to the same primary rather than a read replica?
- Why alternate before and after runs, and why a burn-in run?

### Still open

- A slow GET trace from an after run (pool wait or `SELECT`?)
- The read pool under hot-unit contention load
- Where the stalls outside the VM come from; why Kafka uses ~3 CPUs
- The idempotent response inside the booking transaction (8 → 7 round
  trips, closes a crash window)
- The same load on Linux / native Docker

## Day 25

### What I built

- **A Redis cache for the availability page** (`GET /units/{id}`),
  cache-aside, with a 30 s TTL as the backstop. Booking never reads it: a
  stale "sold out" loses a sale and a stale price loses money, and the
  single-statement decrement (ADR-002) prevents overselling either way.
- **Invalidation through the outbox**: `booking.created` and
  `booking.cancelled` reach a new `cmd/cacheinvalidator`, which deletes the
  unit's key. `booking.cancelled` gained `unit_id` and `qty` (version 2).
- **Who wrote what:** I designed it in Block 1. I wrote the error sentinel,
  the cache interface with its null object and option, the Redis adapter, the
  first versions of `GetUnit`, the handler and the invalidator, and the
  `GetUnit` handler itself. Claude wrote all the tests, the final fixes to
  `GetUnit`, the response struct and the invalidator (when I asked for
  speed), the step 4 diffs, and the wiring, compose and load scripts.

### Prediction before measuring

- Block 1: cache-on `GET /units` p50 2–3 ms, p99 5–8 ms. **Wrong:** p50
  4.3–15 ms, p99 64 ms–1.7 s, and not better than cache-off at all.
- TTL 30–60 s. The TTL turned out not to matter: invalidation every 20 s
  ends every entry first.
- Block 3 and the follow-ups: **not written** (I skipped them three times).
- Claude: hit-rate ceiling 80% at 200/s (right: 79–81%); ~18,000 fewer unit
  reads per run (right: ~17,800); ~18,000 vs ~11,000 calls at 100/s (right:
  18,202 vs 10,881–11,424).

### What happened

- **At 200 reads/s the machine was overloaded**: 4 of 6 runs dropped
  requests, POST p50 rose to 77–220 ms. In the 2 valid runs and all the
  invalid ones, latency ranges overlapped. The cache took 74% of the
  availability reads off Postgres.
- **The 50 ms timeout fired on 7–8% of reads**, 1,700–2,000 per run, each
  costing 50 ms before Postgres even started.
- **At 100 reads/s all runs were valid**, and the cache made reads
  **slower**: p50 +0.5 ms, p95 about 2×, no overlap. It still took 58% of
  the reads off Postgres.
- **A 250 ms timeout changed nothing**: the same fallbacks, the same
  latency.

### What I understand now

**A cache makes a read faster only if what's behind it is slower than the
cache.** Here Postgres answered in 0.2 ms; the cost was the network trip,
and Redis is the same trip. What I got was database offload, not speed.

**A miss costs more than no cache.** GET the cache, read Postgres, SET the
cache: three trips instead of one. At a 59% hit rate, 41% of reads paid
that, and p95 showed it.

**The hit rate is set by how often a key is read between invalidations.**
Every unit is booked every 20 s, so the 30 s TTL never fires. Half the
reads per unit took the hit rate from 80% to 59%.

**A timeout can't fix a frozen machine.** It only decides how long you wait
before falling back. Set it above the dependency's normal slow requests,
and expect a freeze to blow through any value.

**Libraries retry in places you don't see.** go-redis dials 5 times, 100 ms
apart, by default, separately from `MaxRetries`. Every read waited ~400 ms
with Redis down until a test actually stopped Redis.

**Delete, after commit, through the outbox.** DEL can't install an old value
the way an out-of-order SET can. Deleting after commit means a reader can't
refill the old row in between. The TTL covers a crash between commit and
delete.

**Poison or retry is the consumer's most important decision.** Bad JSON
never becomes good, so skip it. Redis down gets better, so retry without
committing. Get it backwards and you either block a partition forever or
lose invalidations silently.

**Events evolve by adding fields and bumping the version.** Consumers read
only the fields they need (tolerant reader), and old events decode with
the new fields zero.

**`.gitignore` patterns match at any depth.** `api` hid every new file in
`internal/api/`.

### Things that went wrong

- `redis.go` wasn't saved, so a test failed against code I thought I had
  fixed (the second unsaved file in two days).
- I copied `Ping` from a syntax example into `Set`/`Delete`: check-then-act,
  and it passed the first four tests.
- The response started as a `map`, missing two fields; then a struct with
  `omitempty` that dropped `description`.
- The first invalidator deleted `unit:00000000-…` on a missing unit and
  retried bad JSON forever.
- `loadruns/burn-1` already existed, so the burn-in refused to start, and
  the next six runs had no binaries. Fixed with a new label and `&&`.
- I couldn't apply the `+`/`-` diffs by hand; full files worked.
- No predictions for Block 3 or the follow-ups.
- Claude's mistakes: a test gap (Ping-then-Set passed); the 50 ms timeout
  chosen without measuring; go-redis's dial retries found only by a manual
  check; a broken shell heredoc that left four stray files in its own
  container; saying "a Redis hop costs more than Postgres", which the data
  can't separate from the miss share.

### Questions I should be able to answer

- When does a cache lower latency, and when does it only offload the
  database?
- Why can a cache make p95 worse? Count the round trips for a miss.
- Cache-aside: why DEL rather than SET on write, and why after commit?
- What sets the hit rate here, and why didn't the TTL matter?
- Why invalidate through the outbox and Kafka instead of from the API?
- How does the invalidator choose between skipping and retrying an event?
- How do you add fields to an event without breaking old consumers or old
  events?
- What should a cache client's timeout and retries be, and what can't a
  timeout fix?
- Why must the booking path never read the cache?

### Still open

- An in-process cache; filling after the response; caching an expensive
  query instead
- The exact hit-rate model; Redis spans in traces
- A Redis-only invalidator loop; a circuit breaker; rate-limited warnings
- The same comparison on Linux / native Docker

## Day 26

### What I built

- **The relay sends a whole batch to Kafka at once** instead of one event at
  a time, and a failure no longer stops the batch.
- **`published_at` is stamped with `clock_timestamp()`**, so it says when
  the event was really marked.
- **A configurable API write pool and a `/debug/pool` endpoint**, plus a
  drain test for the relay and pool / lag / CPU lines in every load run.
- **Who wrote what:** I wrote `clock_timestamp()`, `RunOnce` in three
  phases, `KafkaPublisher.Publish`, and the pool env var, runtime log and
  `/debug/pool` (after review fixes). Claude wrote the tests, both scripts
  and the old-relay build.

### Prediction before measuring

- Drain before batching: I wrote 2–4 events/s (a seconds/milliseconds
  slip); the formula gave 107–240. **Measured 346–449.**
- Drain after: no number from me; Claude 2,000–4,500. **Measured
  4,278–4,839.** Speed-up predicted ~19×, measured **13×**.
- After batching I said Kafka would still dominate; Claude said Postgres's
  round trips. Neither was measured: per-batch time is more than round trips.
- Outbox lag with batching: **I said 10–20 ms. Measured ~300 ms** (p50):
  wrong by 15×, because the poll wait doesn't change.
- Write pool smaller → "gradually slower". **Wrong:** p50 flat from 50 to 2;
  only 2 got worse, in the tail, and collapsed when the machine was slow.
  Claude said "flat to 5, cliff at 2, p50 jumps": shape right, p50 wrong.
- GOMAXPROCS 2: "p50 same, p99 same or slower". **No effect shown.**
- Confirmation runs: **not written.**

### What happened

- Drain test: **358 → 4,660 events/s**, no overlap.
- Lag under load: p50 766 → 303 ms, p99 2,630 → 579 ms.
- Halfway through the main sequence every arm slowed down; the restart
  didn't help, and `pmset` showed the CPU limited to **~20%** of its speed.
- Pool 2 collapsed every time the machine was throttled; pool 50 never did.
  Reads kept working through it, on their own pool.

### What I understand now

**Batching turns a per-item cost into a per-batch cost.** 100 sends became
one. After that the batch size is the lever, because the remaining cost is
per batch.

**A metric can hide the very cost you're measuring.** `now()` is the
transaction's start, so every event in a batch got the same timestamp from
before any of it was sent. `clock_timestamp()` is the real time.

**Throughput and latency are different questions.** Batching made the relay
13× faster but lag only 2.5× lower: most of the lag is waiting for the next
poll. Lowering that needs LISTEN/NOTIFY, not more batching.

**A slow worker makes its own queue longer.** Slower sends → a longer cycle
→ a bigger batch → even slower sends.

**Little's law: busy = arrival rate × time held.** It tells you how many
connections are in use, not how many seconds. A pool only matters when it's
close to that number, and the number grows when the machine slows down. A
pool has to fit the slow case.

**A bulkhead helps when the bottleneck is behind it.** The read pool did
nothing on Day 24 (the database froze); today it kept reads alive while the
write pool collapsed.

**GOMAXPROCS is about CPU, not connections.** A goroutine waiting for
Postgres uses no thread. The API needs under one core here, so 2 vs 16
made no difference. It matters in containers with CPU limits.

**Check the machine before blaming the code.** `CPU_Speed_Limit 20` explains
an hour of confusing runs, and Day 24's drift.

**Check the error first.** On error, the other return values mean nothing.

**Measure a component alone when you can.** The drain test gave a clean 13×
in 7 minutes; the full-stack runs gave mostly overlap.

### Things that went wrong

- Units again: ms × per-second, three times (Little's law, the drain rate,
  CPU cores).
- My first `RunOnce` checked `len(events)` before `err` (a swallowed fetch
  error), then a missing `continue` marked failed events as published. The
  tests caught both.
- `newRecord(ctx, …)` instead of `m.Ctx`, `fmt.Sprintf("%+v")` instead of
  JSON, a TODO left in, and implementation before the tests.
- Predictions for the confirmation runs not written.
- Claude's mistakes:
  - pointing me at terminal output I can't see;
  - 263 instead of 203 connections;
  - a fetch-failure test that passed by luck;
  - a Kafka test that didn't check each record's `traceparent`;
  - nearly comparing lag with an old relay that still used `now()`;
  - assuming 4–9 ms round trips and a 40 ms connection hold;
  - not checking the thermal state before the long sequence, although it
    was on Day 24's Open list.

### Questions I should be able to answer

- Why 13× and not 19×?
- What does `now()` return in a transaction, and why did it matter here?
- Why did lag fall 2.5× when throughput rose 13×?
- Why mark every success on a partial failure, and what does that give up?
- Why must `ProduceSync`'s results be matched by `*Record`?
- Little's law for the write pool: why did pool 2 survive healthy and
  collapse throttled?
- When does a separate read pool help, and when doesn't it?
- What does GOMAXPROCS control, and when does it matter?
- Why measure the relay with a drain test instead of the full stack?

### Still open

- LISTEN/NOTIFY; a relay CPU profile; batch size 1,000
- Poison rows and events (attempt cap, dead letter); per-booking order
- Pools for 3 replicas; GOMAXPROCS in containers
- Measuring on a cool Mac or on Linux

## Day 27

### What I built

- **A `/metrics` endpoint on the API and the relay**, scraped by Prometheus.
- **A Grafana dashboard for the four golden signals**, provisioned from a
  JSON file in the repo, in its own compose project so `make reset` doesn't
  wipe its history.
- **`loadrun.sh` now prints the dashboard's p50/p95/p99** under k6's table
  for the same window.
- **Who wrote what:** I applied the telemetry setup, the `/metrics` route and
  the pool metrics, and ran every check and run. To save time Claude wrote
  the relay metrics and their tests, the monitoring stack, the dashboard and
  the loadrun step.

### Prediction before measuring

- Free metrics: "only `http.server.request.duration`". **Wrong:** three
  otelhttp histograms, plus otelpgx's DB durations, plus go/process metrics.
- `/metrics` counted as a route: "yes". **Wrong:** otelhttp wraps each
  route, not the mux.
- p99 of 300 ms with buckets 250 / 500: "between 250 and 500". **Right.**
  "The dashboard shows ~490": **wrong**, it interpolates (375 in the worked
  example).
- Averaging three replicas' p99s: "no, you need the bucket counts". **Right.**
- Raw path as a label: "180,000 paths, ~3 million series, Prometheus runs
  out of memory". **Right** (and the API's own memory grows too).
- Relay batch after the cold start: Claude said "a few ms". **Measured
  42.8 ms.**
- Dashboard vs k6: **not written.**

### What happened

- Docker's memory change didn't apply the first time, and the engine went
  down twice afterwards.
- All the free metrics appeared once a meter provider existed.
- In the load run the Mac was throttled to 20% and everything degraded, but
  k6 and Prometheus still measured the same requests: p50 matched within 2%,
  p99 was 21–25% high on the dashboard.

### What I understand now

**Pull vs push is about who starts the transfer.** Pull: the receiver asks
and controls the pace (Prometheus, Kafka consumers, the relay's poll).
Push: the sender sends right away (OTLP traces, LISTEN/NOTIFY). Robust
systems push a hint and pull the data.

**Golden signals: traffic, latency, errors, saturation.** RED for services,
USE for resources. Saturation is whatever queues up first: here the DB pool
and the outbox backlog.

**A histogram can only say which bucket a percentile is in.**
`histogram_quantile` interpolates inside it, so the error follows the
bucket's width: 2% at p50, 25% at p99 today.

**Never average percentiles.** Sum the bucket counts across instances, then
take the quantile.

**Labels must be bounded.** Every label combination is its own time series;
IDs belong in traces and logs.

**An absent series is not zero.** A counter that never moved has no data,
so the query has to say what "nothing" means.

**Count after the commit.** A rolled-back batch is sent again; counting it
earlier counts it twice.

**Monitoring is a separate system.** Its history shouldn't die with the app's
reset, and losing metrics shouldn't stop the relay from publishing.

### Things that went wrong

- I clicked away from Docker's settings without Apply & Restart.
- Docker created a **folder** called `prometheus.yml` because the file
  didn't exist yet when I started the stack.
- goimports in Neovim removed the new import before the code used it.
- The machine was throttled again (20%); the run counts only for the
  comparison.
- No predictions for the Block 5 comparison.
- Claude's mistakes:
  - guessing a stray old `docker` binary (it was Docker Desktop itself);
  - a first rollback test that passed with the bug in place;
  - a 10 s top bucket just under the 10 s Kafka timeout, so those batches
    landed in +Inf (found in a real run, fixed with 15 and 30 s);
  - predicting "a few ms" for a batch that took 42.8 ms.

### Questions I should be able to answer

- Pull vs push: who controls the rate, who must know whose address, what
  happens when the other side is down?
- The four golden signals for the API and the pipeline: which metric each?
- Why is the dashboard's p99 25% high when p50 is within 2%?
- Why can't you average three replicas' p99s, and what do you do instead?
- What does a raw path label do to Prometheus and to the API?
- Why does `failed_total` show No data, and how do you fix the query?
- Why count published events after the commit, and why skip empty polls?
- Why does the backlog use `max`, not `sum`, across relay replicas?
- Why run monitoring as its own compose project?

### Still open

- Tail-accurate buckets; consumer metrics and Kafka lag; alert rules
- The breaking-point run on a cool Mac (tomorrow)
- Docker Desktop upgrade before k8s
