#!/usr/bin/env bash
# scripts/loadrun.sh
#
# One measured run of the steady-load test, from an identical starting state:
#   build -> make reset -> load seed -> start all six binaries -> warm-up
#   (discarded) -> CHECKPOINT + ANALYZE + reset pg_stat_statements and the
#   Redis counters -> measured k6 run -> dump pg_stat_statements, the Redis
#   hit rate, the API write pool's waits, the outbox lag and pipeline state
#   -> stop.
#
# Containers stay up afterwards, so Jaeger keeps this run's traces until the
# next reset.
#
# Usage:
#   LABEL=before RUN=1 ./scripts/loadrun.sh
#   LABEL=after  RUN=1 BUILD=0 ./scripts/loadrun.sh   # reuse bin/after
#   LABEL=diag   RUN=1 BUILD=0 BIN_DIR=bin/before MONITOR=1 ./scripts/loadrun.sh
#   LABEL=nocache RUN=1 UNIT_CACHE_TTL=0 ./scripts/loadrun.sh   # cache off
#   LABEL=pool5   RUN=1 API_DB_MAX_CONNS=5 ./scripts/loadrun.sh  # API write pool of 5
#   LABEL=procs2  RUN=1 GOMAXPROCS_API=2 ./scripts/loadrun.sh    # the API on 2 threads
#
# MONITOR=1 adds docker-stats.txt and probe.log: a pgbench client inside the
# Postgres container running SELECT 1 at 20/s over the local socket. It adds
# load of its own, so a monitored run is for diagnosis, never a measurement.
#
# Output: loadruns/<LABEL>-<RUN>/ (settings.txt, k6.txt, k6-summary.json,
# pg_stat_statements.txt, redis-stats.txt, pool-before.json, pool-after.json,
# db-state.txt, one log per binary).

set -euo pipefail

LABEL="${LABEL:?set LABEL, e.g. LABEL=before}"
RUN="${RUN:?set RUN, e.g. RUN=1}"
BIN_DIR="${BIN_DIR:-bin/$LABEL}"
BUILD="${BUILD:-1}"
WARMUP="${WARMUP:-15s}"
DURATION="${DURATION:-2m}"
PAYMOCK_LATENCY_MS="${PAYMOCK_LATENCY_MS:-100}"
MONITOR="${MONITOR:-0}" # 1 = record docker stats + an in-VM SELECT 1 probe (diagnosis only)
UNIT_CACHE_TTL="${UNIT_CACHE_TTL:-30s}" # 0 = unit cache off, same binaries
UNIT_CACHE_TIMEOUT="${UNIT_CACHE_TIMEOUT:-50ms}" # Redis read/write timeout in the API
API_DB_MAX_CONNS="${API_DB_MAX_CONNS:-50}"       # API write pool size
GOMAXPROCS_API="${GOMAXPROCS_API:-}"             # threads running Go code in the API; empty = all CPUs
PROM_URL="${PROM_URL:-http://localhost:9090}"   # skipped if nothing answers there
OUT="loadruns/${LABEL}-${RUN}"
BINARIES=(paymock api relay payments notifier cacheinvalidator)

PIDS=()

psql_loka() {
  docker compose exec -T postgres psql -U loka -d loka -v ON_ERROR_STOP=1 "$@"
}

