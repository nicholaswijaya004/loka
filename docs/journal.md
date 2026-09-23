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
