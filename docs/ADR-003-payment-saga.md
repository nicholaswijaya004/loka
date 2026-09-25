# ADR-003: Payment saga

**Status:** Proposed. Implementation starts Day 18.
**Date:** 2026-09-25
**Builds on:**

- ADR-001: data model and constraints;
- ADR-002: concurrency control;
- Day 15: outbox and relay;
- Day 16: the idempotent consumer.

## Context

A booking must be paid for before it is confirmed. The payment happens at an
external provider over HTTP, so it can't share a database transaction with the
booking. Whatever isolation level is used, a transaction can't include a
network call to another company's system. So a booking now spans several steps
that commit separately:

1. **reserve:** seats decremented, booking `pending` (already done at creation);
2. **charge:** the provider takes the money;
3. **confirm** the booking, or **compensate:** cancel it and release the seats.

Any step can fail, or be interrupted by a crash, between the others. The design
must guarantee:

- **no customer is charged twice** for one booking;
- **no paid booking is ever cancelled** because its outcome was unknown;
- **no seat is released twice,** or released while still sold;
- every booking ends **confirmed** or **cancelled**, or is **escalated to a human**. None is silently stuck.

The payment provider is simulated by `cmd/paymock`. It is idempotent on the
`Idempotency-Key` header, and can be configured to be slow, to decline, or to
succeed without answering in time.

## Booking states

```
pending ──(charge starts)──▶ payment_pending ──(succeeded)──▶ confirmed
   │                              │
   └──(expired unpaid)──┐         └──(declined)──▶ cancelled
                        ▼
                    cancelled
```

| From              | To                | Trigger                          |
| ----------------- | ----------------- | -------------------------------- |
| `pending`         | `payment_pending` | the saga starts the charge       |
| `payment_pending` | `confirmed`       | the provider says `succeeded`    |
| `payment_pending` | `cancelled`       | the provider says `declined`     |
| `pending`         | `cancelled`       | expiry: unpaid past the deadline |

- `confirmed` and `cancelled` are final for this saga. Refunds after
  confirmation are a separate, future flow.
- Every move goes through `booking.transition`:
  - `checkTransition` rejects any move not in the table, as `ErrIllegalTransition`;
  - `TransitionBookingStatus` applies it with a compare-and-set
    (`WHERE booking_id = $1 AND booking_status = $from`), returning
    `ErrStatusConflict` if another process moved the booking first.

`payment_pending` exists so that other processes can see a charge is in flight.
Expiry only cancels `pending`. It can never cancel a booking whose payment might
be succeeding right now.

## Decision

The saga is split into two components with different jobs.

### 1. The payments consumer: starts the saga

- An idempotent consumer (Day 16 framework), named `payments`, subscribed to `booking.created`.
- Its `Handle` does one database step, inside the consumer's transaction: `pending → payment_pending`.
- It makes **no network calls.** The transaction is milliseconds long.
- An `ErrStatusConflict` (for example, the booking already expired) means there's nothing to charge. The event is processed, with no action.

### 2. The payment worker: charges, then confirms or compensates

A polling loop, like the relay. It finds bookings in `payment_pending`, and for each one:

1. **Charges the provider, outside any transaction,** with
   `Idempotency-Key: <booking_id>` and a client timeout.
2. **Applies the outcome in one transaction:**

| Outcome                                | Writes, all in one transaction                                                                                                                                                     |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **succeeded**                          | `payment_pending → confirmed` · insert a `payments` row (`charge_id`, amount, `succeeded`) · outbox `booking.confirmed`                                                            |
| **declined**                           | `payment_pending → cancelled` with `failure_reason` · release seats: `available_units = available_units + qty` · insert a `payments` row (`declined`) · outbox `booking.cancelled` |
| **no answer** (timeout, provider down) | nothing. The booking stays `payment_pending` and is retried on a later poll                                                                                                        |

If the transition in step 2 returns `ErrStatusConflict`, another worker has
already recorded this outcome. That's a duplicate, and harmless: the whole
transaction rolls back, and nothing is written twice.

### Why the charge is never inside a transaction

It would be _correct_: the provider's idempotency makes a retried charge safe.
But a transaction held open for the length of an HTTP call:

- holds a database connection, and the consumer's pool is small;
- holds row locks that other work waits on;
- freezes its Kafka partition: every booking behind it waits;
- blocks rebalances, because `BlockRebalanceOnPoll` is on.

This is the rule from Day 15: **no network call inside a transaction when it
blocks other work.**

### Why the worker also handles crash recovery

A crash at any point leaves the booking in `payment_pending`, which is exactly
what the worker polls for. On the next poll, it charges again with the same key:

