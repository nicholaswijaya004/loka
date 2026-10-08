# Metrics

Measurements recorded as the project was built. Each entry states what was run,
what was observed, and what it showed.

**Environments.** Day 2 was measured on macOS (Go 1.27). Days 3 onward were
measured on WSL2 (Ubuntu 24.04), HP EliteBook 840 G7, 4c/8t 15W CPU, 8 GB RAM,
Postgres 16 in Docker capped at 512 MB, connection pool `MaxConns = 50`, with k6
**co-located with the service under test**.

Absolute throughput and latency are therefore not representative — the load
generator competes with the service for the same four cores, and the service
competes with Postgres. What these figures support is _relative_ comparison: two
runs differing by one line of code under identical contention.

---

## Day 2

**Goal:** Understand data races; build the smallest version of double-booking.

**Measured (macOS, Go 1.27, -race):**
| Type | Expected | Got | Result |
|---|---|---|---|
| UnsafeCounter | 1,000,000 | 365,541 | 63.45% lost, RACE |
| MutexCounter | 1,000,000 | 1,000,000 | correct |
| AtomicCounter | 1,000,000 | 1,000,000 | correct |
| UnsafeInventory | 10 booked | 34 booked | sold=25, available=-4, invariant -4+25=21≠10 |
| SafeInventory | 10 booked | 10 booked | sold=10, available=0, invariant holds |

Unsafe counter loss across runs: 61.32% / 62.06% / 63.45% — never the same twice.

**Understood:**

- count++ is load/add/store; races live in the gap between them
- a mutex in the struct does nothing unless every method calls Lock — my
  MutexCounter had the field but no locking and lost 78%
- atomics fix single-value ops but can't span check-then-act across two fields
- the unsafe inventory passed its test on the first run; -race caught it anyway

**Tomorrow:** Data model + migrations. The lock moves into Postgres.

---

## Day 3

**Goal:** Move the invariants into the schema, where no application bug can
route around them.

**Verified by attempting to violate each constraint directly in psql:**

| Constraint                             | Attempt                                           | Result               |
| -------------------------------------- | ------------------------------------------------- | -------------------- |
| `chk_availability`                     | `UPDATE inventory_units SET available_units = -1` | rejected — 23514     |
| `chk_availability`                     | `SET available_units = 50` (total 10)             | rejected — 23514     |
| `chk_booking_status`                   | `INSERT ... booking_status = 'banana'`            | rejected — 23514     |
| `idx_payments_one_success_per_booking` | 2 failed + 1 succeeded payment                    | all accepted         |
| `idx_payments_one_success_per_booking` | a second `succeeded` payment                      | **rejected** — 23505 |
| `idempotency_keys` PK                  | same key inserted twice                           | rejected — 23505     |

```
ERROR:  duplicate key value violates unique constraint "idx_payments_one_success_per_booking"
DETAIL:  Key (booking_id)=(3097064b-d804-44aa-b67f-71d21a110e73) already exists.
```

**Understood:**

- yesterday's `available = -4` is now a state Postgres refuses to store
- a partial unique index permits many payment attempts while forbidding a second
  success — the thing a plain `UNIQUE (booking_id)` could not express
- the same feature serves speed elsewhere: `CREATE INDEX ... WHERE published_at
IS NULL` indexes only the outbox rows the relay queries, and rows leave the
  index automatically once published
- hit a dirty migration state twice and recovered with `migrate force`

**Tomorrow:** The naive booking endpoint, deliberately unsafe.

---

## Day 4

**Goal:** A working booking endpoint with no transaction and no lock — the
baseline to measure against.

Sequential behaviour is entirely correct: 201 with a computed total, availability
10 → 9, 409 when sold out, 404 on an unknown unit, 400 on bad input.

**Verified graceful shutdown** by signalling SIGTERM one second into a 5-second
handler:

|                   |                    |
| ----------------- | ------------------ |
| Signal received   | 02:49:07.722       |
| Shutdown complete | 02:49:11.961       |
| Drain             | **4.24 s**         |
| In-flight request | completed normally |

The same shutdown with nothing in flight completes in 2 ms.

**Prediction for tomorrow:** 500 concurrent requests against a 10-seat unit will
oversell, as the in-memory version did. The CHECK constraint should prevent
corruption but not the race itself, so I expect constraint violations rather than
negative availability.

---

## Day 5

**Goal:** Measure the naive endpoint under contention.

**The prediction was wrong.** 500 concurrent requests against 10 seats produced
exactly 10 bookings, invariant intact, zero overselling.

```bash
make reset && make run
k6 run scripts/k6/contention.js
```

| Decrement strategy | SQL                                          | Bookings | `available_units` | Invariant   | Overbooked |
| ------------------ | -------------------------------------------- | -------- | ----------------- | ----------- | ---------- |
| Computed in SQL    | `SET available_units = available_units - $1` | **10**   | 0                 | 0+10=10 ✓   | 0          |
| Computed in Go     | `SET available_units = $1`                   | **500**  | 8                 | 8+500=508 ✗ | **490**    |

The second row runs with `UNSAFE_DECREMENT=1 make run`. Identical code paths
apart from that one statement.

**Why the SQL version holds.** The decrement is a single statement. Postgres
takes a row lock for its duration and evaluates `available_units - $1` against
the _current committed value_, not against whatever the application read moments
earlier. Fifty requests that all read `available_units = 10` still produce
10, 9, 8 … because each subtraction operates on fresh state. The eleventh is
rejected by `chk_availability`, surfaces as 23514, and becomes a 409.

**Why the Go version does not.** Every request read 10, computed `10 - 1 = 9`,
and wrote `9` absolutely. Fifty requests writing 9 have the same effect as one.
The final value reflects whichever write landed last.

**`chk_availability` fired 490 times in the safe run and zero times in the unsafe
run.** An out-of-date value written absolutely stays within legal bounds, so the
constraint has nothing to reject. A constraint can only defend an invariant the
database is able to evaluate.

**Single-seat contention:** 500 requests against 1 seat produced 1 booking and
499 conflicts. That case is _easier_ than the 10-seat one — the first decrement
takes availability to zero, so every later check fails legitimately and there is
barely a window.

**Consequence for the plan.** Days 8–10 were framed as "fix the overbooking."
That framing no longer holds. The question becomes: four strategies are all
correct — what do they cost?

---

## Day 6

**Goal:** Prevent a retried request from creating a second booking. Different
failure from overselling: no constraint can catch it, because two identical
requests are indistinguishable from two legitimate bookings.

```bash
make reset && make run
k6 run scripts/k6/retry.js
```

**Sequential — two identical requests, same `Idempotency-Key`:**

|                     | Before       | After                |
| ------------------- | ------------ | -------------------- |
| Responses           | 201, 201     | 201, **200**         |
| `Idempotent-Replay` | —            | `true` on the second |
| Booking ids         | two distinct | **identical**        |
| Bookings            | 2            | **1**                |
| `available_units`   | 10 → 8       | 10 → **9**           |

**Concurrent — 500 VUs sharing one key:**

|                          | Before | After      |
| ------------------------ | ------ | ---------- |
| Bookings created         | 10     | **1**      |
| `available_units`        | 10 → 0 | 10 → **9** |
| Seats lost to duplicates | 9      | **0**      |
| 409 in-flight            | —      | 499        |
| 5xx                      | 0      | 0          |
| p95                      | 215 ms | 684 ms     |

Latency rose because 500 requests now contend on one row in `idempotency_keys`
rather than spreading across ten inventory rows — the cost of funnelling every
retry through a single claim.

**Mechanism.** The claim is an `INSERT`, not a `SELECT` then an `INSERT`. Five
hundred requests race to insert the same primary key; Postgres admits exactly one
and returns 23505 to the other 499, which is how each learns whether it owns the
work. No application-level coordination is involved — the same property that made
`chk_availability` hold yesterday.

All 499 losers received 409 rather than a 200 replay, meaning the winner was
still in flight when the burst arrived. The sequential test covers the replay
path.

**Coverage:** `internal/booking` 100.0% of statements. That measures decision
logic only — it says nothing about the SQL, the HTTP wiring, or the concurrency
guarantee. A misspelled column name passes every test in the package.

---

## Open questions

To be measured in week 2:

- Throughput and p99 for four _correct_ concurrency strategies — single
  statement, `SELECT FOR UPDATE`, optimistic `version` column, and
  `SERIALIZABLE` — at low and high contention. Day 5 established that the single
  statement is already correct; what the alternatives cost is open.
- Retry rate under optimistic locking as contention rises.
- Whether the idempotency claim becomes the bottleneck before the inventory row
  does, given the latency increase on day 6.
- Mutex vs atomic throughput across `-cpu=1,2,4,8` (benchmark recorded but not
  yet transcribed here).

**The unsafe run reported zero HTTP failures.** All 500 requests returned 201.
A monitoring dashboard would have shown a 100% success rate while the service
oversold by 4,900% and left 7 seats still marked available. Nothing in the
response codes, the logs, or the metrics would indicate a problem — which is
why the invariant has to be enforced where the data lives rather than inferred
from what the application reports.

## Day 8

**Goal:** Make the booking flow atomic, and measure what that costs.

Environment: macOS, Docker Desktop, 500 VUs, 10 seats, `MaxConns = 50`.
Note this differs from the WSL2 host used on days 2–7 — Docker Desktop adds a
VM layer between the container and the host, so figures here are **not**
comparable with earlier days. Same-host comparisons only.

**Correctness:**

| Flow                                        | Bookings | `available_units` | Invariant     | Seats lost |
| ------------------------------------------- | -------- | ----------------- | ------------- | ---------- |
| Decrement and insert as separate statements | 8        | 0                 | 0 + 8 = 8 ✗   | **2**      |
| Both inside one transaction                 | 10       | 0                 | 0 + 10 = 10 ✓ | **0**      |

With failure deliberately injected after the decrement
(`FAIL_AFTER_DECREMENT=0.3`), the transactional version still held:

|                                          | Errors | Bookings | `available_units` | Invariant     |
| ---------------------------------------- | ------ | -------- | ----------------- | ------------- |
| Transaction, 30% injected insert failure | 5      | 10       | 0                 | 0 + 10 = 10 ✓ |

Five requests decremented availability and then failed. All five rolled back.
Pre-transaction, the equivalent failures consumed the seats permanently.

**Cost:**

| Configuration              | p95    | Errors |
| -------------------------- | ------ | ------ |
| Transaction, no injection  | 1.41 s | 0      |
| Transaction, 30% injection | 1.99 s | 5      |

A transaction holds a pool connection for its entire duration rather than for
each statement, so 500 concurrent requests against `MaxConns = 50` queue
considerably harder. [TODO: same-host pre-transaction baseline — the ~280 ms
figure from day 5 was measured under WSL2 and is not a valid comparison.]

Single runs on a laptop vary widely; p95 readings across runs on this host
ranged 0.74–1.99 s. Figures above are [TODO: single run / median of N].

**Idempotency, unchanged through the transactional path:**

| 500 VUs, one shared key | Result |
| ----------------------- | ------ |
| Bookings created        | 1      |
| 409 in-flight           | 499    |
| `available_units`       | 10 → 9 |
| p95                     | 190 ms |

## Day 9

**Goal:** Measure `SELECT FOR UPDATE` against the single-statement baseline.
Both are correct; the question is cost.

Environment: macOS, Docker Desktop, 500 VUs, one iteration per VU,
`MaxConns = 50`, k6 co-located. All figures produced by `scripts/bench.sh`,
which resets the database, starts the server, polls `/healthz`, runs k6 and
verifies the invariant against Postgres before exiting.

**Correctness — both strategies, all contention levels:**

| Strategy         | Seats | Bookings | `available_units` | Invariant     |
| ---------------- | ----- | -------- | ----------------- | ------------- |
| single-statement | 1     | 1        | 0                 | 0 + 1 = 1 ✓   |
| single-statement | 10    | 10       | 0                 | 0 + 10 = 10 ✓ |
| single-statement | 50    | 50       | 0                 | 0 + 50 = 50 ✓ |
| `FOR UPDATE`     | 1     | 1        | 0                 | 0 + 1 = 1 ✓   |
| `FOR UPDATE`     | 10    | 10       | 0                 | 0 + 10 = 10 ✓ |
| `FOR UPDATE`     | 50    | 50       | 0                 | 0 + 50 = 50 ✓ |

No overselling under either strategy at any contention level.

**Cost — p95 latency:**

| Contention | Seats | single-statement      | `FOR UPDATE`            | Ratio |
| ---------- | ----- | --------------------- | ----------------------- | ----- |
| High       | 1     | 311 ms                | 4.26 s                  | 13.7× |
| Medium     | 10    | 619 ms (344–843, n=3) | 5.23 s (3.62–5.46, n=3) | 8.5×  |
| Low        | 50    | 873 ms                | 5.60 s                  | 6.4×  |

**Cost — throughput (medium contention):**

| Strategy         | req/s | Wall time for 500 requests |
| ---------------- | ----- | -------------------------- |
| single-statement | 720   | 0.7 s                      |
| `FOR UPDATE`     | 82    | 6.1 s                      |

**Why the gap is this large.** The single-statement strategy has a fast path for
requests that will fail: availability is read outside any transaction, the check
happens in Go, and a sold-out request returns without ever acquiring a lock. In
this workload that is 490 of 500 requests.

`FOR UPDATE` removes that path. Every request takes the row lock before it can
discover whether it is sold out, so all 500 serialise through one row. The
minimum latency stayed at 182–248 ms across all `FOR UPDATE` runs — the first
request through is fast, and everyone behind it accumulates the wait.

The ratio shrinks as contention falls (13.7× → 6.4×) because with more seats a
larger proportion of requests are genuine writers that would serialise anyway.

**Caveat.** This workload is 98% rejections at medium contention. The
single-statement advantage comes entirely from letting rejected requests exit
early, so a workload where most requests succeed would show a much narrower gap.
These figures should not be generalised beyond a high-rejection booking path.

**Variance.** p95 on identical code ranged 344–843 ms (single) and 3.62–5.46 s
(`FOR UPDATE`) across runs on this host. Medians of three, ranges recorded.
Single-run figures elsewhere in this document should be read with the same
caution.

## Day 10

**Goal:** Measure optimistic locking (version column + retry) against the
single-statement and `FOR UPDATE` baselines, with particular attention to
retry budget sizing rather than just latency.

Environment: unchanged from Day 9 — macOS, Docker Desktop, 500 VUs, one
iteration per VU, `MaxConns = 50`, k6 co-located. All figures from
`scripts/bench.sh`.

**Correctness — low contention (50 seats), both retry budgets:**

| Strategy   | Retries budget | Bookings created | Failures | Failure type                  | Invariant                                    |
| ---------- | -------------- | ---------------- | -------- | ----------------------------- | -------------------------------------------- |
| optimistic | 5              | 9                | 491      | retry-exhausted, not sold-out | passes arithmetically; masks 41 unsold seats |
| optimistic | 50             | 50               | 0        | —                             | holds genuinely                              |

**Cost — p95 latency, low contention (50 seats):**

| Strategy               | p95      |
| ---------------------- | -------- |
| single-statement       | 873 ms   |
| `FOR UPDATE`           | 5.60 s   |
| optimistic, retries=5  | 2.19 s\* |
| optimistic, retries=50 | 5.63 s   |

\*Not a comparable "cost of success" — this run had 491/500 requests fail
outright. The p95 mostly reflects requests exhausting an undersized budget
quickly, not resolving correctly.