# macOS only: the share of normal CPU speed allowed right now (pmset -g therm).
# 100 when nothing is recorded, n/a where pmset doesn't exist.
cpu_speed_limit() {
  if ! command -v pmset >/dev/null 2>&1; then
    echo "n/a"
    return
  fi
  pmset -g therm 2>/dev/null | awk -F= '
    /CPU_Speed_Limit/ { gsub(/[ \t]/, "", $2); v = $2 }
    END { print (v == "" ? "100" : v) }'
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
speed_start=$(cpu_speed_limit)
mkdir -p "$OUT"
# What this run was, next to its results.
{
  echo "commit=$(git rev-parse --short HEAD)$(git diff --quiet HEAD -- . ':!loadruns' || echo '+dirty')"
  echo "bin_dir=$BIN_DIR"
  echo "unit_cache_ttl=$UNIT_CACHE_TTL unit_cache_timeout=$UNIT_CACHE_TIMEOUT"
  echo "rate=${RATE:-50} get_rate=${GET_RATE:-25} unit_rate=${UNIT_RATE:-200}"
  echo "api_db_max_conns=$API_DB_MAX_CONNS gomaxprocs_api=${GOMAXPROCS_API:-default}"
  echo "warmup=$WARMUP duration=$DURATION paymock_latency_ms=$PAYMOCK_LATENCY_MS"
  echo "cpu_speed_limit_start=$speed_start"
} >"$OUT/settings.txt"

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
# GOMAXPROCS is only set when asked for: Go treats an empty value as unset,
# but leaving it out says so plainly.
api_env=(UNIT_CACHE_TTL="$UNIT_CACHE_TTL" UNIT_CACHE_TIMEOUT="$UNIT_CACHE_TIMEOUT" API_DB_MAX_CONNS="$API_DB_MAX_CONNS")
if [[ -n "$GOMAXPROCS_API" ]]; then
  api_env+=(GOMAXPROCS="$GOMAXPROCS_API")
fi
env "${api_env[@]}" "$BIN_DIR/api" >"$OUT/api.log" 2>&1 & PIDS+=($!)
wait_for http://localhost:8080/healthz api
"$BIN_DIR/relay" >"$OUT/relay.log" 2>&1 & PIDS+=($!)
"$BIN_DIR/payments" >"$OUT/payments.log" 2>&1 & PIDS+=($!)
"$BIN_DIR/notifier" >"$OUT/notifier.log" 2>&1 & PIDS+=($!)
"$BIN_DIR/cacheinvalidator" >"$OUT/cacheinvalidator.log" 2>&1 & PIDS+=($!)
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
# Count only the measured run's cache reads; the warm-up's entries stay cached.
docker compose exec -T redis redis-cli CONFIG RESETSTAT >/dev/null
api_log_start=$(wc -l <"$OUT/api.log") # to count only the measured run's fallbacks

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

# The pool's counters are cumulative since the API started; the difference
# between these two snapshots is the measured window alone.
curl -sf localhost:8080/debug/pool >"$OUT/pool-before.json"
measure_start=$(psql_loka -tA -c "SELECT clock_timestamp()")
measure_start_epoch=$(date +%s)

echo "==> Measured run ($DURATION)"
k6_status=0
k6 run -q -e DURATION="$DURATION" -e SUMMARY_FILE="$OUT/k6-summary.json" scripts/k6/load.js \
  2>&1 | tee "$OUT/k6.txt" || k6_status=$?
curl -sf localhost:8080/debug/pool >"$OUT/pool-after.json"
measure_end_epoch=$(date +%s)

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

# keyspace_hits/misses count every key read (GET, but also TTL or EXISTS), not
# SET or DEL. During a run only the API reads, so don't poke Redis by hand.
{
  docker compose exec -T redis redis-cli INFO stats
  docker compose exec -T redis redis-cli INFO keyspace
} | tr -d '\r' >"$OUT/redis-stats.txt"
hit_rate=$(awk -F: '
  $1 == "keyspace_hits"   { h = $2 }
  $1 == "keyspace_misses" { m = $2 }
  END {
    if (h + m == 0) print "n/a (no cache reads)"
    else printf "%.1f%% (%d hits, %d misses)", 100 * h / (h + m), h, m
  }' "$OUT/redis-stats.txt")
# Reads where Redis failed (usually a timeout) and Postgres answered instead.
fallbacks=$(tail -n +"$((api_log_start + 1))" "$OUT/api.log" | grep -c '"msg":"unit cache unavailable' || true)
echo "unit cache hit rate: $hit_rate; reads that fell back to postgres: $fallbacks" | tee -a "$OUT/k6.txt"

# Write pool in the measured window. "No idle connection" covers both waiting
# for a busy one and opening a new one; new_conns separates the two.
jq -rn --slurpfile a "$OUT/pool-before.json" --slurpfile b "$OUT/pool-after.json" '
  ($a[0].write) as $x | ($b[0].write) as $y |
  ($y.acquire_count - $x.acquire_count)                          as $acq   |
  ($y.empty_acquire_count - $x.empty_acquire_count)              as $empty |
  (($y.new_conns_count // 0) - ($x.new_conns_count // 0))        as $new   |
  ($y.empty_acquire_wait_ms - $x.empty_acquire_wait_ms)          as $wait  |
  ($y.canceled_acquire_count - $x.canceled_acquire_count)        as $cancel |
  "write pool \($y.max_conns): \($acq) acquires, \($empty) found no idle connection " +
  "(\(if $acq > 0 then ($empty * 1000 / $acq | floor) / 10 else 0 end)%, \($new) of them opened a new one), " +
  "\($wait | floor) ms waiting in total, \($cancel) canceled"' | tee -a "$OUT/k6.txt"

# Outbox lag for the events created in the measured window (a window owns what
# it created). Two seconds first, so the relay publishes the last second's.
sleep 2
read -r lag_n lag_p50 lag_p95 lag_p99 lag_max lag_unpub <<< "$(psql_loka -tA -F' ' -c "
  SELECT count(*) FILTER (WHERE published_at IS NOT NULL),
         round((percentile_cont(0.50) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - created_at) * 1000))::numeric, 1),
         round((percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - created_at) * 1000))::numeric, 1),
         round((percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - created_at) * 1000))::numeric, 1),
         round(max(extract(epoch FROM published_at - created_at) * 1000)::numeric, 1),
         count(*) FILTER (WHERE published_at IS NULL)
  FROM outbox_events
  WHERE created_at >= '$measure_start'")"