- if the first charge never reached the provider, this is the real charge;
- if it did (a lost response), the provider **replays** the stored result.

Either way, the booking reaches the right final state, and the customer is
charged once. **There is no separate recovery code path.** Normal operation and
recovery are the same path.

### Escalation, not guessing

If a booking stays `payment_pending` past a threshold (proposed: 30 minutes),
the provider's answer is still unknown. The money may have been taken. The
worker therefore:

- **never cancels it,** because that could void a paid booking;
- keeps it `payment_pending`, which is the truthful state;
- logs at `Error` level, and raises an alert, e.g. a metric for bookings stuck
  over the threshold;
- leaves it for **reconciliation**: a human checks with the provider, and applies
  the right transition.

## Why compensation is safe

**Compensation is not rollback.** The seat decrement committed when the booking
was created. Releasing seats is a _new_ write that reverses its effect.

**It happens exactly once because it is tied to the transition.** The release
runs in the same transaction as `payment_pending → cancelled`, and the
compare-and-set lets only one process win that transition. A second processing
of the same decline gets `ErrStatusConflict` and rolls back, including its
release.

**The idempotency key does not protect the seats.** It only prevents a second
charge at the provider.

**`chk_availability` (`available_units <= total_units`) is a backstop, not the
protection.** It only catches a release that would exceed total capacity. A
double release with other seats still sold would stay under the limit, pass the
constraint, and create a seat that doesn't exist. That's why the transition,
not the constraint, is what guarantees exactly-once release.

A second successful payment for one booking is also impossible at the database
level: the partial unique index on `payments (booking_id) WHERE payment_status =
'succeeded'` (ADR-001).

## Failure scenarios

| What happens                                             | Result                                                                                                                                     |
| -------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| The consumer crashes before committing `payment_pending` | the event is redelivered, the claim is retried, and the transition happens once                                                            |
| The worker crashes after charging, before recording      | the booking stays `payment_pending`, the next poll charges again with the same key, the provider replays, and the outcome is recorded once |
| The provider times out, but the charge succeeded         | the same as above: resolved on a later poll                                                                                                |
| The provider is down for a long time                     | the booking stays `payment_pending`, is retried, then escalated past the threshold. Never cancelled blind                                  |
| Two workers pick the same booking                        | both charge with the same key and get one charge. One wins the transition; the other gets `ErrStatusConflict` and rolls back               |
| Expiry runs while a charge is in flight                  | expiry only matches `pending`. The booking is `payment_pending`, so nothing happens                                                        |
| The same decline is processed twice                      | the second transition conflicts, so there's no second release, no second row, and no second event                                          |

## Alternatives considered

- **Charge inside the consumer's `Handle`.** Rejected: long transactions, a
  frozen partition, and blocked rebalances (see above).
- **Choreography,** where each service reacts to the previous service's event,
  with no central flow. Rejected for now: with one payment step, an explicit
  worker is easier to read, test and reason about. Worth revisiting if the flow
  grows to many services.
- **Kafka transactions / exactly-once.** Rejected: they can't include the
  Postgres write or the provider call, which is where the real risk is.
- **Only three states, without `payment_pending`.** Rejected: expiry could cancel
  a booking whose charge was succeeding.

## Consequences

**Gains:**

- Every failure mode resolves to one charge and one final state, or to an explicit escalation.
- Recovery isn't a separate feature; it's the worker's normal loop.
- No long transactions anywhere. The only slow call is outside the database.

**Costs and obligations:**

- **Two new components to run and monitor:** the payments consumer and the payment worker.
- **The worker must poll efficiently.** It needs an index that finds `payment_pending` bookings, like the outbox's partial index.
- **Two workers may charge the same booking at once.** That's safe, thanks to the key, but wasteful. `FOR UPDATE SKIP LOCKED` while _selecting_ reduces it. The lock is released before the charge, so it's an optimisation, not the guarantee.
- **The idempotency key is the booking ID,** which means one payment attempt per booking. Letting a customer retry with a different card needs a new key scheme, e.g. `booking_id` plus attempt number.
- **Escalation needs an owner:** someone who receives the alert and can reconcile with the provider.

## Open

- **A provider lookup** (`GET /charges/{key}`) for recovery, which asks "did it happen?" without creating a charge for bookings that were never charged.
- **The expiry job** (`pending → cancelled`, with a seat release), and its deadline.
- **The retry and escalation thresholds:** values, and where they're configured.
- **Refunds** after confirmation.
- **A dead-letter topic** for poison messages (from Day 16).