**Cost — winners vs losers, optimistic retries=50:**

| Group                           | Count | p95               |
| ------------------------------- | ----- | ----------------- |
| Winners (booking created)       | 50    | 3.07 s            |
| All requests (winners + losers) | 500   | 5.63 s            |
| Total retries logged            | 2,476 | ~4.95 per request |

**Why jitter alone didn't move the failure count.** Exponential backoff with
full jitter was added between the two `retries=5` runs; the failure count
stayed at 491 in both. Jitter solves lockstep — contenders retrying at the
same instant and recolliding immediately. It does nothing about total attempts
allowed. At a 500:50 contention ratio, a losing request needs to keep checking
back in until one of 50 slots is actually claimed; well-spaced retries still
run out if there are only 5 of them. Raising the budget, not the spacing, is
what fixed it.

**Caveat.** Only the 50-seat level has been re-measured this session with the
tuned budget and real script output. 1-seat and 10-seat optimistic figures are
not yet re-verified under `retries=50` and should not be cited until they are.

**Variance.** An earlier, informally-cited p95 for this same 50-seat
configuration (~6.82s) does not match this session's measured 5.63s. Consistent
with Day 9's finding that single runs aren't measurements — median of three,
with range recorded, before either number goes in a final comparison table.

## Day 11

**Goal:** Measure `SERIALIZABLE` isolation against the three earlier strategies.
The code shape matches optimistic locking — abort on conflict, back off, retry —
but Postgres detects the conflict (SQLSTATE `40001`) instead of a version column.

Environment: unchanged from Days 9–10 — macOS, Docker Desktop, 500 VUs, one
iteration per VU, `MaxConns = 50`, k6 co-located, retry budget 50. All figures
from `scripts/bench.sh`.

**Correctness — all contention levels:**

| Seats | Bookings | Sold out | `available_units` | Invariant     |
| ----- | -------- | -------- | ----------------- | ------------- |
| 1     | 1        | 499      | 0                 | 0 + 1 = 1 ✓   |
| 10    | 10       | 490      | 0                 | 0 + 10 = 10 ✓ |
| 50    | 50       | 450      | 0                 | 0 + 50 = 50 ✓ |

Every seat sold at every level, so no request gave up while seats remained —
the Day 10 failure mode (invariant holding while 41 seats went unsold) did not
occur. No 5xx responses.

**Cost — p95 latency, all four strategies:**

| Contention | Seats | single-statement | `FOR UPDATE` | optimistic (r=50) | `SERIALIZABLE` |
| ---------- | ----- | ---------------- | ------------ | ----------------- | -------------- |
| High       | 1     | 311 ms           | 4.26 s       | [TODO]            | 348 ms         |
| Medium     | 10    | 619 ms (n=3)     | 5.23 s (n=3) | [TODO]            | 1.31 s         |
| Low        | 50    | 873 ms           | 5.60 s       | 5.63 s            | 2.78 s         |

`SERIALIZABLE` relative to single-statement: 1.1× → 2.1× → 3.2× as seats rise.

**Cost — distribution per level (`SERIALIZABLE`):**

| Seats | req/s | Wall time | min    | median | p95    | max    | Winners p95         |
| ----- | ----- | --------- | ------ | ------ | ------ | ------ | ------------------- |
| 1     | 1,212 | 0.4 s     | 293 ms | 327 ms | 348 ms | 377 ms | 293 ms (one winner) |
| 10    | 337   | 1.5 s     | 495 ms | 1.27 s | 1.31 s | 1.39 s | 1.13 s              |
| 50    | 175   | 2.9 s     | 1.04 s | 2.74 s | 2.78 s | 2.82 s | 2.58 s              |

Medium-contention throughput: single-statement 720 req/s, `SERIALIZABLE` 337,
`FOR UPDATE` 82.

**Retries:** 334 `40001` aborts at medium contention — 0.67 per request, about
33 per successful booking. High and low were not captured (see tooling below).

**Why it lands between the two.** Reads never block under `SERIALIZABLE`. A
request whose snapshot already shows 0 seats returns sold out without touching a
lock, so the fast path that `FOR UPDATE` removed survives — which is why the
1-seat run is within 12% of single-statement while `FOR UPDATE` was 13.7× slower.

A request whose snapshot still shows a seat pays both costs. Its `UPDATE` blocks
on the row lock held by the current writer, like `FOR UPDATE`. When that writer
commits, Postgres aborts the waiting transaction with `40001` rather than letting
it proceed, and it retries like optimistic. Single-statement under
`READ COMMITTED` waits on the same lock but then re-reads the row and continues;
`SERIALIZABLE` throws the whole transaction away.

**The ratio runs the opposite way to `FOR UPDATE`.** `FOR UPDATE` went from 13.7×
to 6.4× as seats rose; `SERIALIZABLE` goes from 1.1× to 3.2×. The two charge
different requests. `FOR UPDATE` puts every request — rejections included — in
the lock queue, so its relative cost is highest when almost everything is a
rejection. `SERIALIZABLE` charges only the requests whose snapshot still showed a
seat, and there are more of those when there are more seats.

**Losing is not free for everyone.** At medium contention winners had p95
1.13 s against 1.31 s overall. A request that starts while seats remain must
block, abort, back off and retry before a fresh snapshot tells it the seats are
gone. The cheap rejection only applies to requests arriving after sell-out.

**Tooling fault found during this session.** `bench.sh` started the server with
`go run`, which compiles and runs the binary as a child process. The script
killed the `go run` wrapper and left the real server running, so its retry total
was only logged when the next run found it on port 8080. The 334 above was
printed at the start of the 1-seat run but belongs to the 10-seat server (PID
8094), which the 1-seat run killed before starting its own. The 1- and 50-seat
totals were never logged. Day 10's 2,476 was captured by the same script and
carries the same doubt.

Latency and correctness figures are unaffected: every run reset the database and
was served by a `SERIALIZABLE` server.

Fixed by building once and running `bin/api` directly, so `$!` is the server,
and printing the retry line from that run's own log.

A separate failure: `make reset` intermittently failed with `migrate: EOF`. On
first boot the Postgres image runs a temporary init server that listens only on
the Unix socket, so `pg_isready` (which used the socket) passed before TCP was
available. Fixed with `pg_isready -h 127.0.0.1`.

**Variance.** Every `SERIALIZABLE` figure is a single run. Days 9–10 saw p95 on
identical code vary by up to 2.5×. `SERIALIZABLE` sits below optimistic at 50
seats (2.78 s vs 5.63 s), but both are single runs and Day 10 already recorded
5.63 s and 6.82 s for the same configuration. Not a finding until both are
medians of three.

**Open:**

- Optimistic (retries=50) at 1 and 10 seats
- `SERIALIZABLE` medium contention ×3, and retry totals at 1 and 50 seats
- Optimistic retry total at 50 seats, re-measured with the fixed script

## Day 12

**Goal:** Prove, against a real Postgres, the claims the fake store can't reach:
constraint mapping, transaction rollback, `SERIALIZABLE` conflict detection
(including `40001` at commit), and correctness of all four strategies and of
idempotency under concurrency.

Environment: Testcontainers for Go v0.44.0, `postgres:16-alpine`, real
migrations from `migrations/`, `scripts/seed.sql` re-applied before every test.
One container per package, started in `TestMain`. pgxpool default size. All
concurrency tests run with `-race`. Shared harness in `internal/testdb`, behind
`//go:build integration`, so `make test` still runs without Docker.

**Suite:**

| Package | Test                                               | Proves                                                                      | Runs | Result  |
| ------- | -------------------------------------------------- | --------------------------------------------------------------------------- | ---- | ------- |
| storage | `TestDecrementAvailabilityRejectsOverbooking`      | `chk_availability` maps to `ErrSoldOut`; failed call writes nothing         | 1    | ✓       |
| storage | `…UnknownUnit` (decrement, get, optimistic)        | missing unit is `ErrUnitNotFound`, never a conflict                         | 1    | ✓       |
| storage | `TestDecrementAvailabilityOptimisticStaleVersion`  | stale version is `ErrVersionConflict`; stale write doesn't land             | 1    | ✓       |
| storage | `TestTx*` × `WithTx`, `WithSerializableTx`         | rollback, commit, isolation until commit, nesting refused                   | 1    | ✓       |
| storage | `TestSerializableConcurrentUpdateBlocksThenAborts` | T2 waits on T1's row lock, then gets `40001`                                | 10   | 10/10 ✓ |
| storage | `TestSerializableWriteSkew` (both commit orders)   | read-write cycle fails the loser at `COMMIT`                                | 10   | 20/20 ✓ |
| booking | `TestCreateUnderContentionAllStrategies`           | 50 workers, 10 seats: exactly 10 booked, rest `ErrSoldOut`, invariant holds | 5    | 20/20 ✓ |
| booking | `TestCreateIdempotentConcurrentSameKey`            | 50 workers, one key: exactly one booking; a late retry replays it           | 13   | 13/13 ✓ |

**Coverage:** `internal/storage` [TODO: `go test -tags=integration -cover ./internal/storage/`].

**Constraint-name experiment.** With `"chk_availability"` misspelled in
`DecrementAvailability`:

- `go test ./internal/booking/` — all green, 100% coverage.
- The storage integration test fails with
  `decrement availability: ERROR: new row for relation "inventory_units" violates check constraint "chk_availability" (SQLSTATE 23514)`.

The unrecognised error falls through the service unchanged, so a sold-out request
would return 500 instead of 409. The fake store can't catch this: it returns
whatever error the test configures, so the mapping is never exercised.

**Where `SERIALIZABLE` fails:**

| Scenario                             | Conflict         | Where `40001` fires  | Winner          |
| ------------------------------------ | ---------------- | -------------------- | --------------- |
| Two transactions update the same row | write-write      | the waiting `UPDATE` | first updater   |
| Each reads one row, writes the other | read-write cycle | the loser's `COMMIT` | first committer |

In the write-skew case no statement fails, so only the commit-time mapping in
`WithSerializableTx` turns it into `ErrSerializationFailure`.

**Contention test, per strategy (5 runs, `-race`):**

| Strategy              | Duration    | Retries |
| --------------------- | ----------- | ------- |
| single-statement      | 1.12–1.77 s | 0       |
| `FOR UPDATE`          | 1.12–1.67 s | 0       |
| optimistic (r=50)     | 3.44–6.25 s | 278–307 |
| `SERIALIZABLE` (r=50) | 1.18–1.86 s | 115–126 |

These are harness numbers, not benchmarks: the pool caps concurrency at
`max(4, NumCPU)`, so real contention is pool size, not 50. Don't compare them
with the `bench.sh` tables. The stable ~2.5× gap between optimistic and
`SERIALIZABLE` retries is noted as an open question below, not a finding.

**Idempotency split:** every one of 13 runs was `fresh=1 replays=0 in-flight=49`.
The start gate releases all 50 at once, so every loser checks the key before the
winner completes. The concurrent part of the test never exercised replay; a
sequential late retry was added to cover it.

**Timing:** container start 4–13 s on Docker Desktop, dominating every run.
Storage tests take ~0.6 s in total, write skew ~50 ms per commit order,
block-then-abort ~80 ms after warm-up.

**Open:**

- `make test-integration` and a CI job running it, plus `go vet -tags=integration`
- Storage coverage figure
- Why optimistic retries ~2.5× more than `SERIALIZABLE` under the same load:
  the read-to-write gap, the extra disambiguation read, or both

## Day 13

**Goal:** Run the integration tests in CI, fill the empty cells in the strategy
comparison, and record the decision in ADR-002.

**CI.** Added `make test-integration` and `make vet` (vet runs with and without
the `integration` tag), plus an `integration` job on `ubuntu-latest`. Lint runs
with `--build-tags=integration`. Also removed an unused Postgres service from the
`build` job, and CI now reads the Go version from `go.mod`.
Integration job duration: [TODO: from the Actions tab].

**The one-session table.** All four strategies, 10 seats, 500 VUs, three runs
each, back to back (14:04–14:07), median shown:

| Strategy          | p95    | p95 range       | req/s | Retries |
| ----------------- | ------ | --------------- | ----- | ------- |
| single-statement  | 864 ms | 792 ms – 1.94 s | 473   | 0       |
| `SERIALIZABLE`    | 865 ms | 811 – 905 ms    | 539   | 144     |
| optimistic (r=50) | 1.91 s | 1.74 – 1.92 s   | 244   | 1,070   |
| `FOR UPDATE`      | 2.14 s | 2.00 – 2.27 s   | 202   | 0       |

Every run sold all 10 seats, and the invariant held.

**Two tiers, not four places.** Single-statement and `SERIALIZABLE` are tied.
Optimistic and `FOR UPDATE` are close to each other, about 2.2–2.5× slower.

**Earlier comparisons are withdrawn.** Days 9–11 put single-statement about 2×
ahead of `SERIALIZABLE`, and `FOR UPDATE` 8.5× behind single. Those tables mixed
numbers from different days. The same `SERIALIZABLE` code measured 1.31 s p95 on
Day 11 and 729–865 ms today. The clue was the fastest request: 495 ms on Day 11,
against 47–97 ms today. That points at the machine, not the strategy.

**Noise floor.** Two `SERIALIZABLE` sessions 20 minutes apart:

| Session | Median p95 | Median req/s | Median retries |
| ------- | ---------- | ------------ | -------------- |
| 13:48   | 729 ms     | 589          | 100            |
| 14:06   | 865 ms     | 539          | 144            |

That's about 19% drift on identical code. On this laptop, gaps under ~20% are noise.

**Warm-up.** The first run of the session (single-statement, run 1) had p95 1.94 s,
against 792 and 864 ms for runs 2 and 3. The median absorbs it. Future sessions
should start with one untimed run.

**Optimistic, previously empty cells:**

| Seats | p95    | req/s | Retries |
| ----- | ------ | ----- | ------- |
| 1     | 417 ms | 999   | 0       |
| 10    | 1.33 s | 362   | 1,087   |

These are single runs, from before the one-session batch.

**The 1-seat run tested no contention.** The only winner took 34 ms, which was
also the minimum latency of all 500 requests, and there were 0 retries. It
finished before most requests reached the database, so the row measures how fast
sold-out requests are rejected, not how conflicts are handled.

**Retries.** Optimistic retried about 7× more than `SERIALIZABLE` under the same
load (1,070 against 144). The integration harness showed the same direction at
2.5×. Still unexplained.

**Decision:** see `ADR-002-concurrency-control.md`.

## Day 14

**Goal:** Run Kafka locally, learn its behaviour from experiments rather than
docs, and understand why "write to the database, then publish" is broken.

Environment: `apache/kafka:4.0.0` in Docker Compose, single node in KRaft mode
(broker and controller in one process), heap capped at 512 MB. Topic
`booking-events`, 3 partitions, replication factor 1. Go client: franz-go.
Demo program: `cmd/kafkademo`.

**Experiment 1: the key decides the partition.**

| Key                             | CLI producer (Java) | Go producer (franz-go) |
| ------------------------------- | ------------------- | ---------------------- |
| booking-1, booking-2, booking-5 | 0                   | 0                      |
| booking-3, booking-4            | 2                   | 2                      |
| booking-6                       | 1                   | —                      |

Both clients hash keys the same way, so services written in different languages
agree on where each booking's events go. Within a partition, order held every
time. Across partitions it didn't: the consumer printed `booking-3 paid` (the
last message produced) before `booking-1 created` (the first).

