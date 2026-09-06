## Day 2 — <date>

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
