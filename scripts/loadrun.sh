#!/usr/bin/env bash
# scripts/loadrun.sh
#
# One measured run of the steady-load test, from an identical starting state:
#   build -> make reset -> load seed -> start all five binaries -> warm-up
#   (discarded) -> CHECKPOINT + ANALYZE + reset pg_stat_statements ->
#   measured k6 run -> dump pg_stat_statements and pipeline state -> stop.
#
# Containers stay up afterwards, so Jaeger keeps this run's traces until the
# next reset.
#
# Usage:
#   LABEL=before RUN=1 ./scripts/loadrun.sh
#   LABEL=after  RUN=1 BUILD=0 ./scripts/loadrun.sh   # reuse bin/after
#   LABEL=diag   RUN=1 BUILD=0 BIN_DIR=bin/before MONITOR=1 ./scripts/loadrun.sh
#
# MONITOR=1 adds docker-stats.txt and probe.log: a pgbench client inside the
# Postgres container running SELECT 1 at 20/s over the local socket. It adds
# load of its own, so a monitored run is for diagnosis, never a measurement.
#
# Output: loadruns/<LABEL>-<RUN>/ (k6.txt, k6-summary.json,
# pg_stat_statements.txt, db-state.txt, one log per binary).

set -euo pipefail

LABEL="${LABEL:?set LABEL, e.g. LABEL=before}"
RUN="${RUN:?set RUN, e.g. RUN=1}"
BIN_DIR="${BIN_DIR:-bin/$LABEL}"
BUILD="${BUILD:-1}"
WARMUP="${WARMUP:-15s}"
DURATION="${DURATION:-2m}"
PAYMOCK_LATENCY_MS="${PAYMOCK_LATENCY_MS:-100}"
MONITOR="${MONITOR:-0}" # 1 = record docker stats + an in-VM SELECT 1 probe (diagnosis only)
OUT="loadruns/${LABEL}-${RUN}"
BINARIES=(paymock api relay payments notifier)

PIDS=()

psql_loka() {
  docker compose exec -T postgres psql -U loka -d loka -v ON_ERROR_STOP=1 "$@"
}

stop_all() {
  local pid
  for pid in "${PIDS[@]+"${PIDS[@]}"}"; do
    kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "${PIDS[@]+"${PIDS[@]}"}"; do
    wait "$pid" 2>/dev/null || true
  done
  PIDS=()
}
trap stop_all EXIT

wait_for() { # url, name
  local i
  for i in $(seq 1 30); do
    curl -sf "$1" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "$2 did not respond at $1 within 30s; see $OUT/$2.log" >&2
  exit 1
}

for port in 8080 8081; do
  if lsof -ti tcp:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "Port $port is in use; stop whatever is on it first (lsof -i :$port)." >&2
    exit 1
  fi
done

if [[ "$BUILD" != 1 ]]; then
  for b in "${BINARIES[@]}"; do
    if [[ ! -x "$BIN_DIR/$b" ]]; then
      echo "$BIN_DIR/$b not found: build first, or point BIN_DIR at existing binaries (e.g. BIN_DIR=bin/before)." >&2
      exit 1
    fi
  done
fi

if [[ -e "$OUT" ]]; then
  echo "$OUT already exists; pick another RUN or delete it." >&2
  exit 1
fi
mkdir -p "$OUT"

if [[ "$BUILD" == 1 ]]; then
  echo "==> Building into $BIN_DIR"
  for b in "${BINARIES[@]}"; do
    go build -o "$BIN_DIR/$b" "./cmd/$b"
  done
fi

echo "==> Resetting containers and database"
make reset
psql_loka -q < scripts/seed-load.sql

# Fail now, not after a 2-minute run, if the seed or the compose change is missing.
read -r preload has_ext units <<< "$(psql_loka -tA -F' ' -c \
  "SELECT current_setting('shared_preload_libraries') LIKE '%pg_stat_statements%',
          EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements'),
          (SELECT count(*) FROM inventory_units WHERE name LIKE 'Load Unit %')")"
if [[ "$preload" != t ]]; then
  echo "pg_stat_statements is not in shared_preload_libraries: check the postgres 'command:' in docker-compose.yml." >&2
  exit 1
fi
if [[ "$has_ext" != t || "$units" != 1000 ]]; then
  echo "Seed incomplete (extension=$has_ext, load units=$units, want t and 1000): check scripts/seed-load.sql." >&2
  exit 1