Offsets are counted per partition. Partition 2 ran 0–8 while partition 0 ran
0–17, so an offset means nothing without its partition.

**Experiment 2: a crash before the commit means redelivery.**

Auto-commit disabled, and the process exits between printing and committing.

| Partition | Committed after crash | Log end | Lag |
| --------- | --------------------- | ------- | --- |
| 0         | 12                    | 15      | 3   |
| 2         | 5                     | 7       | 2   |

On restart, the same 5 messages (offsets 12–14 and 5–6) were delivered again,
and after that commit the lag was 0 on every partition. This is at-least-once
delivery.

The first restart printed nothing. The crashed consumer never left the group,
because `os.Exit` skipped `Close()`, so its partitions stayed assigned to a dead
member until its session timeout expired (about 45 s by default).

**Experiment 3: rebalancing.**

- One consumer got `[0 1 2]`. When a second joined, the first gave up only
  partition 2 and kept 0 and 1, a 2/1 split. The split is sticky: partitions
  move only when they have to.
- franz-go's default rebalance is cooperative: the first consumer kept
  processing 0 and 1 while 2 was being moved.
- On Ctrl-C (a clean shutdown), the leaving consumer's partitions reached the
  other one in about a second. After the crash in experiment 2, they sat idle
  for about 45 s. Same partitions, same group; only the shutdown differed.

**`--from-beginning` vs committed offsets.** A group with a committed position
ignores `--from-beginning`. That flag, and franz-go's `ConsumeResetOffset`, only
apply when the group has no committed offset. A group re-run after committing
read 5 new messages, not all 13.

**Open:**

- Check `outbox_events` against the columns the outbox pattern needs,
  especially a key column (`aggregate_id`) for per-booking ordering

## Day 15

**Goal:** Make "a booking and its event" all-or-nothing with a transactional
outbox, publish events to Kafka with a relay, and prove a relay crash can cause
duplicates but never a lost event.

**Design:**

- `booking.created` is written to `outbox_events` in the same transaction as the booking, for all four strategies, through one shared helper.
- A separate program, `cmd/relay`, polls with `SELECT … WHERE published_at IS NULL ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED`.
- It publishes each event to `booking-events` with key = `booking_id` and headers `event_id` and `event_type`, then marks the batch published, all in one transaction.
- A publish failure is recorded (`attempt_number`, `error`) and the batch stops there, so a later event for the same booking can't overtake it.
- Batch size 100, poll interval 500 ms, delivery timeout 10 s, `MaxConns = 2`.

**Tests added:**

| Package                             | Proves                                                                                                                                                       | Result                |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------- |
| booking (unit)                      | a booking writes one correct `booking.created`; a failed event write fails the booking                                                                       | ✓                     |
| booking (integration)               | all 4 strategies write booking + event together; an injected outbox failure leaves neither                                                                   | 8/8 ✓                 |
| storage (integration)               | fetch returns unpublished events oldest-first within the limit; mark and record-failure behave; `SKIP LOCKED` hands two relays disjoint rows without waiting | 15/15 ✓ (×3)          |
| relay (integration, fake publisher) | publish-all; failure recorded and batch stopped; retry next run in order; empty; batch size; `Run` drains a backlog and stops on cancel                      | 18/18 ✓ (×3, `-race`) |

**`SKIP LOCKED`:** relay 1 locked events 1–2, relay 2 got exactly [3 4] in
21–149 ms against a 2 s deadline. After relay 1 committed without marking, all 4
were available again.

**Missing topic (accidental test):** after `make reset`, `booking-events` didn't
exist, and franz-go doesn't auto-create topics. Publishes failed with
`UNKNOWN_TOPIC_OR_PARTITION` about every 10.5 s (the 10 s delivery timeout plus
the 500 ms poll). The relay kept running and lost nothing. Once the topic was
created, the pending event was published without a restart. `make reset` now
creates the topic explicitly (3 partitions).

**End-to-end latency:** booking to `published_at`, about 0.1–0.2 s, within one poll interval.

**Crash after publishing, before marking:**

| Step                           | Outbox (`published_at`)                       | Kafka      |
| ------------------------------ | --------------------------------------------- | ---------- |
| 3 bookings, relay stopped      | 3 pending                                     | 0 messages |
| relay crashes after publishing | 3 pending (transaction rolled back)           | 3 messages |
| normal relay runs              | 3 published (same timestamp, one transaction) | 6 messages |

The duplicates carry the same `event_id` (1, 2, 3), key and payload, on the same
partition, at new offsets. Per-partition order held: partition 0 shows
event 1, 3, 1, 3. `attempt_number` stayed 0, because a crash isn't a recorded
publish failure.

**Result:** no event lost, duplicates as designed. Consumers must deduplicate on
`event_id`.

**Open:**

- Idempotent consumer (Day 16)
- Dead-letter handling: one permanently failing event blocks everything behind it
- Server-side re-claim for a released idempotency key (the Block 0 note)
- Per-booking ordering if more than one relay runs

## Day 16

**Goal:** Turn at-least-once delivery into effectively-once processing: every
event takes effect exactly once, even when it is delivered more than once.

**Design:**

- **`processed_events (consumer, event_id)`** is the primary key. Each consumer tracks its own progress, so two services consuming the same event don't block each other.
- **`ClaimEvent`** is `INSERT … ON CONFLICT DO NOTHING`. 1 row inserted means new, 0 rows means duplicate. There's no `SELECT` first, so no check-then-act race.
- **`Process`** claims the event and runs the handler **in one transaction**. A handler failure rolls back the claim, so the event is retried.
- **`Run`** polls, then parses, processes and retries each record. It commits the Kafka offset **only after** the database commit, using `DisableAutoCommit` and `BlockRebalanceOnPoll`.
- **Failures:**
  - poison (bad headers, bad payload, unknown version): logged with partition and offset, then skipped;
  - transient: the same record is retried with backoff from 100 ms to 6.4 s.
- **`notifications`** has a `UNIQUE (booking_id, kind)` index as a second line of defence, with the insert using `ON CONFLICT DO NOTHING`.
- **Row retention:** rows must live at least as long as Kafka retention (168 h, 7 days), plus a margin.

**Tests added:**

| Package                | Proves                                                                                                                                                          | Result                |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------- |
| consumer (unit)        | `parseEvent`: valid records; missing or invalid `event_id`, `event_type`, or overflow → `ErrPoisonMessage`; the error names partition and offset                | ✓                     |
| storage (integration)  | `ClaimEvent`: new vs duplicate; per consumer; different events are independent; a rolled-back claim is forgotten; 20 concurrent claims → exactly 1 winner       | 18/18 ✓ (×3, `-race`) |
| consumer (integration) | `Process`: new runs the handler once; a duplicate skips it; a handler failure leaves no claim and is retried; the handler's own write rolls back with the claim | ✓ (×3, `-race`)       |

**End-to-end, one booking:** API → outbox → relay → Kafka → notifier → notification.

| Event | Booking → published | Published → processed |
| ----- | ------------------- | --------------------- |
| 1     | 0.4 s               | 15.9 s                |
| 2     | —                   | 0.03 s                |

The steady state is about 30 ms from Kafka to a committed notification. Event 1's
16 s is **unexplained**. It happened once, with the notifier already running for
about 3 minutes, and didn't recur in later runs. Investigate with franz-go
logging if it reappears.

**Experiment 1: duplicates from the relay.** 3 bookings published normally, then 3
more published twice (a relay crash after publishing, then a normal relay).

|                          | Count                                          |
| ------------------------ | ---------------------------------------------- |
| Bookings                 | 8                                              |
| Messages in Kafka        | 11 (8 + 3 duplicates)                          |
| Notifications            | 8                                              |
| `processed_events`       | 8                                              |
| `duplicate skipped` logs | 3: event_id 6, 7, 8, exactly the crashed batch |

**Experiment 2: the notifier crashes after the database commit, before the offset commit.**

|                                        | Result                                                 |
| -------------------------------------- | ------------------------------------------------------ |
| Notifications after the crash          | 10: the database work survived                         |
| Notifications after the normal restart | 10: nothing doubled                                    |
| Consumer group after restart           | `LAG 0` on all 3 partitions, log-end total 13 (11 + 2) |

The redelivery of events 9 and 10 (the `duplicate skipped` lines) and its delay
(expected up to the session timeout, since the crashed member never left the
group) were **not captured** in the log. The counts and the lag are consistent
with it, but it isn't directly observed.

**Result:** both sources of duplicates are absorbed. 11 messages → 8 effects in
experiment 1, and a consumer crash → no double effect in experiment 2.

**Open:**

