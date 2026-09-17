#!/usr/bin/env bash
# scripts/bench.sh
#
# Resets the database, builds and starts the server, runs a k6 script,
# verifies the inventory invariant, and prints the server's retry count.
#
# Usage:
#   STRATEGY=serializable ./scripts/bench.sh scripts/k6/contention.js
#   UNIT_ID=44444444-4444-4444-4444-444444444444 STRATEGY=optimistic \
#     ./scripts/bench.sh scripts/k6/contention.js
#
# Environment variables are inherited by both the server and k6.

set -euo pipefail

# ---- 0. args & config ----
K6_SCRIPT="${1:?Usage: $0 <k6-script.js>}"
PORT="${PORT:-8080}"
UNIT_ID="${UNIT_ID:-22222222-2222-2222-2222-222222222222}"
HEALTHZ_URL="http://localhost:${PORT}/healthz"
MAX_WAIT_SECONDS=30
BIN=bin/api

SERVER_PID=""                              # must exist before the trap, because of set -u
LOG="$(mktemp -t loka-bench.XXXXXX)"

# Stop the server gracefully, then print the retry summary it logs on shutdown.
# Safe to call twice: the second call sees an empty SERVER_PID and returns.
stop_server() {
  [[ -z "$SERVER_PID" ]] && return 0
  if kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "Stopping server (PID $SERVER_PID)..."
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true   # non-zero exit must not trip set -e
  fi
  SERVER_PID=""
  grep '"msg":"retries"' "$LOG" || echo "(no retries line in server log: $LOG)"
}

# ---- cleanup: always stop the server, and show its log if something failed ----
cleanup() {
  local status=$?
  stop_server
  if [[ $status -ne 0 ]]; then
    echo "--- last 20 lines of server log ($LOG) ---" >&2
    tail -n 20 "$LOG" >&2 || true
  fi
}
trap cleanup EXIT

port_in_use() {
  lsof -ti tcp:"$PORT" -sTCP:LISTEN >/dev/null 2>&1
}

# ---- 1. make sure nothing else is listening on the port ----
echo "Checking for an existing process on port $PORT..."
existing_pids="$(lsof -ti tcp:"$PORT" -sTCP:LISTEN 2>/dev/null || true)"
if [[ -n "$existing_pids" ]]; then
  echo "Killing existing process(es): $(echo $existing_pids)"
  # Unquoted on purpose: lsof can return several PIDs, one per line.
  kill $existing_pids 2>/dev/null || true
  for _ in $(seq 1 20); do
    port_in_use || break
    sleep 0.5
  done
  if port_in_use; then
    echo "Port $PORT is still in use; refusing to benchmark an unknown server." >&2
    exit 1
  fi
fi

# ---- 2. build first, so a compile error fails before the slow reset ----
echo "Building server..."
go build -o "$BIN" ./cmd/api

# ---- 3. reset ----
echo "Running make reset..."
make reset

# ---- 4. start the server binary directly, so $! is the real server ----
echo "Starting server: $BIN (STRATEGY=${STRATEGY:-<unset>})"
"$BIN" >"$LOG" 2>&1 &
SERVER_PID=$!
echo "Server started with PID $SERVER_PID, logging to $LOG"

# ---- 5. wait for /healthz ----
echo "Waiting for $HEALTHZ_URL to respond..."
healthy=false
for i in $(seq 1 "$MAX_WAIT_SECONDS"); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "Server exited during startup." >&2
    exit 1
  fi
  if curl -sf "$HEALTHZ_URL" >/dev/null 2>&1; then
    echo "Server is healthy (took ${i}s)."
    healthy=true
    break
  fi
  sleep 1
done

if [[ "$healthy" != true ]]; then
  echo "Server did not become healthy within ${MAX_WAIT_SECONDS}s" >&2
  exit 1
fi

# Confirm which strategy is actually being measured.
grep '"msg":"booking strategy"' "$LOG" || true

# ---- 6. run the k6 script ----
echo "Running k6 script: $K6_SCRIPT"
k6 run "$K6_SCRIPT"

# ---- 7. check the invariant: available + seats booked == total ----
# Sums qty rather than counting rows, so a multi-seat booking counts correctly.
echo "Checking invariant for unit $UNIT_ID..."

read -r total available booked <<< "$(docker compose exec -T postgres psql -U loka -d loka -tA -F' ' -c \
  "SELECT u.total_units,
          u.available_units,
          COALESCE((SELECT sum(b.qty) FROM bookings b WHERE b.unit_id = u.unit_id), 0)
   FROM inventory_units u
   WHERE u.unit_id = '${UNIT_ID}';")"

if [[ -z "${total:-}" ]]; then
  echo "Unit $UNIT_ID not found." >&2
  exit 1
fi

actual=$((available + booked))
echo "Invariant: available ($available) + booked ($booked) = $actual (total_units = $total)"

if [[ "$actual" -ne "$total" ]]; then
  echo "INVARIANT VIOLATED: got $actual, expected $total" >&2
  exit 1
fi
echo "Invariant holds."

# ---- 8. stop explicitly, so the retry count prints with this run ----
stop_server
echo "Done."