echo "outbox lag (ms): p50 $lag_p50 p95 $lag_p95 p99 $lag_p99 max $lag_max over $lag_n events ($lag_unpub unpublished)" | tee -a "$OUT/k6.txt"

# The same window as Prometheus saw it, next to k6's table: server-side
# durations (from the handler's start to its end, no network, no client
# queueing) estimated from histogram buckets, against k6's exact client-side
# ones. The window also holds k6's setup requests, which k6's table leaves out.
if curl -sf "$PROM_URL/-/ready" >/dev/null 2>&1; then
  sleep 6 # one more scrape after the run, so its last seconds are in
  at=$((measure_end_epoch + 5))
  window="$((at - measure_start_epoch))s"
  promq() {
    curl -sfG "$PROM_URL/api/v1/query" --data-urlencode "query=$1" --data-urlencode "time=$at" |
      jq -r '.data.result[0].value[1] // "NaN"' || echo NaN
  }
  {
    printf '\n%-20s%10s%10s%10s%10s\n' 'prometheus (ms)' p50 p95 p99 count
    for r in 'POST /bookings' 'GET /bookings/{id}' 'GET /units/{id}'; do
      sel="job=\"api\", http_request_method=\"${r%% *}\", http_route=\"${r#* }\""
      vals=()
      for q in 0.5 0.95 0.99; do
        vals+=("$(promq "histogram_quantile($q, sum by (le) (increase(http_server_request_duration_seconds_bucket{$sel}[$window])))")")
      done
      n=$(promq "sum(increase(http_server_request_duration_seconds_count{$sel}[$window]))")
      # No samples comes back as NaN; print "-", not a misleading 0.
      awk -v r="$r" -v a="${vals[0]}" -v b="${vals[1]}" -v c="${vals[2]}" -v n="$n" '
        function ms(x) { return x == "NaN" ? "-" : sprintf("%.2f", x * 1000) }
        BEGIN { printf "%-20s%10s%10s%10s%10s\n", r, ms(a), ms(b), ms(c), (n == "NaN" ? "-" : sprintf("%.0f", n)) }'
    done
  } | tee -a "$OUT/k6.txt"
else
  echo "prometheus: not reachable at $PROM_URL, skipped (make monitoring-up)" | tee -a "$OUT/k6.txt"
fi

speed_end=$(cpu_speed_limit)
echo "cpu_speed_limit_end=$speed_end" >>"$OUT/settings.txt"
echo "cpu speed limit: ${speed_start}% at start, ${speed_end}% at end (below 100 = throttled)" | tee -a "$OUT/k6.txt"

psql_loka -P pager=off >"$OUT/db-state.txt" <<'SQL'
SELECT booking_status, count(*) FROM bookings GROUP BY 1 ORDER BY 1;
SELECT count(*) FILTER (WHERE published_at IS NULL) AS outbox_unpublished,
       count(*) AS outbox_total
FROM outbox_events;
SQL

stop_all
echo "==> Done: $OUT (k6 exit $k6_status; non-zero means a threshold failed, check failed/dropped)"