- Dead-letter topic for poison messages (they're only logged today)
- An upper limit on retries (a long outage blocks the partition, and rebalances with it)
- Cleanup job for `processed_events` (≥ 7 days + margin)
- Unit tests for the notifier handler (unknown type, bad payload, duplicate notification)
- The 16 s first-message delay
- Capture the redelivery log in experiment 2

## Day 17

**Goal:** A payment provider to fail against, the booking state machine that a
saga needs, and the saga's design (ADR-003).

**`cmd/paymock`:** `POST /charges` is idempotent on the `Idempotency-Key` header.
Knobs: `LATENCY_MS`, `DECLINE_RATE`, `LOST_RESPONSE_RATE`, `LOST_RESPONSE_DELAY_MS`.

| Test                                                  | Result                                                                                             |
| ----------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| same key twice                                        | same `charge_id` (`dff5c360…`), second response has `Idempotent-Replay: true`                      |
| same key after a paymock restart                      | **new** charge (`055a52eb…`): keys live in memory only. A real provider persists them              |
| lost response (`--max-time 2`, delay 5 s), then retry | first call timed out after 2005 ms with 0 bytes; the retry replayed the stored success immediately |
| a lost response without a client timeout              | answered after 5 s: a response is only "lost" if the caller gives up                               |

**Migration 9:** `chk_booking_status` gains `payment_pending`. Up → 4 statuses,
down → 3, up → 4. The down migration deliberately fails while any booking is
`payment_pending`.

**State machine:** `pending → payment_pending → confirmed / cancelled`, plus
`pending → cancelled` (expiry). Enforced by `checkTransition` (table-driven unit
test, 9 cases) and applied by `TransitionBookingStatus`, a compare-and-set
(`WHERE booking_id = $1 AND booking_status = $from`).

**ADR-003 (proposed):** the saga as consumer + worker, with the charge outside
any transaction and compensation tied to the transition.

## Day 18

**Goal:** Build ADR-003 and prove its failure table.

**Built:**

- **The payments consumer (`internal/payments`, group `payments`):** `booking.created` → `pending → payment_pending`.
- **`booking.Transition`:** checks the move is legal, then compare-and-set. It takes a one-method `transitioner` interface, so a plain `*storage.Store` works without an adapter.
- **Storage:**
  - `ClaimDuePayments`: `FOR UPDATE SKIP LOCKED` plus a lease on `next_attempt_at` (migration 10, with a partial index on `payment_pending`);
  - `InsertPayment`;
  - `ReleaseSeats`.
- **`Provider` interface and `HTTPProvider`:** HTTP → succeeded / declined / permanent error / transient error.
- **The worker:** claims a batch, charges it **in parallel**, and applies each outcome in one transaction.
- **`cmd/payments`:** runs the consumer and the worker together.

**Design change during implementation:** a batch claimed with a 30 s lease and
charged sequentially (10 s timeout each) would outlive its leases from the
fourth booking on. So charges within a batch now run in parallel, and a batch
finishes in about one timeout.

**Settings:** batch 10, poll 500 ms, lease 30 s, charge timeout 10 s, escalation
after 30 min, `MaxConns` = batch + 4.

**Tests added:**

| Package                     | Proves                                                                                                                                                                       | Result          |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------- |
| booking (unit)              | `Transition`: an illegal move never reaches the store; a race passes through as `ErrStatusConflict`; a legal move reaches the store unchanged                                | ✓               |
| payments (integration)      | consumer: pending → payment_pending; not pending → `nil`, unchanged; unknown booking or bad payload → poison; other event types ignored                                      | ✓ (×3, `-race`) |
| storage (integration)       | `ClaimDuePayments`: only `payment_pending`; lease hides a booking; expired lease makes it due; `updated_at` untouched; oldest first within limit; concurrent claims disjoint | ✓ (×3, `-race`) |
| storage (integration)       | `InsertPayment`: succeeded and failed rows; second success → `ErrDuplicateSuccessfulPayment`; several failures allowed                                                       | ✓               |
| storage (integration)       | `ReleaseSeats`: gives seats back and bumps version; over-release → `ErrOverRelease`, row unchanged; unknown unit; non-positive qty                                           | ✓               |
| payments (unit, `httptest`) | 11 status/body cases; rejection body in the error; request matches the contract; timeout, connection refused and cancelled context are transient                             | ✓               |

**End-to-end scenarios:**

| Scenario                                           | Evidence                                                                                                       | Result                                                                                                                                                             |
| -------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| success                                            | `booking.created` published 15:41:02.259 → confirmed 15:41:03.143 → `booking.confirmed` published 15:41:03.389 | about 0.9 s from event to confirmed; one `succeeded` payment with the paymock's `charge_id`                                                                        |
| decline (`DECLINE_RATE=1`)                         | seats 9 → 8 (booking) → 9 (release)                                                                            | `cancelled`, reason `insufficient_funds`; one `failed` payment; `booking.cancelled` published; `cancelled_at` = `failed_at` to the microsecond, so one transaction |
| lost response (`LOST_RESPONSE_RATE=1`, delay 20 s) | paymock: charge `88f2fd77…` at 22:47:21.517 (`lost_response`), the same `88f2fd77…` at 22:47:51.687 (`replay`) | worker timed out at 10.2 s and left the booking `payment_pending`; confirmed 30.2 s after the first attempt (the lease); **one charge, one payment row**           |

**Not yet run:** the provider down for a while (expect delay, never
cancellation), and the worker crashing mid-charge (expect recovery after the
lease). Both rely on the same lease + replay mechanism shown above.

**Open:**

- Worker integration tests with a fake `Provider`
- The provider lookup endpoint for recovery (`GET /charges/{key}`)
- The expiry job (`pending → cancelled` plus release)
- An escalation metric and alert; repeated escalation logs every lease period
- A dead-letter topic; refunds; card retries with a per-attempt key

## Day 19

**Goal:** Make payment retries resilient: exponential backoff with full jitter,
and a circuit breaker around the payment provider (the original plan's Day 26).
Prove both during a provider outage.

**Settings:**

| Setting                   | Value                  | Constraint                        |
| ------------------------- | ---------------------- | --------------------------------- |
| charge timeout            | 10 s                   | < lease                           |
| lease                     | 30 s                   | always included in full           |
| backoff base / max        | 5 s / 5 min            | lease + max ≪ escalation (30 min) |
| breaker: trip after       | 5 consecutive failures |                                   |
| breaker: open for         | 30 s                   | > charge timeout                  |
| breaker: half-open probes | 1                      |                                   |

**Backoff:**

- Migration 11 adds `bookings.payment_attempts`.
- Each claim increments it, and sets `next_attempt_at` to
  `now + lease + random() × min(max, base × 2^attempts)`: full jitter,
  **added to** the lease.
- The first design took the **larger** of the lease and the backoff. That gave
  identical retry times for early attempts (no jitter at all, exactly when a
  herd forms), so it was replaced before implementation.

**Circuit breaker:**

- `BreakerProvider` decorates `HTTPProvider` (`sony/gobreaker/v2`). One
  instance is shared by the whole worker.
- Counts only transient provider failures: timeouts, refused connections,
  `5xx`, `408`, `429`.
- Declines, `ErrChargeRejected`, `ErrUnexpectedProviderResponse` and
  `context.Canceled` never count.
- A refused call is transient, so the booking waits for its next attempt.

**Tests added:**

| Package               | Proves                                                                                                                                                                                                                                                                                                                          | Result          |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------- |
| storage (integration) | claims count attempts (1, 2); delay stays within lease … lease+base (first attempt) and lease … lease+max (10 attempts, capped); across 20 bookings at attempt 10, delays exceed anything a first attempt could get                                                                                                             | ✓ (×3, `-race`) |
| payments (unit)       | breaker opens after 5 consecutive failures and stops calling the provider; declines, rejections, unrecognised responses and cancellations never open it; a success resets the count; recovers after the timeout; a failed probe reopens it; half-open allows exactly one probe, and others get a transient `ErrTooManyRequests` | ✓ (×3, `-race`) |

**Outage experiment:** paymock stopped, 6 bookings made at 18:38:43–45, paymock
restarted about 5½ minutes later.

| Time                                                     | Event                                                                                                          |
| -------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| 18:38:45.010 – 18:38:46.123                              | 6 charges reach the dead provider (`connection refused`)                                                       |
| 18:38:46.121                                             | breaker **closed → open** after the 5th failure. The 6th had already started, since the batch runs in parallel |
| 18:39:17, :39:50, :40:22, :41:01, :41:35, :42:35, :43:25 | **open → half-open → open**: one probe each, all failed                                                        |
| between probes                                           | every other call: `circuit breaker refused the call`, with no network call                                     |
| 18:44:25.843                                             | probe **succeeds** → breaker **closed**                                                                        |
| 18:44:25.857 – 18:45:13.180                              | all 6 confirmed, each when its own `next_attempt_at` came due (spread over 48 s)                               |

**Retries spreading out (jitter + exponential backoff):**

| Attempt | Retries landed between | Spread                |
| ------- | ---------------------- | --------------------- |
| 2       | 18:39:15 – 18:39:20    | about 5 s             |
| 3       | 18:39:50 – 18:39:58    | about 8 s             |
| 4       | 18:40:22 – 18:40:38    | about 16 s            |
| 5–6     | 18:41:01 – 18:42:58    | wider; rounds overlap |

A psql snapshot at attempt 2 showed `next_attempt_at` 11–19 s ahead: 6
bookings that failed together, retrying at different times.

**Calls to the provider during the outage:** 37 failed attempts in total, of
which only **13 reached the provider** (the first burst of 6, plus 7 probes).
Without the breaker, all 37 would have.

**Result:**

- 6 bookings `confirmed`, 1 succeeded payment each, 6 distinct `charge_id`s.
- No booking cancelled, none escalated (the outage stayed well under 30 min).
- No call made during the outage reached the provider, so each booking was
  charged exactly once.

**Observations:**

- **A breaker can't stop calls already in flight.** In a parallel burst, "trip
  after 5" still let 6 through.
- **Attempts rise while the breaker is open,** although no real call is made,
  because the claim counts the attempt before `Charge` is refused. It's
  harmless (it only lengthens the backoff), but it isn't a true count of calls.
- **Recovery is paced by each booking's schedule,** not by the breaker
  closing: 48 s from the breaker closing to the last confirmation.

**Open:**

- Validate the config at startup (`MaxBackoff > 0`, `Lease + MaxBackoff < EscalateAfter`, `OpenTimeout > charge timeout`)
- Skip claiming while the breaker is open, so attempts aren't burnt
- Consider a failure-ratio trip, for providers that fail only intermittently
- Breaker state as a metric, and an alert on "open"
- Remove or generalise the `pgErr.Position` debug branch in `ClaimDuePayments`

## Day 20

**Goal:** Build the saga's timeout path (the original plan's Day 23–25 "timeout
path", skipped on Days 17–18): expire `pending` bookings past a deadline and
release their seats. Prove it with a relay outage.

**Settings:**

| Setting                           | Value                        | Constraint                                                  |
| --------------------------------- | ---------------------------- | ----------------------------------------------------------- |
| deadline (`BOOKING_EXPIRE_AFTER`) | 15 min (60 s in experiments) | ≫ healthy insert → `payment_pending`; > a routine restart   |
| poll interval                     | 5 s                          |                                                             |
| batch size                        | 50                           | small: the batch holds hot inventory row locks until commit |
| clock                             | Postgres `now()`             | the clock that wrote `created_at`                           |

**Built:**

- **Migration 12:** partial index `idx_bookings_pending_created ON bookings (created_at) WHERE booking_status = 'pending'`.
- **`storage.ExpirePendingBookings`:** a CTE picks the oldest expired `pending` bookings (`FOR UPDATE SKIP LOCKED`), and one `UPDATE … FROM` cancels them with `failure_reason = 'expired'`, returning `booking_id`, `unit_id`, `customer_id` and `qty`.
- **`booking.Expirer`:** `checkTransition` once per batch; then, in one transaction, the query, seat releases summed per unit in `unit_id` order, and one `booking.cancelled` event per booking. No payment row.
- **`booking.CancelledPayload`:** the `booking.cancelled` schema moved from `payments` into `booking`, so the worker and the expirer write the same shape.
- **`cmd/payments`:** runs the expirer as a third goroutine; reads `BOOKING_EXPIRE_AFTER` and refuses to start on a bad value; one more pool connection.

**Tests added:**

| Package               | Proves                                                                                                                                                                                                                                                                                                                                                                                                      | Result    |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------- |
| storage (integration) | only `pending` bookings past the deadline expire (a fresh `pending`, and old `payment_pending` / `confirmed` / `cancelled`, are untouched); status, `failure_reason` and `cancelled_at` are set; oldest first within the limit; a second call finds nothing; two concurrent calls are disjoint and cover everything                                                                                         | ✓ `-race` |
| booking (integration) | seats come back summed per unit, on every unit; exactly one `booking.cancelled` event per booking with reason `expired`; no payment rows; fresh and `payment_pending` bookings keep status and seats; a second run changes nothing; an outbox failure rolls back the transition and the release; two concurrent expirers with small batches across two units release every seat exactly once, with no error | ✓ `-race` |

**Checked that the tests catch bugs:**

| Deliberate bug                                | Caught by                                                                      |
| --------------------------------------------- | ------------------------------------------------------------------------------ |
| seats released outside the transaction        | `TestExpirerWritesNothingWhenEventFails` (available 10, want 8)                |
| `FOR UPDATE SKIP LOCKED` removed from the CTE | `TestExpirerConcurrentRunsReleaseEachSeatOnce` (`ErrOverRelease`, seats wrong) |

The storage-level concurrency test did **not** catch the second bug: its two
calls rarely overlap in time. The booking-level test loops with small batches,
so it does.

**Experiment: relay down past the deadline.** `BOOKING_EXPIRE_AFTER=60s`, paymock
with no declines and no lost responses, unit `2222…` (10 seats).

| Time              | Event                                                                                                                      |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------- | --- |
| 19:50:08          | control booking, relay running: `confirmed` by the next check (14 s later)                                                 |
| —                 | relay stopped                                                                                                              |
| 19:50:53–57       | batch A: 3 bookings                                                                                                        |
| 19:51:55–57       | batch B: 2 bookings                                                                                                        |
| ~19:52:07         | check 1: A (70–73 s old) `cancelled` / `expired`; B (9–11 s) `pending`                                                     |
| 19:52:07–19:52:55 | relay restarted (time not recorded; after check 1, and before batch B turned 60 s old, since B was confirmed, not expired) |     |
| ~19:53:03         | check 2: all published; B `confirmed`; A still `cancelled`                                                                 |

| Check | `available_units`      | outbox unpublished                         | payments                  |
| ----- | ---------------------- | ------------------------------------------ | ------------------------- |
| 1     | 7 = 10 − 1 − 3 − 2 + 3 | 5 `booking.created`, 3 `booking.cancelled` | 1 succeeded (control)     |
| 2     | 7 = 10 − 3 confirmed   | 0                                          | 3 succeeded (control + B) |

T4 on relay restart: not saved before the terminal was closed. The indirect
evidence is above: batch A has 0 payment rows, and every `booking.created`
was processed. The chaos harness now keeps one log file per process start,
so this can't be lost again.

**Result:**

- Batch A expired, released its seats, and was **never charged**: 0 payment rows. Its late `booking.created` lost the race to the expirer.
- Batch B, inside the deadline, carried on and was confirmed.
- Invariants held: available = total − confirmed (S2); no payment for a cancelled booking (S3); nothing left `pending` or `payment_pending` (L1); every event published (E1).

**Observations:**

- **The outbox kept the expiry events while the relay was down.** The expirer's transaction only needs Postgres, so it keeps working through a relay or Kafka outage; the events go out when the relay returns.
- **Batch A's `booking.created` was published after it had already expired.** That is exactly the Q4 race, happening for real.
- **Batch B was created 62 s after A, not the planned 30 s.** It worked only because the relay came back before B turned 60 s old.
- **The control run doesn't measure healthy latency:** "confirmed within 14 s" is only when I looked.

**Open:**

- Measure healthy insert → `payment_pending` at p99 under load (the chaos test)
- Expiry count as a metric; alert on a spike (a pipeline outage shows up as expiries)
- Customer cancel during `payment_pending` (needs the provider lookup)

## Days 21–22

**Goal:** Automate the chaos test from the original plan (Days 27–28): real
binaries under load, the relay and `cmd/payments` killed mid-run, 30% declines
and 10% lost responses, then check every invariant after the system drains.
Scale it to 10,000 bookings.

**Built:**

- **Paymock ledger:** `GET /charges` lists every charge with `idempotency_key`,
  `booking_id`, `lost_response` and `calls`. `POST /charges` answers are
  unchanged (`chargeRecord` embeds `chargeResponse`; only the embedded part is
  sent).
- **The chaos harness (`internal/chaos`, build tag `chaos`):** builds the four
  binaries once, starts them with `os/exec`, sends paced load, kills processes
  with SIGKILL on a schedule, waits for the drain, checks the invariants.
  `CHAOS_BOOKINGS` and `CHAOS_RATE` set the size.
- **Worker: batches → slots.** `Worker.Run` keeps up to `MaxInFlight` charges
  in flight and refills a slot as soon as it frees, instead of claiming a batch
  and waiting for all of it. `BatchSize` became `MaxInFlight` (still 10).
- **The worker's first tests** (with a fake provider), closing the Day 18 open
  item.
- **Deferred from Day 20:** the worker uses `booking.CancelledPayload`;
  `make up` waits for the healthchecks (`--wait`).

**Invariants checked after every run:**

| Group    | Checks                                                                                                                                                             | Source                        |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------- |
| Safety   | S1 available ≥ 0 · S2 available = total − confirmed seats · S3 / S3b / S4 payments per booking · S7/E2 one created + one matching outcome event                    | Postgres                      |
| Provider | S5 one successful charge per booking · S6 charged ⇔ confirmed · S6b declined ⇔ cancelled as declined                                                               | Paymock ledger vs Postgres    |
| Events   | E1 all published · E3/E4 outbox ids = `processed_events` ids for `payments`, both directions                                                                       | Postgres                      |
| Liveness | L1 nothing `pending` or `payment_pending`                                                                                                                          | Postgres                      |
| Validity | M1 every 201 exists, DB count within 201s … 201s + unknown · M2 ≥ 1 of each: relay restart, payments restart, lost response, replay, decline, expiry, confirmation | Load result, ledger, Postgres |

**Chaos schedule** (triggers are shares of the load, so they land mid-traffic
at any size):

| At  | Fault                                                     |
| --- | --------------------------------------------------------- |
| 10% | SIGKILL `cmd/payments`, back after 5 s                    |
| 25% | SIGKILL the relay, back after 5 s                         |
| 40% | relay down until 5 more bookings expire (state, not time) |
| 75% | SIGKILL `cmd/payments`, back after 5 s                    |

Settings: `BOOKING_EXPIRE_AFTER=30s`, `DECLINE_RATE=0.3`, `LOST_RESPONSE_RATE=0.1`,
20 units with 2× the seats the load needs.

**Tests added:**

| Package                | Proves                                                                                                                                                                                                                                                               | Result          |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------- |
| payments (integration) | a hung charge holds only its own slot while the others confirm (**fails on the batch design**); never more than `MaxInFlight` charges at once, and they do overlap; `Run` returns only after charges in flight end, and an unanswered charge stays `payment_pending` | ✓ (×3, `-race`) |
| chaos (`-tags=chaos`)  | all invariants above, under the schedule above                                                                                                                                                                                                                       | ✓ 1k and 10k    |

**Worker fix, 1,000 bookings at 5/s** (same chaos, same 10-way concurrency;
only the scheduling changed):

| Machine     | Worker | Drain after load | Whole test | Confirmed / declined / expired | Charges / calls | Lost = retried | Violations |
| ----------- | ------ | ---------------- | ---------- | ------------------------------ | --------------- | -------------- | ---------- |
| container   | batch  | 290.8 s          | 521 s      | 607 / 266 / 128                | 873 / 934       | 61 = 61        | 0          |
| container   | batch  | 300.0 s          | 536 s      | 609 / 260 / 132                | 869 / 935       | 66 = 66        | 0          |
| container   | slots  | **78.9 s**       | 300 s      | 578 / 269 / 154                | 847 / 914       | 67 = 67        | 0          |
| MacBook Pro | slots  | **71.2 s**       | 300 s      | 592 / 241 / 168                | 833 / 888       | 55 = 55        | 0          |
| MacBook Pro | slots  | **79.4 s**       | 308 s      | 588 / 246 / 167                | 834 / 899       | 65 = 65        | 0          |

Drain −73 to −75%. Provider calls during a batch-design run: about 1.9/s
against 5 bookings/s arriving.

**10,000 bookings at 25/s** (slots, `MaxInFlight` 10):

| Machine     | Load  | Work left 30 s after load | Drain after load | Whole test | Confirmed / declined / expired | Charges / calls | Lost = retried | Violations |
| ----------- | ----- | ------------------------- | ---------------- | ---------- | ------------------------------ | --------------- | -------------- | ---------- |
| container   | 400 s | 4,289                     | 334.7 s          | 767 s      | 6,498 / 2,771 / 732            | 9,269 / 9,902   | 633 = 633      | 0          |
| MacBook Pro | 400 s | 3,304                     | 317.9 s          | 789 s      | 6,312 / 2,543 / 1,146          | 8,855 / 9,452   | 597 = 597      | **0**      |

In every run, charges = confirmed + declined exactly: no expired booking
reached the provider.

**Mac 10k timeline:**

| Time    | Event                                                |
| ------- | ---------------------------------------------------- |
| 40.1 s  | kill payments                                        |
| 100.0 s | kill relay                                           |
| 160.1 s | relay down until 5 more expire (549 already expired) |
| 192.3 s | relay back (32 s outage)                             |
| 300.0 s | kill payments                                        |
| 400.1 s | load finished: 10,000 created, 0 errors, 0 unknown   |
| 430.7 s | 3,304 units of work left                             |
| 674.3 s | 30 left                                              |
| 718.0 s | drained                                              |

**Observations:**

- **A SIGKILLed consumer blocks its partitions for about 45 s.** It never
  leaves the group, so the broker waits for the session timeout. The new
  process is up in 5 s but consumes nothing until then. With a 30 s deadline,
  every booking that arrives in that gap expires: the expirer log showed bursts
  of 25 every 5 s (5 bookings/s × 5 s poll) starting about 35 s after each
  kill.
- **The batch worker suffered head-of-line blocking.** P(a batch of 10 has a
  lost response) ≈ 1 − 0.9¹⁰ ≈ 65%, and a lost response holds the batch for
  the full 10 s timeout. That works out to about 1.5–1.9 charges/s, which
  matches the measured value.
- **Only successful new charges can lose their response,** so 6.8% of charges
  were lost at 10k, not 10%. The average slot time is about 0.7 s, so 10 slots
  give about 13–15 charges/s: measured 13.2/s on the Mac.
- **At 10k the worker is capacity-bound,** not blocked: about 13/s against
  about 21/s needing a charge. The backlog grows during the load and then
  shrinks at a steady rate, never stalling.
- **Expiries vary from run to run** (128–168 at 1k, 732–1,146 at 10k). They
  depend on when each kill lands relative to Kafka's heartbeats, not on the
  worker.

**Open:**

- Tune `MaxInFlight` against the provider's real concurrency limit (a separate
  experiment from the scheduling fix)
- Shorten the consumer's rejoin after a crash (static group membership or a
  lower session timeout), and measure how much expiry drops
