#!/usr/bin/env bash
# scripts/draintest.sh
#
# The relay's maximum publish rate, with nothing else in the way: no API, no
# k6, no consumers. N unpublished events go into the outbox, only the relay
# runs, and Postgres times how long it takes until none is left.
#
#   make reset -> insert N events -> start the relay -> wait until the outbox
#   is empty -> check every event reached Kafka -> stop.
#
# The wait runs inside Postgres (a PL/pgSQL loop polling every 50 ms), so the
# timing doesn't depend on how slow `docker compose exec` is on this machine.
# It includes the relay's own startup, the same for every binary.
#
# Usage:
#   RELAY_BIN=bin/relay-before LABEL=before RUN=1 ./scripts/draintest.sh
#   RELAY_BIN=bin/relay-after  LABEL=after  RUN=1 ./scripts/draintest.sh
#
# Output: loadruns/drain-<LABEL>-<RUN>/ (summary.txt, relay.log, wait.txt)

set -euo pipefail

LABEL="${LABEL:?set LABEL, e.g. LABEL=before}"
RUN="${RUN:?set RUN, e.g. RUN=1}"
RELAY_BIN="${RELAY_BIN:?set RELAY_BIN, e.g. RELAY_BIN=bin/relay-before}"
N="${N:-20000}"
TIMEOUT_MIN="${TIMEOUT_MIN:-15}"
OUT="loadruns/drain-${LABEL}-${RUN}"

psql_loka() {
  docker compose exec -T postgres psql -U loka -d loka -v ON_ERROR_STOP=1 "$@"
}

if [[ ! -x "$RELAY_BIN" ]]; then
  echo "$RELAY_BIN not found or not executable; build it first." >&2
  exit 1
fi
if [[ -e "$OUT" ]]; then
  echo "$OUT already exists; pick another RUN or delete it." >&2
  exit 1
fi
mkdir -p "$OUT"

echo "==> Resetting containers and database"
if ! make reset >"$OUT/reset.log" 2>&1; then
  tail -n 5 "$OUT/reset.log" >&2
  exit 1
fi

echo "==> Inserting $N unpublished events"
# event_type drain.test: no consumer would act on these, and none is running.
psql_loka -q -c "
  INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
  SELECT 'booking', gen_random_uuid(), 'drain.test', jsonb_build_object('n', i)
  FROM generate_series(1, $N) AS i"
psql_loka -q -c 'CHECKPOINT' -c 'ANALYZE outbox_events' >/dev/null

echo "==> Draining with $RELAY_BIN"
# Each statement in the loop sees what has committed so far (READ COMMITTED),
# so the count drops as the relay commits its batches.
psql_loka >"$OUT/wait.txt" 2>&1 <<SQL &
DO \$\$
DECLARE
  t0        timestamptz := clock_timestamp();
  remaining bigint;
BEGIN
  LOOP
    SELECT count(*) INTO remaining FROM outbox_events WHERE published_at IS NULL;
    EXIT WHEN remaining = 0;
    IF clock_timestamp() - t0 > interval '$TIMEOUT_MIN minutes' THEN
      RAISE EXCEPTION 'not drained after $TIMEOUT_MIN minutes: % left', remaining;
    END IF;
    PERFORM pg_sleep(0.05);
  END LOOP;
  RAISE NOTICE 'drain_seconds=%', round(extract(epoch FROM clock_timestamp() - t0)::numeric, 3);
END
\$\$;
SQL
WAIT_PID=$!
sleep 0.5 # let the wait start its clock before the relay starts

"$RELAY_BIN" >"$OUT/relay.log" 2>&1 &
RELAY_PID=$!
trap 'kill -TERM "$RELAY_PID" 2>/dev/null || true' EXIT

wait_status=0
wait "$WAIT_PID" || wait_status=$?
kill -TERM "$RELAY_PID" 2>/dev/null || true
wait "$RELAY_PID" 2>/dev/null || true
if [[ $wait_status -ne 0 ]]; then
  echo "Drain failed or timed out:" >&2
  tail -n 3 "$OUT/wait.txt" >&2
  exit 1
fi

secs=$(grep -o 'drain_seconds=[0-9.]*' "$OUT/wait.txt" | cut -d= -f2)
rate=$(awk -v n="$N" -v s="$secs" 'BEGIN { printf "%.0f", n / s }')

# Marked published is not the same as in Kafka: count what the topic holds.
in_kafka=$(docker compose exec -T kafka /opt/kafka/bin/kafka-get-offsets.sh \
  --bootstrap-server localhost:9092 --topic booking-events | awk -F: '{ s += $3 } END { print s + 0 }')

read -r published failed_attempts distinct_stamps <<< "$(psql_loka -tA -F' ' -c "
  SELECT count(*) FILTER (WHERE published_at IS NOT NULL),
         coalesce(sum(attempt_number), 0),
         count(DISTINCT published_at)
  FROM outbox_events")"

{
  echo "label=$LABEL run=$RUN relay=$RELAY_BIN"
  echo "commit=$(git rev-parse --short HEAD)$(git diff --quiet HEAD -- . ':!loadruns' || echo '+dirty')"
  echo "events=$N drain_seconds=$secs events_per_second=$rate"
  echo "published=$published in_kafka=$in_kafka failed_attempts=$failed_attempts"
  # One value per batch means published_at = now() (the transaction start);
  # about one per event means clock_timestamp().
  echo "distinct_published_at=$distinct_stamps"
} | tee "$OUT/summary.txt"

if [[ "$published" != "$N" || "$in_kafka" != "$N" ]]; then
  echo "WARNING: published=$published in_kafka=$in_kafka, want both $N" >&2
  exit 1
fi