fi

echo "==> Starting binaries"
LATENCY_MS="$PAYMOCK_LATENCY_MS" "$BIN_DIR/paymock" >"$OUT/paymock.log" 2>&1 & PIDS+=($!)
wait_for http://localhost:8081/charges paymock
"$BIN_DIR/api" >"$OUT/api.log" 2>&1 & PIDS+=($!)
wait_for http://localhost:8080/healthz api
"$BIN_DIR/relay" >"$OUT/relay.log" 2>&1 & PIDS+=($!)
"$BIN_DIR/payments" >"$OUT/payments.log" 2>&1 & PIDS+=($!)
"$BIN_DIR/notifier" >"$OUT/notifier.log" 2>&1 & PIDS+=($!)
sleep 3 # let the consumers join their groups before load starts

echo "==> Warm-up ($WARMUP, discarded)"
# Exit 99 (a threshold crossed while cold) is tolerated; anything else, such as
# 107 when setup can't create bookings, means the stack is broken.
warmup_status=0
k6 run -q -e DURATION="$WARMUP" scripts/k6/load.js >"$OUT/warmup.txt" 2>&1 || warmup_status=$?
if [[ $warmup_status -ne 0 && $warmup_status -ne 99 ]]; then
  echo "Warm-up failed (k6 exit $warmup_status):" >&2
  tail -n 5 "$OUT/warmup.txt" >&2
  exit 1
fi

echo "==> Equalising Postgres state"
psql_loka -q -c 'CHECKPOINT' -c 'ANALYZE' -c 'SELECT pg_stat_statements_reset()' >/dev/null

seconds() { # 2m -> 120, 90s -> 90
  case "$1" in
    *m) echo $(( ${1%m} * 60 )) ;;
    *s) echo "${1%s}" ;;
    *)  echo "$1" ;;
  esac
}

if [[ "$MONITOR" == 1 ]]; then
  echo "==> Monitoring: docker stats + in-VM SELECT 1 probe"
  (
    while :; do
      printf '%s ' "$(date -u +%H:%M:%S)"
      docker stats --no-stream --format '{{.Name}}={{.CPUPerc}},{{.MemUsage}}' | tr '\n' ' '
      echo
    done
  ) >"$OUT/docker-stats.txt" 2>&1 &
  PIDS+=($!)
  docker compose exec -T postgres sh -c \
    "echo 'SELECT 1;' > /tmp/probe.sql && cd /tmp && rm -f probe.[0-9]* &&
     pgbench -U loka -n -f /tmp/probe.sql -R 20 -T $(( $(seconds "$DURATION") + 5 )) -l --log-prefix=probe loka" \
    >"$OUT/probe-summary.txt" 2>&1 &
  PROBE_PID=$!
fi

echo "==> Measured run ($DURATION)"
k6_status=0
k6 run -q -e DURATION="$DURATION" -e SUMMARY_FILE="$OUT/k6-summary.json" scripts/k6/load.js \
  2>&1 | tee "$OUT/k6.txt" || k6_status=$?

if [[ "$MONITOR" == 1 ]]; then
  wait "$PROBE_PID" || true
  docker compose exec -T postgres sh -c 'cat /tmp/probe.[0-9]*' >"$OUT/probe.log"
fi

psql_loka -P pager=off >"$OUT/pg_stat_statements.txt" <<'SQL'
SELECT calls,
       round(total_exec_time::numeric, 1)  AS total_ms,
       round(mean_exec_time::numeric, 3)   AS mean_ms,
       round(stddev_exec_time::numeric, 3) AS stddev_ms,
       round(max_exec_time::numeric, 2)    AS max_ms,
       rows,
       shared_blks_hit  AS hit,
       shared_blks_read AS read,
       left(regexp_replace(query, '\s+', ' ', 'g'), 140) AS query
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = 'loka')
ORDER BY total_exec_time DESC
LIMIT 25;
SQL

psql_loka -P pager=off >"$OUT/db-state.txt" <<'SQL'
SELECT booking_status, count(*) FROM bookings GROUP BY 1 ORDER BY 1;
SELECT count(*) FILTER (WHERE published_at IS NULL) AS outbox_unpublished,
       count(*) AS outbox_total
FROM outbox_events;
SQL

stop_all
echo "==> Done: $OUT (k6 exit $k6_status; non-zero means a threshold failed, check failed/dropped)"