- An expiry-rate metric and alert: a spike means the pipeline stalled
- Run the 1k chaos test in CI (nightly, not on every PR)

## Day 23

**Goal:** Distributed tracing with OpenTelemetry: follow one booking through
every service and every async hop (HTTP, outbox, Kafka, the booking row,
HTTP to the provider) in a single trace, then use the traces to find where
the time goes.

**Settings:**

| Setting     | Value                                     | Why                                                                            |
| ----------- | ----------------------------------------- | ------------------------------------------------------------------------------ |
| backend     | Jaeger v2 (`jaegertracing/jaeger:2.21.0`) | OTLP in (gRPC 4317, HTTP 4318), UI on 16686                                    |
| exporter    | OTLP gRPC, batched                        | spans leave in the background, every 5 s or when the batch fills               |
| sampler     | `ParentBased(AlwaysSample())`             | follow the caller's decision; production: `TraceIDRatioBased` or tail sampling |
| propagation | W3C `traceparent`                         | one format for HTTP headers, Kafka headers and the JSONB columns               |
| on failure  | best effort                               | export errors go to `slog`; requests never wait for or fail on telemetry       |

**Built:**

- **`internal/telemetry`:** `Setup(ctx, service, logger)` (provider, resource
  `service.name`, sampler, propagator, error handler → `slog`; returns the
  flush), `Inject(ctx) map[string]string` (`nil` when there is no span) and
  `Extract(ctx, map) ctx`.
- **API:** `otelhttp` per route (`POST /bookings`, `GET /bookings/{id}`),
  `otelpgx` on the pool (a span per statement, plus `pool.acquire`).
- **Migration 13:** `trace_context JSONB` on `outbox_events` and `bookings`.
  The booking transaction stores the request's span on both rows.
- **Relay:** one `publish <event type>` span per event (kind Producer), parent
  read from `outbox_events.trace_context`; `newRecord` puts the relay's own
  span into the Kafka headers.
- **Consumer:** `Process` starts `process <event type>` (kind Consumer) from
  the Kafka headers; a redelivery is marked `duplicate=true`, a failure is an
  error span.
- **Worker:** `processOne` starts `charge booking` per attempt, parent read
  from `bookings.trace_context` (choice A); `payment.attempt`,
  `payment.outcome`; error only when the system failed (no answer, permanent
  error, stuck, record failed), never for a decline. Outcome events carry the
  charge span.
- **Expirer:** one `expire booking` span per booking in that booking's trace,
  ended **after** the batch transaction, so a rollback is an error and never
  a claimed expiry. Each `booking.cancelled` event carries its own booking's
  span.
- **Payments → paymock:** `otelhttp.NewTransport` on the provider client
  (client span `POST /charges` + `traceparent` header); paymock wraps
  `POST /charges` in `otelhttp.NewHandler` and adds `booking.id`,
  `charge.status`, `charge.replay`, `charge.lost_response`.
- **Setup in every binary:** `api` exits if tracing can't start; `relay`,
  `payments` and `paymock` log it and run without tracing.
- **Fix found from a trace:** `defer client.CloseAllowingRebalance()` in
  `cmd/payments` and `cmd/notifier` (see the rebalance table below).

**Tests added:**

| Package                       | Proves                                                                                                                                               | Result              |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------- |
| telemetry (unit)              | inject → extract keeps the trace; a span from extracted context is a child; no span → `nil`; nil, empty or invalid headers leave ctx unchanged       | ✓ 4                 |
| booking (integration)         | every strategy stores the request's exact `traceparent` on the booking and outbox rows; no span → SQL `NULL`                                         | ✓ 2 (×4 strategies) |
| relay (unit)                  | the Kafka record carries the relay's span, not the stored one; no trace → no trace headers; key and value unchanged                                  | ✓ 3                 |
| relay (integration)           | `publish` continues the stored trace; a failed publish is an error span; an event without a trace still publishes                                    | ✓ 3                 |
| consumer (integration)        | `process` is a child of the publish span and the handler sees it; a redelivery is `duplicate=true`; a failure is an error span                       | ✓ 3                 |
| booking expirer (integration) | each booking in one batch gets a span in its own trace and its event carries it; no stored trace → a root span; a rollback marks the spans as errors | ✓ 3                 |
| payments (unit)               | a charge sends the caller's trace in `traceparent`; no span → no header                                                                              | ✓ 2                 |

All with `-race`. Each new test was checked against a deliberately broken
version of the code it covers, and failed.

**One booking, warm, everything running** (`eb567d73`, 1.24 s end to end):

| Span                                 | Starts   | Takes   | Wait before it | The wait is |
| ------------------------------------ | -------- | ------- | -------------- | ----------- |
| api `POST /bookings`                 | 0 ms     | small   |                |             |
| relay `publish booking.created`      | 181 ms   | 16.6 ms | ~170 ms        | relay poll  |
| payments `process booking.created`   | 202 ms   | 13.8 ms | ~5 ms          | Kafka       |
| payments `charge booking`            | 688 ms   | 43.8 ms | **~470 ms**    | worker poll |
| relay `publish booking.confirmed`    | 1,220 ms | 5.1 ms  | **~490 ms**    | relay poll  |
| payments `process booking.confirmed` | 1,230 ms | 8.5 ms  | ~5 ms          | Kafka       |

About 1.13 s of 1.24 s (**~90%**) is waiting for a poll. A hop through Kafka
costs ~5 ms; a hop through a polled table costs up to one interval (500 ms).

**API `POST /bookings`:**

| Case                             | Spans            | Duration | Note                                       |
| -------------------------------- | ---------------- | -------- | ------------------------------------------ |
| cold (first request after start) | 19               | 81 ms    | a nested prepare span under each statement |
| cold, pool's first connection    | 25 (whole trace) | 404 ms   | `connect` alone 88 ms                      |
| warm                             | 13               | 24–33 ms | statement cache hit: no prepare spans      |

Warm breakdown (`24f2b4b9`, 32.9 ms), one `pool.acquire` per database step:

| Step                          | Statements                                      | Time   |
| ----------------------------- | ----------------------------------------------- | ------ |
| claim the idempotency key     | `INSERT`                                        | 6.6 ms |
| read                          | `SELECT`                                        | 2.6 ms |
| the booking transaction       | `BEGIN`, `UPDATE`, `INSERT`, `INSERT`, `COMMIT` | ~14 ms |
| store the idempotent response | `UPDATE`                                        | 5.6 ms |

The idempotency layer is 2 of the 4 steps and about a third of the time.

**Jaeger down:** the booking still returned 201; the exporter logged its error
about 12 s later; that trace was lost. Telemetry never blocks a request.

**Relay `publish` after idle:** 338 ms after about 2 h idle, 16–22 ms warm.
Likely a reconnect (Kafka closes idle connections after about 10 min);
not verified.

**Consumer restart before / after `CloseAllowingRebalance`:**

| Trace      | Old process stopped (killed by 2nd Ctrl-C) | + 45 s session timeout | `process booking.created` actually ran |
| ---------- | ------------------------------------------ | ---------------------- | -------------------------------------- |
| `79642ff2` | ~01:18:30 (inferred)                       | ~01:19:15              | 01:19:14.8                             |
| `24f2b4b9` | ~01:44:52 (inferred)                       | ~01:45:37              | 01:45:36.7                             |
| `c478e94d` | 01:50:40.3 (logged)                        | **01:51:25.3**         | **≈ 01:51:25.6**                       |

|                               | Before                                                    | After                                        |
| ----------------------------- | --------------------------------------------------------- | -------------------------------------------- |
| Ctrl-C                        | logs `payments stopped`, then hangs until a second Ctrl-C | exits on its own                             |
| Group right after shutdown    | dead member stays until its session times out             | `has no active members`                      |
| Start → first booking charged | waits for old shutdown + 45 s                             | **5.7 s** (≤ 3 s group join + poll + charge) |

**Lost response** (`LOST_RESPONSE_RATE=1`, charge timeout 10 s, lease 30 s,
backoff base 5 s):

|                                 | Attempt 1                                                                       | Attempt 2                                          |
| ------------------------------- | ------------------------------------------------------------------------------- | -------------------------------------------------- |
| starts                          | 2.61 s                                                                          | ~37 s (lease 30 s + jitter ~4.4 s; window 30–35 s) |
| `charge booking`                | 10.0 s, **error**, `payment.outcome=unknown`                                    | 28.4 ms, `payment.attempt=2`, `succeeded`          |
| payments client `POST /charges` | 10.0 s, **error** (request cancelled), no status code                           | 2.4 ms, 200                                        |
| paymock server `POST /charges`  | **10 s, 200, no error**, `charge.lost_response=true`, `charge.status=succeeded` | `charge.replay=true`                               |
| paymock `charge_id`             | `62abed4b…`                                                                     | `62abed4b…` (same: money moved once)               |

The server span is 10 s, not 30 s: the client hung up, `r.Context()` was
cancelled, and the handler's sleep returned. Its 200 is the default status
of a handler that never wrote; the client never received it.

The same run with payments not yet restarted (no `otelhttp` transport):
attempt 1 10.0 s error, attempt 2 at +33.7 s, 27.9 ms; no `POST /charges`
spans under `charge booking`.

**Observations:**

- **End-to-end latency is set by poll intervals,** not by any service: three
  polls (relay, worker, relay) ≈ 1.1 s of a 1.24 s booking.
- **Retries line up in one trace.** The booking row's context doesn't change,
  so every `charge booking` attempt is a sibling under the API span.
- **The trace found a bug no log or test showed:** every restart of a
  consumer paused it for ~45 s, because `Close` hung with
  `BlockRebalanceOnPoll` and the process was killed before it left the group.
  Every rolling deploy would have paused consumption.
- **Client and server disagree on a lost response**, and both are right from
  their side: error vs 200. Only the trace shows both.
- **Every span except a few has `Warnings (1)` in Jaeger.** Not yet read.

**Open:**

- Step 7, choice B: the consumer re-parents `bookings.trace_context` to its
  own span, so `charge booking` sits under `process booking.created`
- Worker trace tests (`processOne` runs in goroutines; wait for the spans)
- `LISTEN/NOTIFY` for the relay and worker; measure booking → confirmed again
- An OpenTelemetry Collector between the services and Jaeger (buffering,
  tail sampling)
