# Loka

A booking service for perishable inventory (rooms, cabins, seats), built over
30 days to answer one question properly, and then the questions that came
after it:

1. **What actually prevents overselling** when hundreds of requests compete for
   the last unit?
2. How does a booking, its event and its payment **stay consistent** when they
   live in three systems that can each fail on their own?
3. **How much load can it take,** and what gives way first?

Go · PostgreSQL · Kafka · Redis · OpenTelemetry · Prometheus · Grafana ·
k6 · Docker · Kubernetes (kind)

Every number in this README was measured and comes from
[`docs/metrics.md`](docs/metrics.md). That file also records each run's
machine, settings and caveats. The design decisions are in three ADRs
(see [Documents](#documents)).

---

## The finding

500 concurrent requests against a 10-seat unit. The code paths are identical
except for one line of SQL.

| Decrement | Bookings | `available_units` | Invariant | Oversold |
|---|---|---|---|---|
| `SET available_units = available_units - $1` | **10** | 0 | 0 + 10 = 10 ✓ | 0 |
| `SET available_units = $1` (computed in Go) | **500** | 8 | 8 + 500 = 508 ✗ | **490** |

**The first version is correct because the subtraction happens inside a single
statement.** Postgres holds the row lock for that statement and evaluates
`available_units - $1` against the current committed value, not against
whatever the application read a moment earlier. The 11th request fails
`CHECK (available_units >= 0)` and becomes a 409.

**The second version trusts a stale read.** Every request computes `10 - 1 = 9`
and writes `9`. Fifty requests writing 9 have the same effect as one.

**`chk_availability` fired 490 times in the safe run and zero times in the
unsafe one.** A value that is stale but legal gives the constraint nothing to
reject. A constraint only protects an invariant the database can evaluate.

**The unsafe run returned no HTTP errors.** Nothing in the responses showed
the bug; only the invariant check after the run did. That became the rule for
the rest of the project: **every experiment ends by checking the invariants,
not the status codes.**

---

## Architecture

~~~
                     ┌──────────── Redis (unit cache) ◄── cmd/cacheinvalidator
                     │                                          ▲
 client ──HTTP──► cmd/api ──one transaction──► PostgreSQL       │
                             booking + seats   ├ bookings       │
                             + outbox row      ├ inventory_units│
                                               ├ outbox_events ─┼─► cmd/relay ──► Kafka "booking-events"
                                               ├ payments       │                    │
                                               └ processed_events                    ├─► cmd/payments (saga)
                                                                                     │     consumer + worker
                                                                                     │     + expirer ──► cmd/paymock
                                                                                     ├─► cmd/notifier
                                                                                     └─► cmd/cacheinvalidator

 traces: OpenTelemetry → Jaeger    metrics: /metrics → Prometheus → Grafana
~~~

| Binary | Role |
|---|---|
| `cmd/api` | HTTP API. Writes the booking, the seat decrement and the `booking.created` event in **one transaction**. Reads units through Redis. |
| `cmd/relay` | Polls `outbox_events` (`FOR UPDATE SKIP LOCKED`) and publishes to Kafka in batches. |
| `cmd/payments` | The payment saga: moves bookings to `payment_pending`, charges the provider, then confirms or compensates. It also expires unpaid bookings. |
| `cmd/notifier` | An idempotent consumer: one notification per event, however many times it is delivered. |
| `cmd/cacheinvalidator` | Deletes a unit's cache entry when its availability changes. |
| `cmd/paymock` | A payment provider to fail against: latency, declines, lost responses, idempotency keys. |

Inside the API the layers depend in one direction only:
`internal/api` → `internal/booking` → `internal/storage` → Postgres.
`booking` defines the `Store` interface it needs, so it can be tested without
a database.

---

## What is guaranteed, and how it was proven

**Rule: if an invariant can be written in the schema, it is written there.**
Application code will have bugs and will run as several copies. The database
is the one place that every copy, and every bug, has to go through.

| Guarantee | Mechanism | Proven by |
|---|---|---|
| Seats never go below 0 or above capacity | single-statement decrement + `CHECK (available_units >= 0 AND available_units <= total_units)` | 500 requests → exactly 10 bookings; the unsafe control oversold 490 |
| A retried request books at most once | `Idempotency-Key`, claimed by `INSERT` into a primary key **before** any work | 500 copies of one key → **1** booking (10 before the fix) |
| A failure leaves no half-booking | decrement + insert in one transaction | separate statements lost 2 seats; the transaction lost 0, even with 30% injected failures (all 5 rolled back) |
| No booking is charged twice | partial unique index: one `succeeded` payment per booking; idempotency key at the provider | lost-response test: one charge, one payment row |
| A booking and its event are never separated | transactional outbox: the event is written in the same transaction | relay killed after publishing: duplicates, never a loss |
| A duplicate event has no second effect | `processed_events (consumer, event_id)`, `INSERT … ON CONFLICT DO NOTHING`, in the handler's transaction | 11 Kafka messages → 8 effects; a consumer crash after its commit → no double effect |
| All of the above, under failure | the chaos test (below) | **10,000 bookings, 0 invariant violations** |
| All of the above, on several replicas | the same rules live in Postgres, not in a process | 3 replicas: 10 bookings for each of the 4 strategies; one key → 1 booking |

Two results from the early days shaped everything after them:

- **Tests can pass by luck.** An unsafe in-memory inventory booked 34 seats of
  10, yet its test **passed on the first run**. The race detector flagged it
  on that same run. An unsafe counter lost 61–63% of its increments, and never
  the same amount twice.
- **Unit tests can't see SQL.** With the constraint name misspelled in the
  error mapping, every unit test passed. The integration test (Testcontainers,
  real Postgres) failed, because a sold-out booking returned 500 instead of
  409.

---

## Choosing a concurrency strategy

Four correct strategies, all in the code, measured in one session (10 seats,
500 VUs, median of 3 runs):

| Strategy | p95 | req/s | Retries |
|---|---|---|---|
| single-statement decrement | 864 ms | 473 | 0 |
| `SERIALIZABLE` | 865 ms | 539 | 144 |
| optimistic (`version` column) | 1.91 s | 244 | 1,070 |
| `SELECT … FOR UPDATE` | 2.14 s | 202 | 0 |

They form **two tiers, not four places**. The same `SERIALIZABLE` code drifted
about 19% between two sessions 20 minutes apart, so on this laptop a gap under
~20% is noise. Earlier tables that mixed days were withdrawn because of that.

**Decision ([ADR-002](docs/ADR-002-concurrency-control.md)):** booking uses
the single-statement decrement. A future rule that spans more than one row uses
`SERIALIZABLE` with the existing retry loop.

---

## Events and the payment saga

**Transactional outbox.** The API never talks to Kafka. The booking and its
`booking.created` row commit together, and the relay publishes later. When the
relay was killed between publishing and marking the batch, the 3 events were
published again with the same `event_id`. The result is duplicates and never a
loss, so **every consumer must deduplicate**, and they all do.

**Payment saga ([ADR-003](docs/ADR-003-payment-saga.md)).** A charge is an HTTP
call to another company, so it can never be inside a database transaction. A
booking moves through `pending → payment_pending → confirmed | cancelled`, and
each move is a compare-and-set on the status.

- **The charge is outside any transaction.** The outcome is applied in one
  transaction: the payment row, the status and the event. A decline releases
  the seats in that same transaction (compensation).
- **A lost response is never treated as a failure.** The booking stays
  `payment_pending` until its lease expires. The retry reuses the idempotency
  key, and the provider replays the original charge. In the test, the booking
  was confirmed 30.2 s after the first attempt, with one charge.
- **Retries use exponential backoff with full jitter,** plus a circuit breaker.
  During a 5½-minute provider outage there were 37 failed attempts, and only
  **13 reached the provider** (the first burst of 6, then 7 probes). All 6
  bookings were confirmed afterwards, each charged once.
- **Unpaid bookings expire.** `pending → cancelled` releases the seats.

### The chaos test

`internal/chaos` builds the real binaries, sends paced load, SIGKILLs
`cmd/payments` and the relay mid-run, keeps the relay down until bookings
expire, and sets the provider to 30% declines and 10% lost responses. Then it
waits for the system to drain and checks every invariant: seats, payments per
booking, charged ⇔ confirmed against the provider's ledger, every event
published and processed, nothing stuck.

| Run (MacBook Pro) | Confirmed / declined / expired | Charges / calls | Violations |
|---|---|---|---|
| 10,000 bookings at 25/s | 6,312 / 2,543 / 1,146 | 8,855 / 9,452 | **0** |

What it found along the way:

- **The batch worker had head-of-line blocking.** One lost response held the
  whole batch of 10 for the 10 s timeout. Slots that refill one at a time cut
  the drain time by **73–75%**.
- **A SIGKILLed consumer blocks its partitions for about 45 s,** the Kafka
  session timeout, because it never leaves the group. Every booking that
  arrives in that gap waits, and some expire.
- **A trace found a shutdown bug.** A consumer stopped with Ctrl-C hung until
  a second Ctrl-C and stayed in the group until its session timed out, so its
  replacement waited the same 45 s. With `CloseAllowingRebalance` it exits and
  leaves the group, and the new process charged its first booking 5.7 s after
  starting.

---

## Performance: what it takes, and what gives way

### The breaking point

Steps of the steady mix (POST : GET booking : GET unit = 2 : 1 : 4), each from
a fresh reset. A step counted as **broken** when POST p99 > 1 s, failures > 1%
or dropped requests > 1%; that rule was decided before any run. The machine
was a GitHub Codespace with 4 vCPUs, and k6 ran on the same machine.

| Step | Offered req/s | POST p50 | POST p99 | Dropped | Write-pool avg waiters | Verdict |
|---|---|---|---|---|---|---|
| ×1 | 87 | 6.2 ms | 21.5 ms | 0 | 0 | ok |
| ×4 | 350 | 12.0 ms | 150.7 ms | 0 | 0 | ok |
| ×12 | 1,050 | 32.6 ms | 461.5 ms | 0 | 3.9 | ok |
| ×14 | 1,225 | 51.3 ms | 597.4 ms | 0.10% | 14.7 | ok |
| ×16 | 1,400 | 99.5–198 ms | 722–1,170 ms | 0.23–0.85% | 44–87 | **edge** (1 of 3 runs broken) |
| ×20 | 1,750 | **832 ms** | **1,771 ms** | **4.93%** | 292 | **broken** |

**Breaking point: about 1,400 req/s (400 POST/s).** What gave way, in order:

1. **Payments, from ×4.** 10 charges in flight against a provider that takes
   100 ms is at most 10 / 0.1 s = **100 charges/s** (Little's law). The backlog
   grows from there. The API never noticed: confirmations just fell further
   behind.
2. **CPU, from ×4–×6.** Idle time fell to 2–9%, and 40–47% of the time went to
   the kernel (networking).
3. **The write pool, from ×6.** It is where the queue forms, not the cause:
   with the CPU full, every transaction holds its connection longer. A bigger
   pool would have no CPU to run more transactions.
4. **The API, between ×16 and ×20.** 25% more load gave **8× the median**
   latency. This is the queueing cliff.

**What held:** the relay. Outbox lag p99 stayed between 517 and 658 ms at every
step, and no request failed at any step.

### Changes that were measured

| Change | Result |
|---|---|
| Relay: one Kafka produce call per batch instead of one per event | **358 → 4,660 events/s (13×)**; outbox lag p99 2,630 → 579 ms |
| Redis cache for units | Postgres unit reads **−59%** at 200/s and **−38%** at 100/s, but reads were **not faster**: at a 59% hit rate, p95 was about **2× slower** than without the cache |
| Separate read pool | no change shown in GET p99: the tail came from machine stalls, which reach every pool |
| `GOMAXPROCS` 16 → 2 | no effect shown |

Three lessons came out of those runs:

- **On a laptop, p99 shows whether a stall happened.** On identical code, p99
  ranged from 79 to 1,071 ms across runs while p50 stayed at 29–59 ms. An
  improvement only counts when the worst run after a change beats the best run
  before it.
- **A cache's hit rate follows reads per invalidation, not the TTL.** Every
  booking invalidates its unit. In the test each unit was booked about every
  20 s, so the 30 s TTL never came into play.
- **One booking takes 1.24 s end to end, and ~90% of that is polling.** A hop
  through Kafka costs about 5 ms. A hop through a polled table costs up to one
  interval (500 ms).

### Measuring the measurement

The Grafana dashboard computes percentiles from Prometheus histograms. Checked
against k6 on the same runs, p50 was within 2% but **p99 read 21–25% high,**
because it fell in the 2.5–5 s bucket and `histogram_quantile` interpolates
inside a bucket. Percentiles are therefore summed from buckets across
instances, never averaged, and wide buckets are read as ranges.

---

## Scaling out

**3 replicas behind nginx.** All four strategies sold exactly 10 seats, and
every replica won some of them. The unsafe control, through the same setup,
**oversold by 497**, which shows the test can fail. 500 copies of one
idempotency key created 1 booking: 1 × 201, 170 replays, 329 × 409. Correctness
didn't depend on the number of processes, because it never lived in a process.

The limit is connections: 3 × (20 write + 10 read) = **90 of Postgres's 100**.

**Kubernetes (kind) with a CPU HorizontalPodAutoscaler**, 1–3 pods, CPU limit
500m:

- **Scale-up came one HPA sync after the metric showed the load,** and new pods
  were ready in 5 s. The slow part is the metric, not the pod.
- At 498% CPU the formula `ceil(3 × 498 / 50)` asked for **30 pods**.
  `maxReplicas: 3` held it at 3 (`ScalingLimited`). That cap is set by the
  database's connection limit, not by Kubernetes.
- At 400 req/s, 3 capped pods dropped 3.3% of requests, and p99 was 2.87 s
  against a 6.78 ms median. **The tail was CFS throttling, measured:** the first
  pod was paused for 93.7 s, in 23% of its scheduling periods.
- Scale-down came 5 minutes after the load ended, in two steps (3 → 2 → 1).
- Go set `GOMAXPROCS` to 2 under the 0.5-CPU limit, as read from the pod.

---

## What I'd do next at 100×

These come from what broke or nearly broke, not from a checklist:

- **Payments throughput.** It is the first ceiling (100 charges/s). Run more
  charges in flight, or more workers: `SKIP LOCKED` already lets them share the
  table.
- **PgBouncer,** so the replica count isn't capped by `max_connections`.
- **Scale on a better signal than CPU,** such as outbox age or p99, through a
  Prometheus adapter or KEDA.
- **Push instead of poll** (for example `LISTEN/NOTIFY`), since polling is ~90%
  of end-to-end latency.
- **Static group membership** or a shorter session timeout, so a crashed
  consumer doesn't block its partitions for 45 s.
- **A dead-letter topic.** Poison messages are only logged and skipped
  today.
- **Better measurements:** k6 on a separate machine, and a soak test (one load
  for an hour) to show data growth and backlog carried between steps.

---

## Caveats

- **Most runs are from laptops** (an Intel MacBook running Docker Desktop, and
  earlier WSL2). The MacBook throttled its CPU to about 20% when hot, which
  made it useless for the breaking point. Figures are compared only within one
  machine and one session.
- **The load generator shares the machine,** including in the Codespace, so
  the API never had all 4 vCPUs.
- **Many results are n = 1 or a median of 3.** `docs/metrics.md` gives the
  ranges and marks what isn't shown.

---

## Run it

Requires Docker, Go 1.26+, and
[`golang-migrate`](https://github.com/golang-migrate/migrate). Load tests need
[k6](https://k6.io).

~~~bash
make reset            # fresh Postgres, Kafka and Redis; migrations; seed; Kafka topic
make run              # the API on :8080
~~~

The full pipeline is one process per binary:

~~~bash
go run ./cmd/paymock            # :8081
go run ./cmd/relay
go run ./cmd/payments
go run ./cmd/notifier
go run ./cmd/cacheinvalidator
~~~

Make a booking. Send it twice and the second response is a replay:

~~~bash
curl -i -X POST localhost:8080/bookings \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: my-first-booking' \
  -d '{
    "unit_id": "22222222-2222-2222-2222-222222222222",
    "customer_id": "11111111-1111-1111-1111-111111111111",
    "qty": 1,
    "visit_date_time": "2026-10-01T10:00:00Z"
  }'
~~~

### Tests and experiments

| Command | What |
|---|---|
| `make test` | unit tests, race detector on |
| `make test-integration` | integration tests against a real Postgres (Testcontainers) |
| `go test -tags=chaos -count=1 -v -timeout=30m ./internal/chaos/` | the chaos test (`CHAOS_BOOKINGS`, `CHAOS_RATE`) |
| `STRATEGY=single ./scripts/bench.sh scripts/k6/contention.js` | 500 VUs on 10 seats, then the invariant check |
| `UNSAFE_DECREMENT=1 ./scripts/bench.sh scripts/k6/contention.js` | the control: must oversell |
| `LABEL=base RUN=1 ./scripts/loadrun.sh` | one measured steady-load run of all six binaries |
| `STEPS="1 2 4 8 12 16 20" LABEL=step RUN=1 ./scripts/steprun.sh` | the breaking point |
| `STRATEGY=single ./scripts/replicas.sh scripts/k6/contention.js` | 3 replicas behind nginx |
| `go build -o bin/relay ./cmd/relay && RELAY_BIN=bin/relay LABEL=x RUN=1 ./scripts/draintest.sh` | the relay's maximum publish rate |
| `make monitoring-up` | Prometheus on :9090, Grafana on :3000 (dashboard provisioned) |
| [`k8s/README.md`](k8s/README.md) | the API on kind with an HPA |

---

## Documents

| File | What |
|---|---|
| [`docs/ADR-001-data-model.md`](docs/ADR-001-data-model.md) | schema, constraints, money as `bigint` minor units, no card numbers stored |
| [`docs/ADR-002-concurrency-control.md`](docs/ADR-002-concurrency-control.md) | the four strategies and the decision |
| [`docs/ADR-003-payment-saga.md`](docs/ADR-003-payment-saga.md) | the saga, its failure table, and why compensation is safe |
| [`docs/metrics.md`](docs/metrics.md) | every measurement, day by day, with settings and caveats |
| [`docs/journal.md`](docs/journal.md) | what I learned each day |
