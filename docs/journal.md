# Day 2 — <date>

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


# Day 3 - <postgre sql>

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

## Day 4 — <date>
Built the naive booking endpoint. Sequential behaviour is fully correct:
201 with computed total, availability 10 → 9, 409 when sold out, 404 on
unknown unit, 400 on bad input.

The flow is deliberately unsafe — four separate round trips, no transaction:
  GetInventoryUnit → check availability → DecrementAvailability → InsertBooking
The gap between the check and the decrement is the same check-then-act race
as Day 2's UnsafeInventory, now at database scale.

**Prediction for Day 5** (500 concurrent requests against the 1-seat unit):
- successful bookings: ___
- constraint violations from chk_availability: ___
- bookings created vs seats actually sold: ___
Unlike Day 2, available_units cannot go negative — the CHECK constraint
blocks it. So I expect failures rather than corruption.

### Day 5 — Concurrency baseline (500 VUs, 10 seats, pool=50)

| Strategy | Bookings | available_units | Invariant | Overbooked |
|---|---|---|---|---|
| `SET available = available - $1` (SQL) | 10 | 0 | 0+10=10 ✓ | 0 |
| `SET available = $1` (Go-computed) | 500 | 8 | 8+500=508 ✗ | 490 |

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