- One Setup-failure policy for every binary (the API exits, the rest don't)
- Tracing in the notifier
- paymock: mark the server span when the caller is gone (untested)
- Read the `Warnings (1)` text
- Verify the idle-reconnect explanation for the 338 ms publish

## Day 24

**Goal:** Under a steady load spread over many units, find what dominates the
slowest 1% of `POST /bookings` from the traces and from Postgres, fix one
thing, and compare p99 before and after over several runs.

**Settings:**

| Setting     | Value                                                                                                                                                          | Why                                                                                     |
| ----------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| load        | k6 `constant-arrival-rate`: `POST /bookings` 50/s round-robin over 1,000 units (100 seats each), `GET /bookings/{id}` 25/s over 200 bookings made in `setup()` | open model, so a slow server can't lower the load; no row contention; nothing sells out |
| run         | full reset + seed, 15 s warm-up (discarded), `CHECKPOINT` + `ANALYZE` + `pg_stat_statements_reset()`, 2 min measured                                           | the same starting state every run; no checkpoint inside the measured window             |
| VUs         | pre-allocated 200 POST / 100 GET (max 500 / 200)                                                                                                               | with 50 / 20, k6 dropped requests during stalls instead of measuring them               |
| valid run   | 0 failed, 0 dropped                                                                                                                                            | decided before the runs                                                                 |
| tracing     | `ParentBased(AlwaysSample())` on both sides                                                                                                                    | same overhead before and after                                                          |
| paymock     | `LATENCY_MS=100`                                                                                                                                               | same background pipeline load every run                                                 |
| Postgres    | `shared_preload_libraries=pg_stat_statements`, `track_io_timing=on`                                                                                            | per-statement times                                                                     |
| machine     | MacBook Pro 16" 2019, Intel i9 8-core, 16 GB; Docker Desktop 8 CPUs, 2 GB memory, 1 GB swap                                                                    | numbers are this machine's, not Loka's in general                                       |
| improvement | only if the worst after-run beats the best before-run                                                                                                          | p99 is noisy; a median alone can't show a change                                        |

**Built:**

- **Load tooling:** `scripts/k6/load.js` (steady spread load, POST and GET
  reported separately, compact summary + JSON), `scripts/seed-load.sql`
  (1,000 units, 100 customers, deterministic IDs, `pg_stat_statements`),
  `scripts/loadrun.sh` (one run from an identical start; fails fast on a
  missing seed, compose change or binary; `MONITOR=1` adds `docker stats` and
  an in-VM `SELECT 1` probe for diagnosis), `scripts/traces.sh` (Jaeger v3
  API as text: slow traces per second, pick by duration, one trace's spans
  and the gap).
- **API:** `GET /bookings/{id}` reads through its own pool (`MaxConns` 10,
  `application_name` `loka-api-read`); the write pool is `loka-api`.
  `booking.WithReader` option; an idempotent replay stays on the write store.

**Tests added:**

| Package               | Proves                                                                                                                                    | Result           |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | ---------------- |
| booking (integration) | with every write connection held, `Get` with a reader is served from the read pool; control: without a reader it waits until its deadline | ✓ 1 (2 subtests) |
| booking (unit)        | `Get` uses the store by default and the reader when one is set; a replay never uses the reader                                            | ✓ 3              |

All with `-race`. Checked against broken code: `Get` ignoring the reader
(integration: deadline exceeded after 2 s; unit fails), replay using the
reader (unit fails).

**Baseline (Block 1), 3 valid runs:**

| Run      | POST p50 | POST p95 | POST p99  | POST max | GET p99 | Note                                                |
| -------- | -------- | -------- | --------- | -------- | ------- | --------------------------------------------------- |
| before-1 | 58.6     | 238.0    | 1,071.2   | 1,646.7  | 816.8   | first run of a sequence, binaries built just before |
| before-2 | 29.6     | 48.3     | 79.3      | 199.7    | 11.2    | 50 / 20 VUs, 0 dropped                              |
| before-3 | 29.0     | 47.2     | 280.8     | 961.1    | 145.4   | 50 / 20 VUs, 0 dropped                              |
| median   | 29.6     | 48.3     | **280.8** |          |         | p99 range **79.3–1,071.2**                          |

Excluded (dropped requests, 50 / 20 VUs): p99 838.6 (13 dropped) and 511.4
(9 dropped). All times in ms.

**Where the slowest 1% goes (Block 2, before-1's stack):**

| Question                  | Answer                   | Evidence                                                                                                              |
| ------------------------- | ------------------------ | --------------------------------------------------------------------------------------------------------------------- |
| When are POSTs slow?      | in bursts                | 78 POSTs ≥ 1 s, all in 4 episodes (09:57:39–40, 09:58:00–01, 09:58:34–35, 09:58:41)                                   |
| Which statement?          | whichever was in flight  | slow traces: store-response `UPDATE` 1,022 ms, booking `INSERT` 1,012 ms, **`BEGIN` 1,011 ms**, `pool.acquire` 902 ms |
| Inside Postgres?          | no                       | largest `max_ms` of any statement 221; `UPDATE idempotency_keys` max 94.7                                             |
| N+1 or missing index?     | no                       | the slow step differs per trace; every statement is fast in Postgres                                                  |
| Checkpoints?              | no                       | 09:57:08 (forced, before the run) and 10:02:08 (after it)                                                             |
| Typical request (58.5 ms) | 8 round trips × 4–9 ms   | `BEGIN` 6.4 ms in the span vs 0.004 ms in Postgres                                                                    |
| A slow GET                | waiting for a connection | `pool.acquire` 881.6 ms, then `SELECT` 22 ms                                                                          |

`pg_stat_statements` doesn't count commit work: `commit` shows 20,048 calls
at 0.006 ms mean.

**Diagnostic run** (`MONITOR=1`, not a measurement; p50 84.5 ms from the
monitors' own overhead):

| API stall episode | Traces ≥ 1 s | In-VM `SELECT 1` probe |
| ----------------- | ------------ | ---------------------- |
| 10:35:20          | 6            | nothing > 100 ms       |
| 10:35:38–39       | 23           | 872 ms                 |
| 10:35:48–50       | 21           | 1,015 ms               |
| 10:36:34–35       | 31           | 119 ms                 |
| 10:37:03–04       | 41           | nothing > 100 ms       |

`docker stats` (every ~4 s): Kafka 75–421% CPU, Postgres 45–229%, Jaeger
< 16%. Peak memory Kafka 507 MiB, Postgres 202 MiB, Jaeger 199 MiB of the
VM's 1.94 GiB. Kafka's peaks don't line up with the stalls.

**Fix (Block 3): separate read pool, alternated runs:**

| Order | Run      | POST p50 | POST p95 | POST p99 | GET p95 | GET p99 |
| ----- | -------- | -------- | -------- | -------- | ------- | ------- |
| 1     | after-1  | 42.4     | 157.6    | 1,086.9  | 19.4    | 796.0   |
| 2     | before-7 | 43.8     | 196.6    | 1,127.7  | 36.0    | 842.6   |
| 3     | after-2  | 30.9     | 49.8     | 80.6     | 6.5     | 10.4    |
| 4     | before-8 | 31.4     | 52.6     | 94.5     | 7.1     | 14.5    |
| 5     | after-3  | 62.9     | 930.4    | 1,622.3  | 225.8   | 925.5   |
| 6     | before-9 | 45.9     | 131.5    | 1,000.8  | 19.0    | 738.5   |

Each sequence started with a discarded burn-in run. Not alternated (same
session, earlier): before-4..6 GET p99 20.2 / 685.2 / 772.3, POST p99
125.7 / 936.2 / 953.9.

| Metric   | Before 7–9: median (range) | After 1–3: median (range) | Verdict                           |
| -------- | -------------------------- | ------------------------- | --------------------------------- |
| GET p99  | 738.5 (14.5–842.6)         | 796.0 (10.4–925.5)        | overlap: **no improvement shown** |
| POST p99 | 1,000.8 (94.5–1,127.7)     | 1,086.9 (80.6–1,622.3)    | overlap: no change shown          |
| POST p50 | 43.8 (31.4–45.9)           | 42.4 (30.9–62.9)          | overlap                           |

**k6 VUs vs a 2 s Postgres freeze** (my Linux container, `docker pause`):

| Pre-allocated VUs | POST p95 | POST p99 | Dropped |
| ----------------- | -------- | -------- | ------- |
| 50 / 20           | 384 ms   | 2,089 ms | 47      |
| 200 / 100         | 963 ms   | 2,319 ms | 0       |

**Observations:**

- **The slowest 1% is not a query problem.** 1–1.7 s stalls, several per
  2 minutes, freeze whatever each request is doing, even a `BEGIN`. No
  statement is slow inside Postgres, and no checkpoint runs. Not N+1, not a
  missing index.
- **p99 measures whether a stall happened.** Across the baseline runs p50
  ranged 29–59 ms, p95 47–238 ms and p99 79–1,071 ms on identical code. At
  75 requests/s a 1 s stall alone fills the ~60 requests that decide p99.
- **Neighbouring runs resemble each other, whichever binary runs.** The
  stalls come and go over ~10-minute periods. Without alternating, the fix
  would have looked better or worse depending on which period it landed in.
- **The read pool didn't change GET p99.** GETs on their own pool still
  waited ~0.8–0.9 s in stall runs, so the freeze reaches reads directly.
  Block 2's "GET p99 is pool wait" came from one trace and was too broad.
  Not verified with a slow GET trace from an after run.
- **The in-VM probe froze in 2 of 5 episodes.** Some stalls are inside the
  VM; the others are outside what a local `SELECT 1` sees (the Mac ↔ VM
  path, the Mac side, or the write path).
- **Too few pre-allocated VUs hide the tail.** k6 dropped the stall's
  requests instead of timing them: p95 384 vs 963 ms for the same freeze.
- **First runs after heavy CPU work were among the slowest** (p50 38–69 ms,
  after building binaries or `make test-integration`), but after-3, in the
  middle of a sequence, reached 62.9 ms too. A weak pattern, cause unknown.
- **A typical POST is 8 round trips** at ~4–9 ms each through Docker
  Desktop; my Linux container: 0.7 ms per `BEGIN`, POST p99 17.9 ms in one
  check run (not comparable).

**Open:**

- A slow GET trace from an after run: waiting in `pool.acquire` or `SELECT`?
- Measure the read pool under the hot-unit contention load, where row locks
  really do fill the write pool
- Locate the stalls outside the VM: a host-side probe, `track_wal_io_timing`
- Why Kafka uses ~3 of 8 VM CPUs at ~75 messages/s
- Store the idempotent response inside the booking transaction: 8 → 7 round
  trips, 3 → 2 commits, and no booking committed with its key still
  `in_progress` after a crash
- First-run drift: a first run with no heavy work before it; `pmset -g therm`
- VM swap counters (`vmmon.txt`) not checked
- The same load on Linux / native Docker, to separate the machine from Loka

## Day 25

**Goal:** Cache inventory units in Redis for the availability page
(`GET /units/{id}`), invalidate on every write that changes a unit, and
measure the hit rate and the read p99 delta.

**Settings:**

| Setting      | Value                                                                                                                                                                | Why                                                        |
| ------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| load         | Day 24's (POST 50/s round-robin over 1,000 units, `GET /bookings/{id}` 25/s) + `GET /units/{id}` at 200/s (Block 3) or 100/s (follow-up), a uniform random unit each | reads arrive independently per unit, as from many browsers |
| cache        | cache-aside, `UNIT_CACHE_TTL` 30 s; 0 = off with the same binary                                                                                                     | before and after differ only by one env var                |
| invalidation | outbox → relay (500 ms poll) → Kafka → `cmd/cacheinvalidator`: `DEL unit:<id>` on `booking.created` and `booking.cancelled`                                          | after commit, never before; DEL, not overwrite             |
| Redis        | `redis:7.4-alpine`, no persistence, `maxmemory 64mb allkeys-lru`, container limit 128M                                                                               | a cache, never a source of truth                           |
| API client   | read/write timeout 50 ms (250 ms in one arm), dial 200 ms, `MaxRetries -1`, `DialerRetries 1`                                                                        | fail fast and fall back to Postgres                        |
| run          | Day 24's, plus `CONFIG RESETSTAT` after the warm-up; hit rate from `INFO stats`; fallbacks counted in `api.log`                                                      | only the measured window counts                            |
| valid run    | 0 failed, 0 dropped                                                                                                                                                  | decided before the runs                                    |
| improvement  | only if the worst run of one arm beats the best run of the other                                                                                                     | p99 is noisy                                               |
| machine      | Day 24's (MacBook Pro 2019 i9, Docker Desktop 8 CPUs / 2 GB)                                                                                                         | numbers are this machine's                                 |

**Built:**

- **`GET /units/{id}`**: the service reads the cache first, then the read
  pool. A miss stores the unit. A cache failure is logged and served from
  Postgres without storing. `POST /bookings` never touches the cache.
- **`internal/unitcache`**: the Redis adapter (`Get` / `Set` with TTL /
  `Delete`, JSON values, key `unit:<id>`) and the invalidator, a
  `consumer.Handler`. A v1 `booking.cancelled` without a unit is logged and
  skipped; a v2 or `booking.created` without a unit, or bad JSON, is poison;
  a Redis error is retried without committing the offset.
- **`booking.cancelled` v2**: adds `unit_id` and `qty`, written by the
  expirer and by the payment worker on a decline. v1 events still decode.
- **`cmd/cacheinvalidator`**: its own consumer group; a new group starts at
  the newest offset.
- **Load tooling**: a Redis service in compose; `loadrun.sh` starts the sixth
  binary, records `settings.txt`, and prints the hit rate and fallback
  count; `load.js` adds the `GET /units/{id}` scenario (`UNIT_RATE`).
- **`.gitignore`**: `api` → `/api`. The old pattern ignored every path named
  `api`, including new files in `internal/api/`.

**Tests added:**

| Package                 | Proves                                                                                                                                                              | Result           |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------- |
| booking (unit)          | `GetUnit`: hit; miss stores; a failed store still answers; cache down → Postgres, no store; unknown unit; Postgres failure; cache off; booking never uses the cache | ✓ 3 (8 cases)    |
| booking (unit)          | a v1 `booking.cancelled` decodes, `unit_id` and `qty` zero                                                                                                          | ✓ 1              |
| booking (integration)   | the expirer's event carries `unit_id` and `qty`, read from the JSON keys                                                                                            | ✓ 1              |
| payments (integration)  | the decline event carries `unit_id` and `qty`                                                                                                                       | ✓ 1              |
| unitcache (integration) | real Redis: miss then round trip; TTL set; delete → miss, twice OK; Redis down is not a miss; a refused SET (OOM) is reported                                       | ✓ 5              |
| unitcache (unit)        | invalidator: created / cancelled v2 delete; v1 skipped and logged; no unit or bad JSON → poison; confirmed / unknown ignored; Redis down → retryable                | ✓ 1 (9 subtests) |
| api (unit)              | 200 with exactly 8 fields and `description: null`; 400 before the service; 404; 500 logged, cause not in the body                                                   | ✓ 1 (5 subtests) |

All with `-race`. Checked against broken code, each caught by at least one
test: Ping-then-Set, every handler/invalidator break listed in the PR
(missing fields, `omitempty`, leaked error, error swallowed, Redis error as
poison, bad JSON retried forever, version not bumped, wrong JSON tag,
hard-coded qty).

**Smoke checks** (my Linux container, real stack, not comparable to the Mac):
miss → `SET` with TTL 30 → hit; after a 201 the key was gone in ~400 ms and
the next read showed the new availability; Redis stopped → 200 from Postgres.
With go-redis's default dial retries (5 × 100 ms) every read with Redis down
took ~408 ms; with `DialerRetries: 1`, ~3 ms.

**Block 3: `UNIT_RATE=200`, 1 burn-in + 3 + 3 alternated** (ms):

| Order | Run      | Valid | units p50 | p95      | p99      | POST p50 | POST p99 | Dropped | Hit rate | Fallbacks | unit `SELECT` calls |
| ----- | -------- | ----- | --------- | -------- | -------- | -------- | -------- | ------- | -------- | --------- | ------------------- |
| 0     | burn25-1 | –     | 8.91      | 115.71   | 1,036.56 | 99.13    | 1,250.81 | 27      | 80.7%    | 1,091     | –                   |
| 1     | off-1    | ✓     | 10.31     | 52.42    | 565.74   | 113.93   | 973.70   | 0       | –        | –         | 30,202              |
| 2     | on-1     | ✗     | 9.35      | 868.29   | 1,667.10 | 99.34    | 2,005.76 | 112     | 79.3%    | 2,026     | 12,757              |
| 3     | off-2    | ✗     | 7.77      | 1,315.65 | 2,261.53 | 76.98    | 3,293.89 | 220     | –        | –         | 29,982              |
| 4     | on-2     | ✓     | 14.94     | 277.30   | 830.34   | 172.43   | 1,199.17 | 0       | 81.0%    | 1,720     | 12,158              |
| 5     | off-3    | ✗     | 17.25     | 81.62    | 735.24   | 219.54   | 1,989.83 | 32      | –        | –         | 30,169              |
| 6     | on-3     | ✗     | 11.43     | 297.05   | 1,052.99 | 126.77   | 1,483.65 | 44      | 80.3%    | 1,738     | 12,308              |

2 of 6 runs valid. Latency: every range overlaps, **no change shown**.
POST p50 77–220 ms vs ~30 ms on Day 24: this load was beyond the machine.

**Follow-up: `UNIT_RATE=100`, 1 burn-in + 3 × (off, on 50 ms, on 250 ms)** (ms):

| Run        | units p50 | p95   | p99    | POST p99 | Hit rate | Fallbacks | unit `SELECT` calls |
| ---------- | --------- | ----- | ------ | -------- | -------- | --------- | ------------------- |
| lo-burn-1  | 8.15      | 44.32 | 843.49 | 1,291.60 | 66.0%    | 483       | –                   |
| lo-off-1   | 4.04      | 7.55  | 494.08 | 858.95   | –        | –         | 18,202              |
| lo-on50-1  | 4.32      | 12.54 | 63.93  | 258.07   | 58.5%    | 146       | 11,266              |
| lo-on250-1 | 4.53      | 14.13 | 618.11 | 769.44   | 58.8%    | 204       | 11,265              |
| lo-off-2   | 3.69      | 6.69  | 478.07 | 836.64   | –        | –         | 18,201              |
| lo-on50-2  | 4.55      | 14.86 | 766.78 | 1,005.96 | 58.4%    | 402       | 11,424              |
| lo-on250-2 | 4.57      | 20.49 | 843.75 | 1,052.42 | 59.2%    | 394       | 11,329              |
| lo-off-3   | 3.65      | 6.21  | 34.82  | 129.87   | –        | –         | 18,202              |
| lo-on50-3  | 4.37      | 12.71 | 566.87 | 727.29   | 58.4%    | 205       | 11,311              |
| lo-on250-3 | 5.14      | 16.96 | 185.25 | 539.66   | 61.4%    | 86        | 10,881              |

All 9 measured runs valid. POST p50 30–34 ms (back to Day 24's level).

| Comparison (`GET /units`) | p50                                                 | p95                                                   | p99     |
| ------------------------- | --------------------------------------------------- | ----------------------------------------------------- | ------- |
| off vs on 50 ms           | **off faster**, no overlap (3.65–4.04 vs 4.32–4.55) | **off ~2× faster**, no overlap (6.2–7.6 vs 12.5–14.9) | overlap |
| on 50 ms vs on 250 ms     | overlap                                             | overlap                                               | overlap |
| fallbacks 50 vs 250 ms    | 146–402 vs 86–394: overlap                          |                                                       |         |

**Postgres reads of the unit row** (one statement; the POST path adds 6,200
per run in both arms, 6,000 load + 200 from setup):

| Load  | Off: total (read path) | On: total (read path)       | Total    | Read path |
| ----- | ---------------------- | --------------------------- | -------- | --------- |
| 200/s | ~30,100 (~23,900)      | 12,158–12,757 (5,958–6,557) | **−59%** | **−74%**  |
| 100/s | 18,202 (12,002)        | 10,881–11,424 (4,681–5,224) | **−38%** | **−58%**  |

With the cache off, the read path's calls equal the `GET /units` count
(e.g. 12,002 vs 12,001); with it on, misses + fallbacks (4,928 + 146 =
5,074 vs 5,066 for lo-on50-1).

**Observations:**

- **The cache cut Postgres reads but did not make reads faster.** A
  primary-key `SELECT` costs ~0.2 ms in Postgres; the rest is the round trip
  through Docker Desktop, the same trip a Redis `GET` makes.
- **At 59% hits the cache made reads slower:** p50 +~0.5 ms and p95 ~2×,
  with no overlap. A miss is 3 round trips (Redis `GET`, Postgres, Redis
  `SET`), so with ~41% misses p95 lands on a miss, and p50 lands in the
  slower part of the hits.
- **Hit rate follows reads per unit per invalidation, not the TTL.** Every
  unit is booked every 20 s, so the 30 s TTL never fires. 200/s → 79–81%,
  100/s → 58–61%. Share of all reads served from the cache, counting
  fallbacks as misses: 73–76% and 57–61%.
- **A longer timeout didn't reduce fallbacks.** 50 ms and 250 ms gave the
  same counts: the slow Redis calls are the machine's ~1 s freezes, longer
  than any sensible timeout. At 200/s the 50 ms timeout fired on 7–8% of
  reads; at 100/s on 1–3%.
- **Hidden retries in the client:** go-redis's `DialerRetries` (default 5,
  100 ms apart; ≤ 0 means default) is separate from `MaxRetries`. Only a
  test with Redis actually down showed it.
- **p99 is still decided by the stalls**, in both arms at both loads.

**Open:**

- An in-process (L1) cache: no network hop, the only way here to make a hot
  primary-key read faster
- Fill the cache after responding (a miss becomes 2 round trips, not 3)
- Cache an expensive query (availability across dates) instead of a
  primary-key lookup, and measure there
- The exact hit-rate model: measured 79–81% / 58–61% sits between a Poisson
  model (80% / 66.7%) and a fixed-interval invalidation model (75.5% / 56.8%)
- Redis spans in traces (`redisotel`): today a hit is invisible in Jaeger
- A Redis-only consumer loop without the `processed_events` claim (~50
  Postgres writes/s that `DEL` doesn't need)
- A circuit breaker for Redis, and rate-limited fallback warnings (200/s of
  WARN lines during an outage)
- Versioned cache keys (`unit:v1:`), so a struct change can't decode old
  entries
- Per-unit TTL, negative caching, CDC as an invalidation source
- The same comparison on Linux / native Docker

## Day 26

**Goal:** Batch the outbox relay, tune the API's connection pool, try
GOMAXPROCS, and record each change's effect separately: change → p99
before → p99 after → throughput delta.

**Settings:**

| Setting       | Value                                                                                                               | Why                                                                    |
| ------------- | ------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| drain test    | 20,000 unpublished rows, only Postgres + Kafka + the relay running; Postgres times the drain (PL/pgSQL loop, 50 ms) | the relay's own maximum rate, without HTTP, k6 or consumers in the way |
| load runs     | Day 25's load at `UNIT_RATE=100` (175 req/s), 15 s warm-up, 2 min measured                                          | the load this Mac sustains                                             |
| outbox lag    | `published_at − created_at` for events created in the measured window; `published_at = clock_timestamp()`           | `now()` is the transaction's start and hid the sequential sends        |
| pool counters | `GET /debug/pool` before and after the measured window (`pgxpool.Stat`)                                             | the difference is the window alone                                     |
| arms          | `API_DB_MAX_CONNS` 50/10/5/2, `GOMAXPROCS` default (16)/2, relay old/new; same binaries otherwise                   | one change at a time                                                   |
| old relay     | `main`'s relay code + the `clock_timestamp()` fix (`bin/relay-old-clock`)                                           | differs from the new one only in batching                              |
| valid run     | 0 failed, 0 dropped; pool 2 exempt (drops are the result there)                                                     | decided before the runs                                                |
| improvement   | only if the worst run of one arm beats the best run of the other                                                    | p99 is noisy                                                           |

**Built:**

- **Relay batching**: one `ProduceSync` per batch instead of one per event.
  `Publisher.Publish(ctx, []Message) []error` returns one result per message;
  `KafkaPublisher` maps franz-go's results back by `*Record` (they come back
  in completion order). Each message keeps its own publish span and
  `traceparent`.
- **Partial failure**: every acknowledged event is marked, each failed one
  gets its own error and attempt; no more stop-at-first-failure. A poison
  event no longer blocks the outbox. **Behaviour change:** per-booking order
  is no longer kept when a batch partly fails (today's consumers don't
  depend on it; a batched send couldn't keep it anyway).
- **`published_at = clock_timestamp()`** instead of `now()`.
- **API**: `API_DB_MAX_CONNS` (write pool, default 50); startup log of
  GOMAXPROCS and pool sizes; `GET /debug/pool` (both pools' `pgxpool.Stat`
  counters).
- **Tooling**: `scripts/draintest.sh`; `loadrun.sh` passes
  `API_DB_MAX_CONNS` / `GOMAXPROCS_API`, prints the write pool's waits, the
  outbox lag and the CPU speed limit.

**Tests added:**

| Package             | Proves                                                                                                                                                                                                                                                     | Result           |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------- |
| relay (unit)        | `KafkaPublisher` against an in-process Kafka (`kfake`): every record arrives with its own `traceparent`; each result lands on its own message although results return in completion order                                                                  | ✓ 2              |
| relay (integration) | one call per batch; one failure doesn't hold back the others; only the failed event is retried; all fail → all recorded; wrong result count → error, nothing marked; an undecodable row is an error, not an empty batch; each message carries its own span | ✓ 7 (2 replaced) |

All 18 relay tests with `-race`. Checked against broken code: one call per
event, stop at first failure, results in completion order, the batch's
context for every record, no result-count check, shared span, failure not
recorded, failed events marked published, `len(events) == 0` checked before
`err`. Not tested: `API_DB_MAX_CONNS` and `/debug/pool` (checked by hand).

**Drain test: 20,000 events, alternated** (events/s):

| Run    | 1     | 2     | 3     | Median    | Drain time  |
| ------ | ----- | ----- | ----- | --------- | ----------- |
| before | 346   | 449   | 358   | **358**   | 44.6–57.8 s |
| after  | 4,839 | 4,660 | 4,278 | **4,660** | 4.1–4.7 s   |

Burn-in (after, discarded): 1,892. No overlap: **13×**. All runs: 20,000
published, 20,000 in Kafka, 0 failed attempts. Implied round trip in the
quiet drain: ~2.7 ms (100 events ÷ 358/s ÷ 104 round trips). After: ~21 ms
per batch, more than ~5.5 round trips explain (startup and per-batch CPU,
not measured).

**Load runs, main sequence** (ms; order: round 1 top to bottom, round 2
reversed):

| Run         | POST p50 | p95     | p99       | units p50 | Fail/drop    | Write pool: no idle conn, total wait | Lag p50 | p95     | p99       |
| ----------- | -------- | ------- | --------- | --------- | ------------ | ------------------------------------ | ------- | ------- | --------- |
| relay-old-1 | 30.2     | 65      | 798       | 4.6       | 0/0          | 1.4%, 4.5 s                          | **766** | 1,380   | **2,630** |
| base-1      | 28.2     | 71      | 916       | 4.5       | 0/0          | 1.7%, 2.4 s                          | **303** | 538     | **579**   |
| pool10-1    | 27.0     | 51      | 709       | 4.3       | 0/0          | 2.0%, 63.6 s                         | 296     | 531     | 565       |
| pool5-1     | 27.4     | 48      | 425       | 4.2       | 0/0          | 3.8%, 52.6 s                         | 295     | 531     | 558       |
| pool2-1     | 27.3     | **628** | **1,304** | 4.3       | 0/0          | **18%, 394.6 s**                     | 294     | 532     | 1,097     |
| procs2-1    | 34.5     | 115     | 388       | 6.4       | 0/0          | 0.1%, 10.6 s                         | 317     | 576     | 684       |
| procs2-2    | 85.0     | 397     | 1,476     | 9.9       | 0/0          | 3.8%, 64.0 s                         | 402     | 687     | 1,273     |
| pool2-2     | 37,127   | 60,005  | 60,010    | 40.9      | 1,163/4,317  | 99.1%, 57,431 s                      | 474     | 1,047   | 1,513     |
| pool5-2     | 30,239   | 60,005  | 60,009    | 213       | 1,140/4,624  | 97%, 58,104 s                        | 645     | 1,637   | 2,551     |
| pool10-2    | 233      | 749     | 1,221     | 17.1      | 0/0          | 53.7%, 786 s                         | 496     | 837     | 966       |
| base-2      | 311      | 1,505   | 1,879     | 33.9      | 0/0          | 14.2%, 307 s                         | 700     | 1,568   | 2,060     |
| relay-old-2 | 10,695   | 54,541  | 60,136    | 6,442     | 1,242/11,783 | 92.5%, 51,843 s                      | 155,387 | 218,196 | 219,514   |

Round 1 healthy (POST p50 27–35 ms); round 2 degraded from ~35 minutes in,
whatever the arm (base-2: 11× base-1 with identical settings).

**Confirmation runs, after a Docker Desktop restart** (ms):

| Run           | POST p50 | p95    | p99    | units p50 | Fail/drop   | Write pool      | Lag p50 |
| ------------- | -------- | ------ | ------ | --------- | ----------- | --------------- | ------- |
| conf-base-1   | 54.6     | 102    | 936    | 6.8       | 0/0         | 1.6%, 10.1 s    | 340     |
| conf-procs2-1 | 58.5     | 187    | 1,066  | 7.2       | 0/0         | 2.5%, 12.6 s    | 361     |
| conf-pool2-1  | 21,643   | 37,315 | 38,311 | 13.3      | 2,010/3,072 | 99.5%, 59,255 s | 387     |
| conf-pool2-2  | 22,130   | 38,423 | 38,970 | 13.5      | 2,176/3,109 | 99.5%, 60,720 s | 402     |
| conf-procs2-2 | 66.4     | 178    | 1,169  | 8.0       | 0/0         | 2.3%, 17.8 s    | 361     |
| conf-base-2   | 78.6     | 777    | 1,490  | 9.7       | 0/0         | 5.8%, 96.4 s    | 411     |

`pmset -g therm`: **CPU_Speed_Limit 22 before, 20 after** (the CPU allowed
~20% of its normal speed). Both conf-base runs above the 40 ms degraded
line set before the runs: the whole set ran throttled.

**The plan's table:**

| Change                 | p99 before          | p99 after                                | Throughput delta                               |
| ---------------------- | ------------------- | ---------------------------------------- | ---------------------------------------------- |
| Relay batching         | outbox lag 2,630 ms | **579 ms** (worst new run 2,060)         | relay **358 → 4,660 events/s (13×)**           |
| Write pool 50 → 10 / 5 | POST 916 ms         | 709 / 425 ms (n = 1)                     | none (fixed-rate load)                         |
| Write pool 50 → 2      | POST 916 ms         | 1,304 ms healthy; **collapse throttled** | throttled: 1,431–2,929 POSTs answered of 6,000 |
| GOMAXPROCS default → 2 | POST 916–1,879 ms   | 388–1,476 ms                             | none shown                                     |

**Observations:**

- **Batching turned a per-event cost into a per-batch cost**: 13×. Less
  than the ~19× the round-trip count suggests; the rest is startup and
  per-batch CPU.
- **Lag fell 2.5× (p50) and 4.5× (p99), but not to tens of ms**: the 500 ms
  poll is now most of it (p50 ≈ half a cycle). The old relay was worse than
  modelled because a slower send makes a longer cycle, a bigger batch and a
  slower send.
- **Pool size 50 → 5 changed nothing measurable; 2 has no headroom.**
  Healthy: same p50, tail ×9, 18% of acquires waited (3.3 requests queued
  on average: total wait ÷ 120 s). Throttled: collapse 3 times out of 3,
  while pool 50 survived every time. Busy connections = rate × hold time;
  the hold time grows with a slower CPU, so the cliff moves.
- **Pool 50 still found no idle connection 1.7% of the time** with no new
  connections: the stall moments, when all 50 are busy at once.
- **The read pool kept reads alive while the write pool collapsed**
  (conf-pool2: 0 unit-read failures, p50 13 ms). Day 24's bulkhead helps
  when the pool is the bottleneck, not when the database freezes.
- **GOMAXPROCS 2 vs 16: no effect shown** (n = 3 each; round 1's +6 ms did
  not repeat). The API needs well under one core at this load.
- **The Mac throttles its CPU to ~20%** after sustained load. That explains
  round 2 and probably Day 24's drift. Runs now record it.
- **A POST takes the write pool 4 times** (counted with `/debug/pool`).
- Unexplained: the old relay's correctness check drained 2,000 events at
  111 events/s (vs 346–449 at 20,000).

**Open:**

- LISTEN/NOTIFY for the relay (and worker): the poll is now most of the lag
- A CPU profile of the relay during a drain: what fills the ~21 ms per batch
- Batch size 1,000 (an env var): the cost is per batch now
- A poison **row** (undecodable) still fails every fetch and blocks the
  outbox; an attempt cap / dead-letter for poison events
- A per-booking sequence number for any consumer that needs order
- Pools × 3 API replicas (Day 38): ~203 connections against
  `max_connections` 100 → smaller pools or PgBouncer
- GOMAXPROCS under a container CPU limit (Days 36–37)
- Measurements on a cooled / plugged-in Mac, or Linux; Docker Desktop's
  memory as a second suspect
- Throughput at saturation (Days 34–35); `/debug/pool` → Prometheus (Day 33)

## Day 27

**Goal:** Prometheus + Grafana dashboard for the four golden signals
(traffic, latency, errors, saturation) of the API and the outbox pipeline,
and a check of the dashboard's p99 against k6's.

**Machine change (from today on):** Docker Desktop VM memory **1.94 → 3.84
GiB** (Docker 20.10.12, Compose v2.2.3, Intel Mac). Runs from Day 27 on are
not compared with Days 24–26 unless rerun on this setup. Docker Desktop
stopped twice after the resize (engine down, compose containers gone).

**Settings:**

| Setting      | Value                                                                                                             | Why                                                                  |
| ------------ | ----------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| collection   | pull: Prometheus 3.5.0 scrapes `:8080/metrics` (API) and `:9101/metrics` (relay) every 5 s                        | a dead process shows as `up == 0`; 5 s gives `rate()` enough samples |
| monitoring   | own compose project (`make monitoring-up`); Grafana 12.1.0, dashboard provisioned from JSON                       | `make reset` (`down -v`) would wipe the history on every load run    |
| HTTP latency | otelhttp's `http.server.request.duration`, buckets 5, 10, 25, 50, 75, 100, 250, 500, 750 ms, 1, 2.5, 5, 7.5, 10 s | the library default, read from `/metrics`, not assumed               |
| labels       | route pattern (`/bookings/{id}`), method, status code                                                             | bounded: a raw path is one series per booking                        |
| comparison   | `histogram_quantile` over `increase(...[window])`, window = measured run + 5 s                                    | same requests as k6's table, plus k6's 200 setup POSTs               |
| valid run    | 0 failed, 0 dropped (Day 26's rule)                                                                               | decided before                                                       |

**Built:**

- `/metrics` on the API: otelhttp's three HTTP histograms, otelpgx's
  `db_client_operation_duration_seconds` (free once a meter provider
  existed), `go_*` and `process_*`.
- Pool gauges per pool (`otelpgx.RecordStats`), named `write` / `read`:
  both pools point at the same `host:port/db`, otelpgx's default name, and
  their series would collide.
- Relay on `:9101`: events published (counted after commit), events failed,
  batch duration (non-empty batches only; buckets 1 ms to 30 s), outbox
  backlog and oldest-event age (read from Postgres per scrape).
- `loadrun.sh` prints Prometheus's p50/p95/p99 for the measured window under
  k6's table.

**Free metrics, predicted vs read from `/metrics`:**

| Question                      | Predicted                      | Actual                                                    |
| ----------------------------- | ------------------------------ | --------------------------------------------------------- |
| what appears without code     | `http.server.request.duration` | 3 otelhttp histograms + otelpgx DB durations + go/process |
| `/metrics` counted as a route | yes                            | **no**: otelhttp wraps each route, not the mux            |

**Relay, single events after a restart** (`sum ÷ count` of the batch
histogram): first batch **687 ms**; the next 5 averaged **42.8 ms**. Cold
start confirmed; 42.8 ms for one event is above Day 26's ~21 ms per
100-event batch, cause not measured (JVM warm-up, throttling, fresh VM).

**metrics-1** (`dbaaa7b+dirty`, the relay commit before rebasing onto #41/#42:
otelhttp 0.71, franz-go 1.22.0; 2 min, `UNIT_RATE=200`, CPU_Speed_Limit
**20% at start and end**): **invalid as a measurement** (406 dropped
iterations, k6 at its 500-VU limit), valid for the k6-vs-Prometheus
comparison (both see the same requests).

| ms                 | k6 (client, exact) | Prometheus (server, buckets) | Δ          | bucket of the value |
| ------------------ | ------------------ | ---------------------------- | ---------- | ------------------- |
| POST p50           | 235.32             | 239.39                       | +1.7%      | 100–250 ms          |
| POST p95           | 2,206.45           | 2,367.95                     | +7.3%      | 1–2.5 s             |
| POST p99           | 3,610.83           | 4,362.66                     | **+20.8%** | 2.5–5 s             |
| GET bookings p50   | 24.54              | 23.99                        | −2.2%      | 10–25 ms            |
| GET bookings p95   | 2,270.72           | 2,411.80                     | +6.2%      | 1–2.5 s             |
| GET bookings p99   | 3,541.33           | 4,418.41                     | **+24.8%** | 2.5–5 s             |
| GET units p50      | 23.54              | 23.39                        | −0.6%      | 10–25 ms            |
| GET units p95      | 2,131.56           | 2,251.14                     | +5.6%      | 1–2.5 s             |
| GET units p99      | 3,450.64           | 4,236.55                     | **+22.8%** | 2.5–5 s             |
| POST count         | 5,990              | 6,337                        | +5.8%      | incl. 200 setup     |
| GET bookings count | 3,001              | 3,095                        | +3.1%      |                     |
| GET units count    | 23,606             | 24,349                       | +3.1%      | no setup requests   |

Same run: unit cache hit rate 76.3% (14,072 / 4,380), **5,762 reads fell
back to Postgres**; write pool 50: 24,760 acquires, 16.6% found no idle
connection, 0 new, **988,995 ms waiting** (≈ 8.2 requests waiting on
average); outbox lag p50 640.4 / p95 1,534.2 / p99 2,114.5 / max 3,478.6 ms
over 7,716 events (6 unpublished).

**Observations:**

- **The dashboard's error follows the bucket width, not the traffic:**
  under 2% at p50, 6–7% at p95 (a 1.5 s bucket), **21–25% at p99** (a 2.5 s
  bucket). Linear interpolation assumes requests spread evenly over the
  bucket; they cluster at its low end, so the estimate is high.
- **Server-side vs client-side shows only at p50, and only just:** both GET
  p50s are 0.15–0.55 ms lower on the dashboard (network + HTTP parsing on
  localhost). In the tail, buckets dominate.
- **Counts are estimates too:** `increase()` extrapolates to the window's
  edges, +3% on a route with no setup requests.
- **k6 stays the source of the numbers in this file; the dashboard shows
  shape and timing** (when it degrades, which saturation signal moves first).
- **At 20% CPU everything degraded together:** POST p50 235 ms (Day 26
  healthy: 27–28), Redis timeouts (5,762 fallbacks at 50 ms), pool waits,
  lag ×2, and k6 itself ran out of VUs.
- **An absent counter is not a zero:** `failed_total` has no series until
  its first increment, so a panel shows No data. `or vector(0)` and
  `or ... * 0` in the queries; verified with a forced 5xx in a scratch run.

**Open:**

- Bucket edges where the tail is (an OTel View on
  `http.server.request.duration`) if the dashboard's p99 must be exact
- Consumer metrics and Kafka consumer lag (cut today for time)
- Relay batch errors (e.g. Postgres down) are logged, not counted; only the
  backlog age shows them
- Alert rules (backlog age rising, `up == 0`, 5xx share)
- The breaking-point run (Days 28–29): a cool Mac (`CPU_Speed_Limit 100`),
  and k6's 500-VU limit raised or watched
- Docker Desktop upgrade before k8s (Days 30–31), recorded as its own change

## Day 29

**Goal:** 3 API replicas behind a load balancer; rerun the contention test to
show the invariant holds across instances (plan Day 38).

**Settings:**

| Setting   | Value                                                                                                 | Why                                                       |
| --------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| replicas  | 3 × `cmd/api` in containers (`Dockerfile`, `docker-compose.replicas.yml`), nginx round-robin on :8088 | only the API is replicated; correctness lives in Postgres |
| pools     | `API_DB_MAX_CONNS=20` per replica: 3 × (20 + 10 read) = 90 of `max_connections` 100                   | 3 × the default 60 = 180 would exceed Postgres's limit    |
| tests     | `contention.js` (500 VUs, 1 POST each, 10-seat unit) and `retry.js` (500 VUs, one key)                | Days 3–11's tests, unchanged                              |
| control   | `UNSAFE_DECREMENT=1`: the decrement computed in Go                                                    | proves the setup can detect an oversell                   |
| valid run | no k6 `bookings_error` (status 0 or 5xx)                                                              | status 0 passes the scripts' `< 500` check                |
| machine   | Day 27's: Docker VM 8 CPUs, 3.84 GiB                                                                  |                                                           |

**Results** (`scripts/replicas.sh`, n = 1 each):

| Run          | Bookings | Invariant                          | 201s per replica (api-1/2/3) | Requests per replica | Retries |
| ------------ | -------- | ---------------------------------- | ---------------------------- | -------------------- | ------- |
| single       | 10       | 0 + 10 = 10 ✓                      | 2 / 4 / 4                    | 163 / 169 / 168      | 0       |
| forupdate    | 10       | 0 + 10 = 10 ✓                      | 3 / 2 / 5                    | 166 / 168 / 166      | 0       |
| optimistic   | 10       | 0 + 10 = 10 ✓                      | 4 / 2 / 4                    | 167 / 165 / 168      | 1,924   |
| serializable | 10       | 0 + 10 = 10 ✓                      | 1 / 6 / 3                    | 170 / 167 / 163      | 326     |
| **unsafe**   | **500**  | **7 + 500 = 507 ✗ (oversold 497)** | 168 / 168 / 164              | 168 / 168 / 164      | 0       |
| retry.js     | **1**    | 9 + 1 = 10 ✓                       | 0 / 1 / 0                    | 165 / 166 / 169      | 0       |

retry.js responses: 1 × 201, **170 × 200 (replay)**, 329 × 409 (in progress).
Day 3, one instance: 1 × 201, 0 replays, 499 × 409.

**Observations:**

- **The invariant held on all 4 strategies with every replica winning
  seats.** The guarantee is Postgres's (row lock on the decrement, `CHECK`,
  `FOR UPDATE`, version check, `40001`): 3 processes are 3 clients.
- **The control oversold by 497 through the same setup**, so the test can
  see a violation; 10 in the safe runs is evidence, not luck.
- **Idempotency held across processes: one booking from 500 copies of a
  key.** The unique index on `idempotency_keys` decides, not a process.
  More replays than Day 3 (170 vs 0): the burst took longer through 3
  replicas, so the winner had finished before part of it arrived. Both
  answers are correct.
- **Retries:** serializable 326 vs Day 11's 334 on one instance (n = 1 each,
  older setup): no difference shown. Optimistic 1,924 (~190 per booking) has
  no single-instance baseline at 10 seats (Day 10's table is `[TODO]`).
- **Round-robin spread requests within 160–170 per replica.**
- **First run lost 244 of 500 requests at nginx (EOF)**, a scratch run
  before the real ones: `worker_connections` defaults to 512 and counts
  client and upstream connections. Raised to 4,096. The scripts' `< 500`
  check passed those requests (status 0); `replicas.sh` now marks such a
  run invalid.

**Open:**

- Replicating the workers (relay, payments, consumers): `SKIP LOCKED` and
  consumer groups should share the work; not measured
- PgBouncer instead of shrinking pools per replica
- Latency per strategy with replicas vs one instance (same machine, n ≥ 3)
- k8s manifests + HPA (dropped from the plan for time)

## Day 30

**Goal:** the API on Kubernetes with probes and resource limits, and a
HorizontalPodAutoscaler scaling it under load (plan Days 36–37).

**Settings:**

| Setting      | Value                                                                                                                 | Why                                                            |
| ------------ | --------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------- |
| cluster      | kind, node v1.37.0; kubectl v1.37.1; metrics-server v0.8.0 (`--kubelet-insecure-tls`)                                 | Docker Desktop's built-in Kubernetes never answered (API EOF)  |
| dependencies | Postgres, Redis, Jaeger in compose, reached via `host.docker.internal`                                                | only the API is the subject                                    |
| pod          | requests 100m / 64Mi, limits 500m / 128Mi; readiness `/readyz` (DB ping), liveness `/healthz`                         | HPA % is of the request; liveness must not depend on Postgres  |
| pools        | `API_DB_MAX_CONNS=20` + read 10 per pod                                                                               | 3 × 30 = 90 of 100 connections                                 |
| HPA          | CPU 50% of request, min 1, **max 3**; default behaviour (15 s sync, 5 min scale-down window)                          | the database caps the replica count                            |
| load         | k6 **inside** the cluster against the Service; `GET /units/{id}`, 30 s ramp to 400/s, 4 min hold; `noConnectionReuse` | kube-proxy balances connections; keep-alive would pin old pods |
| GOMAXPROCS   | logged **2** (Go 1.26: limit 0.5 CPU → `max(2, ceil(0.5))`)                                                           | read from the pod, not assumed                                 |

**HPA timeline** (`kubectl get hpa -w`, `get pods -w`, events):

| Time           | Observed CPU (avg of pods, % of request) | Replicas | Event                                    |
| -------------- | ---------------------------------------- | -------- | ---------------------------------------- |
| 02:25:29       | —                                        | 1        | load Job created (k6 image pulled first) |
| 02:26:00       | 8%                                       | 1        | load starting                            |
| 02:26:15       | 387%                                     | 1 → 3    | rescale to 3; 2 pods created             |
| 02:26:20       | —                                        | 3        | both new pods Ready (5 s after creation) |
| 02:26:45       | **498%**                                 | 3        | all 3 near their 500m limit              |
| 02:27–02:30:31 | 238–423%                                 | 3        | `ScalingLimited: TooManyReplicas`        |
| 02:30:46       | 33%                                      | 3        | load over                                |
| 02:31:01       | 2%                                       | 3        | `ScaleDownStabilized`                    |
| 02:35:31       | 2%                                       | 3 → 2    | "All metrics below target"               |
| 02:35:46       | 2%                                       | 2 → 1    |                                          |

**k6** (in-cluster): 98,791 requests, **366.1 req/s** (target 400), 0 failed;
median **6.78 ms**, p95 **668 ms**, p99 **2.87 s**, max 3.5 s;
**dropped 3,357 (3.3%)**, 400 VUs reached.

**CPU throttling** (`/sys/fs/cgroup/cpu.stat`, since pod start):

| Pod               | Periods | Throttled     | Time throttled | CPU used |
| ----------------- | ------- | ------------- | -------------- | -------- |
| 2q2bh (first pod) | 2,972   | **687 (23%)** | **93.7 s**     | 96.1 s   |
| 57tqq             | 2,589   | 356 (14%)     | 32.0 s         | 81.5 s   |
| pzjj5             | 2,607   | 306 (12%)     | 23.0 s         | 81.0 s   |

**Observations:**

- **Scale-up took one HPA sync after the metric showed the load**, and new
  pods were Ready in 5 s. The slow part is the metric (metrics-server +
  HPA's 15 s loop), not the pod.
- **The cap held:** at 498% on 3 pods the formula asks for
  `ceil(3 × 498 / 50) = 30`. `maxReplicas: 3`, set by Postgres's 100
  connections, kept it at 3.
- **3 pods at their limit could not serve 400 req/s:** 3.3% dropped, p99
  2.87 s against a 6.78 ms median. By Day 28's rule (dropped > 1%) this
  capped deployment broke at this load.
- **The tail is CFS throttling, measured:** the first pod was paused for
  93.7 s of a ~5 min load (23% of its periods). Requests wait out the rest
  of each 100 ms period, and queue behind each other.
- **Kernel time exceeded user time** (52 s vs 44 s on the first pod): a new
  TCP connection per request (`noConnectionReuse`). The demo overstates
  per-request CPU (~2.6 ms here: 258.6 s / 98,791) versus keep-alive
  clients.
- **Scale-down came 5 min after the load ended, in two steps** (3 → 2 → 1,
  15 s apart): the HPA takes the highest recommendation of the last 5 min;
  the 33% sample (→ 2 pods) left the window 15 s after the 238% one (→ 3).

**Open:**

- No CPU limit (requests only) vs a limit: throttling vs noisy neighbours
- Scaling on a better signal than CPU (outbox backlog age, p99) via a
  Prometheus adapter or KEDA
- PgBouncer, so `maxReplicas` isn't set by `max_connections`
- The workers (relay, payments, consumers) on Kubernetes; Postgres managed
- Keep-alive load with a request-level balancer (an ingress or service mesh)
