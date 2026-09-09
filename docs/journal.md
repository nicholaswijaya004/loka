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