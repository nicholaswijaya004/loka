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
