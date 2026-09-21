# ADR-002: Concurrency control for inventory booking

**Status:** Accepted
**Date:** 2026-09-21
**Evidence:** `docs/metrics.md`, Days 5–13; integration tests in `internal/storage` and `internal/booking`

## Context

Loka sells perishable inventory: a unit has a fixed number of seats, and many
requests can race for the last few at once. The one rule that must never break:

> `available_units + seats booked = total_units`, and `available_units` never goes below 0.

Day 5 showed the naive approach breaks it. The unsafe path reads availability,
computes the new value in Go, and writes it back, which loses updates under load.
Four correct strategies were then built and measured:

| Strategy             | How it stays correct                                                                                                             |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| **single-statement** | `UPDATE … SET available_units = available_units - $1`; a `CHECK (available_units >= 0)` constraint rejects the overselling write |
| **`FOR UPDATE`**     | lock the row on read, so every request queues behind the current holder                                                          |
| **optimistic**       | version column; a stale write updates 0 rows and the request retries                                                             |
| **`SERIALIZABLE`**   | Postgres detects conflicting transactions and aborts one with `40001`; the request retries                                       |

## Measurements

One session, 10 seats, 500 concurrent requests, median of three runs:

| Strategy          | p95    | req/s | Retries |
| ----------------- | ------ | ----- | ------- |
| single-statement  | 864 ms | 473   | 0       |
| `SERIALIZABLE`    | 865 ms | 539   | 144     |
| optimistic (r=50) | 1.91 s | 244   | 1,070   |
| `FOR UPDATE`      | 2.14 s | 202   | 0       |

All four sold exactly 10 seats and held the invariant on every run. Two sessions
of the same code 20 minutes apart differed by about 19%, so gaps smaller than
that are noise. The strategies form two tiers: single-statement and
`SERIALIZABLE` are tied, and optimistic and `FOR UPDATE` are about 2.2–2.5×
slower.

Earlier tables that compared numbers across days (Days 9–11) showed larger gaps.
Those comparisons are withdrawn: the same code measured on different days varied
by up to 1.8×.

## Decision

**Booking uses the single-statement conditional decrement, guarded by the
`chk_availability` constraint.**

**Any future rule that spans more than one row uses a `SERIALIZABLE`
transaction** with the existing retry loop. The retry wraps the whole
transaction, has a budget of 50, uses jittered backoff, and doesn't sleep after
the final attempt.

`FOR UPDATE` and optimistic locking stay in the code as benchmark strategies
only. Neither is used for new features.

## Why

**The invariant is a property of one row.** "`available_units` never below 0"
depends only on the row being written, so Postgres can enforce it as a `CHECK`
constraint. The decrement is relative (`available_units - $1`), not an absolute
value computed from an earlier read. Under `READ COMMITTED`, a second writer
waits for the row lock, re-reads the committed value, and applies its decrement
to that value. It either succeeds or violates the constraint and is reported as
sold out.

The availability check in Go before the `UPDATE` is only a fast path that lets
requests after sell-out skip the write. Correctness never depends on it.

**Nothing to gain from paying more.** `SERIALIZABLE` ties on speed but adds
retries (a median of 144 per run) and a retry budget that has to be tuned.
Day 10 showed a budget of 5 left 41 seats unsold while the invariant still held.
`FOR UPDATE` makes even rejected requests queue for the lock. Optimistic retries
about 7× more than `SERIALIZABLE` under the same load. For a single-row
invariant, all three add cost without adding safety.

**`SERIALIZABLE` covers the cases single-statement can't.** A rule such as "at
most 2 cabins per customer per cruise" depends on several rows. No row-level
`CHECK` can express it, and two concurrent requests could each see 1 cabin and
both insert. `SERIALIZABLE` detects that read-write cycle: the Day 12 write-skew
test shows the loser failing at `COMMIT` whichever transaction commits first.

**The saga doesn't change this.** Reserve → charge → confirm (week 4) spans an
external payment call, which can't sit inside a database transaction however
strict the isolation level is. Consistency across those steps comes from
compensating actions and the transactional outbox. Each individual step stays a
single-row operation: reserving is this decrement, and releasing is the matching
increment.

## Consequences

**Gains**

- The fastest correct option, with no retry budget to tune on the booking path.
- The guarantee lives in the database schema, not in application code, so a new code path can't bypass it by accident.

**Costs and obligations**

- **The constraint name is load-bearing.** `DecrementAvailability` recognises the violation by the string `"chk_availability"`. With that string misspelled, `internal/booking` stayed green at 100% coverage, while a sold-out request would have returned 500 instead of 409. `TestDecrementAvailabilityRejectsOverbooking` guards it, so renaming the constraint means updating both together.
- **Changes must stay relative.** Any change to `available_units` has to be relative and constraint-checked. Writing an absolute value computed in Go reintroduces the Day 5 lost update. The unsafe path is kept only as a demonstration, behind `UNSAFE_DECREMENT`.
- **Multi-row rules need a `SERIALIZABLE` transaction.** Adding one to a plain transaction would compile, pass the unit tests, and oversell under load. Integration tests should cover each such rule with a concurrent write-skew case.
- **Two strategies in use.** Reviewers need to know which one a code path uses and why. This ADR is that reference.
- **Idempotency is separate.** Concurrency control stops _different_ requests overselling. Duplicate submissions of the _same_ request are handled by idempotency keys (Day 6, tested on Day 12). Both are needed.

## When to revisit

- **A second invariant arrives** that spans rows. Expected; follow the Decision above.
- **Held reservations.** If seats are held for a period before payment, reservation becomes a row with its own lifecycle rather than a decrement, and this analysis needs redoing.
- **A single hot unit becomes the bottleneck** in production metrics: row-lock waits dominate p99 on one unit. Options then include splitting inventory into buckets, or queueing requests per unit.
- **The database changes.** A distributed or sharded store, or writes routed to replicas, removes the single-row atomicity this decision relies on.

## Limits of the evidence

- Laptop measurements (macOS, Docker Desktop), with k6 on the same machine. Treat the tiers as meaningful and the absolute latencies as not.
- The 1-seat runs measure how fast sold-out requests are rejected, not how conflicts are handled. In at least one run the only winner finished before most other requests reached the database.
- Retry counts in the integration tests are capped by the pool size and aren't comparable with benchmark retries.
- Why optimistic retries about 7× more than `SERIALIZABLE` is not yet explained. The candidates are the longer gap between read and write, and the extra read to tell a conflict apart from a missing unit.
