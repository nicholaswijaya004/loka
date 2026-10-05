#!/usr/bin/env bash
# scripts/traces.sh
#
# Text views of the API's traces from Jaeger's v3 query API (the legacy /api/...
# endpoints return 404 on Jaeger 2.x). Needs curl and jq.
#
#   ./scripts/traces.sh when 1s           # POST /bookings traces >= 1s, grouped by second
#   ./scripts/traces.sh pick 25ms 35ms    # ids of up to 5 traces in a duration window
#   ./scripts/traces.sh show <trace-id>   # one trace's API spans as a timeline, plus the gap
#
# Searches cover the last LOOKBACK seconds (default 3600). Times are UTC.

set -euo pipefail

JAEGER="${JAEGER:-http://localhost:16686}"
LOOKBACK="${LOOKBACK:-3600}"
OP="${OP:-POST /bookings}"

search() { # duration_min, duration_max (may be empty), depth
  local min max
  min="$(jq -nr "now - $LOOKBACK | strftime(\"%Y-%m-%dT%H:%M:%SZ\")")"
  max="$(jq -nr 'now + 60 | strftime("%Y-%m-%dT%H:%M:%SZ")')"
  local args=(
    --data-urlencode "query.service_name=api"
    --data-urlencode "query.operation_name=$OP"
    --data-urlencode "query.start_time_min=$min"
    --data-urlencode "query.start_time_max=$max"
    --data-urlencode "query.duration_min=$1"
    --data-urlencode "query.search_depth=$3"
  )
  [[ -n "$2" ]] && args+=(--data-urlencode "query.duration_max=$2")
  # Jaeger answers an empty search with HTTP 404 and an error body, so no -f;
  # the jq below treats a missing .result as "no traces".
  curl -s -G "$JAEGER/api/v3/traces" "${args[@]}" ||
    { echo "cannot reach Jaeger at $JAEGER" >&2; exit 1; }
}

# Root spans of $OP, one object per trace: {id, start_s, ms}.
roots='[(.result.resourceSpans // [])[].scopeSpans[].spans[]
        | select(.name == $op and .kind == 2)
        | {id: .traceId,
           start_s: ((.startTimeUnixNano | tonumber) / 1e9),
           ms: (((.endTimeUnixNano | tonumber) - (.startTimeUnixNano | tonumber)) / 1e6)}]'

case "${1:-}" in
  when)
    search "${2:?min duration, e.g. 1s}" "" 1000 | jq -r --arg op "$OP" "$roots"'
      | if length == 0 then "no traces >= '"$2"'" else
        "\(length) traces >= '"$2"' (time UTC, count, slowest ms):",
        (group_by(.start_s | floor)[]
         | "  \(.[0].start_s | floor | strftime("%H:%M:%S"))  n=\(length)  max=\(map(.ms) | max | floor)")
        end'
    ;;
  pick)
    search "${2:?min}" "${3:?max}" 5 | jq -r --arg op "$OP" "$roots"'[] | "\(.id)  \(.ms | floor) ms"'
    ;;
  show)
    json="$(curl -s "$JAEGER/api/v3/traces/${2:?trace id}")" ||
      { echo "cannot reach Jaeger at $JAEGER" >&2; exit 1; }
    if ! jq -e '.result' >/dev/null 2>&1 <<<"$json"; then
      echo "trace $2 not found" >&2
      exit 1
    fi
    jq -r --arg op "$OP" <<<"$json" '
      [.result.resourceSpans[]
       | (.resource.attributes[] | select(.key == "service.name") | .value.stringValue) as $svc
       | .scopeSpans[].spans[] | . + {svc: $svc}] as $all
      | ($all | map(select(.name == $op and .kind == 2))[0]) as $root
      | ($root.startTimeUnixNano | tonumber) as $t0
      | ($root.endTimeUnixNano | tonumber) as $t1
      | def ms(a; b): ((b - a) / 1e6 * 10 | floor) / 10;
        def depth($id): if $id == $root.spanId then 0
                        else 1 + depth($all[] | select(.spanId == $id) | .parentSpanId) end;
        [$all[] | select(.svc == "api" and .spanId != $root.spanId)] as $api
      | "offset_ms  dur_ms  span        (trace \($root.traceId), total \(ms($t0; $t1)) ms)",
        ($api | sort_by(.startTimeUnixNano | tonumber)[]
         | "\(ms($t0; .startTimeUnixNano | tonumber) | tostring | .[0:9] | " " * (9 - length) + .)  \(ms(.startTimeUnixNano | tonumber; .endTimeUnixNano | tonumber) | tostring | " " * (6 - length) + .)  \(("  " * depth(.parentSpanId)) // "")\(.name)"),
        ([$api[] | select(.parentSpanId == $root.spanId)
          | (.endTimeUnixNano | tonumber) - (.startTimeUnixNano | tonumber)] | add // 0) as $covered
      | "gap (root time not inside any direct child span): \(((($t1 - $t0) - $covered) / 1e6 * 10 | floor) / 10) ms"'
    ;;
  *)
    sed -n '3,12p' "$0"
    exit 1
    ;;
esac
