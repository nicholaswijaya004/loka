# Day 2 — Reproduce Problem

**Goal:** Understand data races; build the smallest version of the double-booking bug.

## Measured

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

## Understood

- `count++` is load/add/store, not one instruction — races live in the gap.
- Atomics fix single-value ops but can't span "check then act".
- Mutex throughput degrades as `-cpu` rises; that's the cost of exclusivity.

## Tomorrow

Data model + migrations. The lock moves into Postgres.

# Day 3 — Database Layer

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
