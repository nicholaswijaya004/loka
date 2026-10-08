#!/usr/bin/env bash
# scripts/replicas.sh
#
# A k6 script against N API containers behind nginx, then the inventory
# invariant and how the requests were spread:
#
#   make reset -> build the API image -> start N replicas -> start nginx on
#   :8088 -> k6 -> invariant -> requests per replica -> stop the replicas
#   (each logs its retry count on the way out).
#
# Usage:
#   STRATEGY=single       ./scripts/replicas.sh scripts/k6/contention.js
#   STRATEGY=serializable ./scripts/replicas.sh scripts/k6/contention.js
#   UNSAFE_DECREMENT=1    ./scripts/replicas.sh scripts/k6/contention.js   # the control: must oversell
#   STRATEGY=single       ./scripts/replicas.sh scripts/k6/retry.js        # one key, 500 times
#
# Output: loadruns/replicas-<label>-<RUN>/ (k6.txt, lb.log, api.log, result.txt)

set -euo pipefail

K6_SCRIPT="${1:?usage: $0 <k6-script.js>}"
REPLICAS="${REPLICAS:-3}"
RUN="${RUN:-1}"
UNIT_ID="${UNIT_ID:-22222222-2222-2222-2222-222222222222}"
export STRATEGY="${STRATEGY:-single}"
export UNSAFE_DECREMENT="${UNSAFE_DECREMENT:-0}"
export API_DB_MAX_CONNS="${API_DB_MAX_CONNS:-20}"
LB_URL="http://localhost:8088"

label="$(basename "$K6_SCRIPT" .js)-${STRATEGY}"
[[ "$UNSAFE_DECREMENT" == 1 ]] && label="$(basename "$K6_SCRIPT" .js)-unsafe"
OUT="loadruns/replicas-${label}-${RUN}"
if [[ -e "$OUT" ]]; then
  echo "$OUT already exists; pick another RUN or delete it." >&2
  exit 1
fi
mkdir -p "$OUT"

compose() { docker compose -f docker-compose.yml -f docker-compose.replicas.yml "$@"; }
psql_loka() { docker compose exec -T postgres psql -U loka -d loka -v ON_ERROR_STOP=1 "$@"; }

echo "==> Removing the previous run's replicas and load balancer"
# `make reset` only knows docker-compose.yml: containers from the overlay would
# be left attached to the network it tries to remove.
compose rm -sf api lb >/dev/null 2>&1 || true

echo "==> Resetting containers and database"
if ! make reset >"$OUT/reset.log" 2>&1; then
  tail -n 5 "$OUT/reset.log" >&2
  exit 1
fi

echo "==> Building the API image"
compose build api >"$OUT/build.log" 2>&1 || { tail -n 20 "$OUT/build.log" >&2; exit 1; }

echo "==> Starting $REPLICAS replicas (STRATEGY=$STRATEGY UNSAFE_DECREMENT=$UNSAFE_DECREMENT API_DB_MAX_CONNS=$API_DB_MAX_CONNS)"
compose up -d --no-deps --scale api="$REPLICAS" api >/dev/null
for i in $(seq 1 60); do
  healthy=0
  for c in $(compose ps -q api); do
    [[ "$(docker inspect -f '{{.State.Health.Status}}' "$c")" == healthy ]] && healthy=$((healthy + 1))
  done
  [[ $healthy -eq $REPLICAS ]] && break
  if [[ $i -eq 60 ]]; then
    echo "only $healthy of $REPLICAS replicas healthy after 60 s" >&2
    compose logs --no-color api | tail -n 20 >&2
    exit 1
  fi
  sleep 1
done

# Started after the replicas, so its one DNS lookup sees all of them.
compose up -d --no-deps lb >/dev/null
for i in $(seq 1 30); do
  curl -sf "$LB_URL/healthz" >/dev/null && break
  [[ $i -eq 30 ]] && { echo "nginx not answering on $LB_URL" >&2; compose logs --no-color lb | tail -n 20 >&2; exit 1; }
  sleep 1
done
# Count only the test's requests: skip the lines nginx has logged so far.
lb_skip=$(compose logs --no-color lb | wc -l)

echo "==> k6: $K6_SCRIPT via $LB_URL"
k6_status=0
BASE_URL="$LB_URL" k6 run "$K6_SCRIPT" 2>&1 | tee "$OUT/k6.txt" || k6_status=$?

# Requests per replica, from nginx's "<upstream> <status>" lines, with each
# upstream address named after its container.
compose logs --no-color lb | tail -n +"$((lb_skip + 1))" >"$OUT/lb.log"
for c in $(compose ps -q api); do
  echo "$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$c"):8080 $(docker inspect -f '{{.Name}}' "$c" | tr -d /)"
done >"$OUT/replicas.txt"
{
  echo "requests per replica and status:"
  awk 'NR == FNR { name[$1] = $2; next }
       $(NF-1) ~ /:8080$/ { n[(($(NF-1) in name) ? name[$(NF-1)] : $(NF-1)) " " $NF]++ }
       END { for (k in n) { split(k, a, " "); printf "  %-22s %-6s %5d\n", a[1], a[2], n[k] } }' \
    "$OUT/replicas.txt" "$OUT/lb.log" | sort
} | tee "$OUT/result.txt"

# k6's own counts. A request that never got an answer has status 0, which
# passes the scripts' "not a server error" check, so look at the counter.
grep -E '^ *(bookings_created|bookings_sold_out|bookings_replayed|duplicate_rejected|bookings_error)\.' "$OUT/k6.txt" |
  awk '{ print "  " $1, $2 }' | tee -a "$OUT/result.txt"
errored=$(awk '$1 ~ /^bookings_error\./ { print $2 }' "$OUT/k6.txt")
if [[ -n "$errored" && "$errored" != 0 ]]; then
  echo "INVALID RUN: $errored requests got no answer or a 5xx; see $OUT/k6.txt" | tee -a "$OUT/result.txt"
fi

# The invariant: seats left + seats booked = seats in total.
read -r total available booked bookings <<<"$(psql_loka -tA -F' ' -c "
  SELECT u.total_units, u.available_units,
         coalesce((SELECT sum(qty) FROM bookings b WHERE b.unit_id = u.unit_id), 0),
         (SELECT count(*) FROM bookings b WHERE b.unit_id = u.unit_id)
  FROM inventory_units u WHERE u.unit_id = '$UNIT_ID'")"
actual=$((available + booked))
{
  echo "bookings: $bookings; seats booked: $booked; available: $available; total: $total"
  if [[ "$actual" -eq "$total" ]]; then
    echo "invariant holds: $available + $booked = $total"
  else
    echo "INVARIANT VIOLATED: $available + $booked = $actual, total is $total (oversold by $((booked - total + available)))"
  fi
} | tee -a "$OUT/result.txt"

# Each replica logs its optimistic/serializable retry count when it stops.
compose stop api >/dev/null
compose logs --no-color api >"$OUT/api.log"
retries=$(grep -o '"msg":"retries".*"total":[0-9]*' "$OUT/api.log" | grep -o '[0-9]*$' | awk '{ s += $1; n++ } END { printf "%d (from %d replicas)", s, n }')
echo "retries: $retries" | tee -a "$OUT/result.txt"
# Remove them, so a later `make reset` (loadrun.sh, steprun.sh) finds no
# containers it doesn't know on its network.
compose rm -sf api lb >/dev/null

echo "==> Done: $OUT (k6 exit $k6_status)"
