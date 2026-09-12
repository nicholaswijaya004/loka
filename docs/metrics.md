# Metrics

Measurements recorded as the project was built. Each entry states what was run,
what was observed, and what it showed.

**Environments.** Day 2 was measured on macOS (Go 1.27). Days 3 onward were
measured on WSL2 (Ubuntu 24.04), HP EliteBook 840 G7, 4c/8t 15W CPU, 8 GB RAM,
Postgres 16 in Docker capped at 512 MB, connection pool `MaxConns = 50`, with k6
**co-located with the service under test**.

Absolute throughput and latency are therefore not representative — the load
generator competes with the service for the same four cores, and the service
competes with Postgres. What these figures support is *relative* comparison: two
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

| Constraint | Attempt | Result |
|---|---|---|
| `chk_availability` | `UPDATE inventory_units SET available_units = -1` | rejected — 23514 |
| `chk_availability` | `SET available_units = 50` (total 10) | rejected — 23514 |
| `chk_booking_status` | `INSERT ... booking_status = 'banana'` | rejected — 23514 |
| `idx_payments_one_success_per_booking` | 2 failed + 1 succeeded payment | all accepted |
| `idx_payments_one_success_per_booking` | a second `succeeded` payment | **rejected** — 23505 |
| `idempotency_keys` PK | same key inserted twice | rejected — 23505 |

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

| | |
|---|---|
| Signal received | 02:49:07.722 |
| Shutdown complete | 02:49:11.961 |
| Drain | **4.24 s** |
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

| Decrement strategy | SQL | Bookings | `available_units` | Invariant | Overbooked |
|---|---|---|---|---|---|
| Computed in SQL | `SET available_units = available_units - $1` | **10** | 0 | 0+10=10 ✓ | 0 |
| Computed in Go | `SET available_units = $1` | **500** | 8 | 8+500=508 ✗ | **490** |

The second row runs with `UNSAFE_DECREMENT=1 make run`. Identical code paths
apart from that one statement.

**Why the SQL version holds.** The decrement is a single statement. Postgres
takes a row lock for its duration and evaluates `available_units - $1` against
the *current committed value*, not against whatever the application read moments
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
499 conflicts. That case is *easier* than the 10-seat one — the first decrement
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

| | Before | After |
|---|---|---|
| Responses | 201, 201 | 201, **200** |
| `Idempotent-Replay` | — | `true` on the second |
| Booking ids | two distinct | **identical** |
| Bookings | 2 | **1** |
| `available_units` | 10 → 8 | 10 → **9** |

**Concurrent — 500 VUs sharing one key:**

| | Before | After |
|---|---|---|
| Bookings created | 10 | **1** |
| `available_units` | 10 → 0 | 10 → **9** |
| Seats lost to duplicates | 9 | **0** |
| 409 in-flight | — | 499 |
| 5xx | 0 | 0 |
| p95 | 215 ms | 684 ms |

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

- Throughput and p99 for four *correct* concurrency strategies — single
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