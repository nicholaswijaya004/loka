#!/usr/bin/env bash
# scripts/steprun.sh
#
# The breaking point: one loadrun.sh run per load step, each from the same
# reset state, stepping up until a step breaks. The mix stays Day 26's
# (POST : GET booking : GET unit = 2 : 1 : 4); step m is m x 25 POST/s.
#
#   STEPS="1 2 4 6 8 10 12" LABEL=step RUN=1 ./scripts/steprun.sh
#
# A step is broken when any of these holds (decided before the runs):
#   POST p99 (k6) > 1 s, failed requests > 1 %, dropped iterations > 1 %.
# The script stops after the first broken step; the rest would only break
# harder and heat the Mac.
#
# Output: one loadruns/<LABEL>-x<m>-<RUN>/ per step (loadrun.sh's files) and
# loadruns/<LABEL>-<RUN>-summary.txt with one row per step.

set -euo pipefail

LABEL="${LABEL:-step}"
RUN="${RUN:?set RUN, e.g. RUN=1}"
STEPS="${STEPS:-1 2 4 6 8 10 12}"
DURATION="${DURATION:-60s}"
P99_LIMIT_MS="${P99_LIMIT_MS:-1000}"
FAIL_LIMIT_PCT="${FAIL_LIMIT_PCT:-1}"
REQUIRE_COOL="${REQUIRE_COOL:-1}" # 1 = refuse to start a step while the CPU is throttled
LOADRUN="${LOADRUN:-./scripts/loadrun.sh}"
BIN_DIR="bin/$LABEL"
SUMMARY="loadruns/${LABEL}-${RUN}-summary.txt"

cpu_speed_limit() {
  local v
  v=$(pmset -g therm 2>/dev/null | awk -F'= *' '/CPU_Speed_Limit/ { print $2 }')
  echo "${v:-100}" # no line means no limit in force
}

if [[ -e "$SUMMARY" ]]; then
  echo "$SUMMARY already exists; pick another RUN." >&2
  exit 1
fi
mkdir -p loadruns
printf '%-7s %8s %8s %9s %9s %9s %7s %7s %10s %9s %8s %5s  %s\n' \
  step offered achieved post_p50 post_p99 unit_p99 fail% drop% pool_wait lag_p99 pending cpu verdict | tee "$SUMMARY"

build=1
for m in $STEPS; do
  rate=$((25 * m)) get_rate=$((25 * m / 2)) unit_rate=$((50 * m))
  offered=$((rate + get_rate + unit_rate))
  dir="loadruns/${LABEL}-x${m}-${RUN}"

  speed=$(cpu_speed_limit)
  if [[ "$REQUIRE_COOL" == 1 && "$speed" -lt 100 ]]; then
    echo "CPU_Speed_Limit is $speed (throttled) before step x$m; stopping. Let the Mac cool, or REQUIRE_COOL=0." | tee -a "$SUMMARY" >&2
    exit 1
  fi

  echo "==> Step x$m: $offered req/s offered (POST $rate, GET booking $get_rate, GET unit $unit_rate)"
  status=0
  RATE=$rate GET_RATE=$get_rate UNIT_RATE=$unit_rate DURATION="$DURATION" \
    LABEL="${LABEL}-x${m}" RUN="$RUN" BIN_DIR="$BIN_DIR" BUILD=$build \
    "$LOADRUN" >"loadruns/${LABEL}-x${m}-${RUN}.log" 2>&1 || status=$?
  build=0
  if [[ $status -ne 0 || ! -f "$dir/k6-summary.json" ]]; then
    echo "x$m: loadrun.sh failed (exit $status); see loadruns/${LABEL}-x${m}-${RUN}.log" | tee -a "$SUMMARY" >&2
    tail -n 5 "loadruns/${LABEL}-x${m}-${RUN}.log" >&2
    exit 1
  fi

  # k6's numbers: exact, client-side. "passes" of a Rate metric counts the
  # true values, i.e. the failed requests.
  read -r post_p50 post_p99 unit_p99 reqs failed iters dropped achieved <<<"$(jq -r '
    .metrics as $m
    | def v(name; k): ($m[name].values[k] // 0);
    [ v("http_req_duration{name:POST /bookings}"; "med"),
      v("http_req_duration{name:POST /bookings}"; "p(99)"),
      v("http_req_duration{name:GET /units/{id}}"; "p(99)"),
      v("http_reqs"; "count"),
      (v("http_req_failed{name:POST /bookings}"; "passes")
       + v("http_req_failed{name:GET /bookings/{id}}"; "passes")
       + v("http_req_failed{name:GET /units/{id}}"; "passes")),
      v("iterations"; "count"),
      v("dropped_iterations"; "count"),
      v("http_reqs"; "rate") ]
    | map(tostring) | join(" ")' "$dir/k6-summary.json")"

  # loadrun.sh's own lines: write-pool waiting (ms in total), outbox lag p99,
  # and bookings still pending payment when the run ended.
  pool_wait=$(sed -nE 's/.* ([0-9]+) ms waiting in total.*/\1/p' "$dir/k6.txt" | tail -1)
  lag_p99=$(sed -nE 's/^outbox lag \(ms\):.* p99 ([0-9.]+) .*/\1/p' "$dir/k6.txt" | tail -1)
  pending=$(awk '$1 == "pending" || $1 == "payment_pending" { s += $3 } END { print s + 0 }' "$dir/db-state.txt")

  verdict=$(awk -v p99="$post_p99" -v reqs="$reqs" -v failed="$failed" -v iters="$iters" -v dropped="$dropped" \
    -v plim="$P99_LIMIT_MS" -v flim="$FAIL_LIMIT_PCT" 'BEGIN {
      fpct = reqs > 0 ? 100 * failed / reqs : 0
      dpct = (iters + dropped) > 0 ? 100 * dropped / (iters + dropped) : 0
      why = ""
      if (p99 > plim) why = why "p99>" plim "ms "
      if (fpct > flim) why = why "fail>" flim "% "
      if (dpct > flim) why = why "drop>" flim "% "
      printf "%.2f %.2f %s", fpct, dpct, (why == "" ? "ok" : "BROKEN: " why) }')
  read -r fail_pct drop_pct verdict_text <<<"$verdict"

  printf '%-7s %8s %8.0f %9.1f %9.1f %9.1f %7s %7s %10s %9s %8s %5s  %s\n' \
    "x$m" "$offered" "$achieved" "$post_p50" "$post_p99" "$unit_p99" "$fail_pct" "$drop_pct" \
    "${pool_wait:--}" "${lag_p99:--}" "$pending" "$speed" "$verdict_text" | tee -a "$SUMMARY"

  if [[ "$verdict_text" != ok ]]; then
    echo "==> x$m broke; stopping. Per-step files: loadruns/${LABEL}-x*-${RUN}/" | tee -a "$SUMMARY"
    exit 0
  fi
done
echo "==> No step broke up to x${m}; raise STEPS to find the limit." | tee -a "$SUMMARY"
