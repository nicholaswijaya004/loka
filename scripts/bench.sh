#!/usr/bin/env bash
# run_load_test.sh
#
# Usage:
#   ./run_load_test.sh <k6-script.js>
#
# Any env vars you set before calling this script are automatically
# passed through to the server, e.g.:
#   PORT=4000 NODE_ENV=staging ./run_load_test.sh scripts/load.js

set -euo pipefail

# ---- 0. args & config ----
K6_SCRIPT="${1:?Usage: $0 <k6-script.js>}"
PORT="${PORT:-8080}"
HEALTHZ_URL="http://localhost:${PORT}/healthz"
MAX_WAIT_SECONDS=30

if [[ -n "${SERVER_CMD:-}" ]]; then
  read -ra SERVER_CMD_ARR <<< "$SERVER_CMD"
else
  SERVER_CMD_ARR=(go run ./cmd/api)
fi

SERVER_PID=""

# ---- cleanup: always kill the server on exit, success or failure ----
cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "Stopping server (PID $SERVER_PID)..."
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ---- 1. kill any already-running server on this port ----
echo "Checking for an existing process on port $PORT..."
find_pid_on_port() {
  if command -v lsof >/dev/null 2>&1; then
    lsof -ti tcp:"$1" 2>/dev/null || true
  elif command -v ss >/dev/null 2>&1; then
    ss -ltnp "sport = :$1" 2>/dev/null | grep -oP 'pid=\K[0-9]+' || true
  elif command -v fuser >/dev/null 2>&1; then
    fuser "$1"/tcp 2>/dev/null | tr -d '[:space:]' || true
  fi
}

existing_pid="$(find_pid_on_port "$PORT")"
if [ -n "$existing_pid" ]; then
  echo "Killing existing process (PID $existing_pid)..."
  kill "$existing_pid" 2>/dev/null || true
  sleep 1
fi

# ---- 2. reset ----
echo "Running make reset..."
make reset

# ---- 3. start the server (inherits whatever env the caller passed) ----
echo "Starting server: ${SERVER_CMD_ARR[*]}"
"${SERVER_CMD_ARR[@]}" &
SERVER_PID=$!
echo "Server started with PID $SERVER_PID"

# ---- 4. wait for /healthz ----
echo "Waiting for $HEALTHZ_URL to respond..."
healthy=false
for i in $(seq 1 "$MAX_WAIT_SECONDS"); do
  if curl -sf "$HEALTHZ_URL" > /dev/null 2>&1; then
    echo "Server is healthy (took ${i}s)."
    healthy=true
    break
  fi
  sleep 1
done

if [ "$healthy" != true ]; then
  echo "Server did not become healthy within ${MAX_WAIT_SECONDS}s" >&2
  exit 1
fi

# ---- 5. run the k6 script ----
echo "Running k6 script: $K6_SCRIPT"
k6 run "$K6_SCRIPT"

# ---- 6. query the database for the invariant ----
# Invariant: for a given unit, available_units + currently-booked count
# must always equal the unit's fixed total capacity, no matter how many
# concurrent bookings raced for it.
echo "Checking invariant..."

UNIT_ID="${UNIT_ID:-22222222-2222-2222-2222-222222222222}"

read -r total actual <<< "$(docker compose exec -T postgres psql -U loka -d loka -tA -F' ' -c \
  "SELECT u.total_units, u.available_units + (SELECT count(*) FROM bookings WHERE unit_id = u.unit_id)
   FROM inventory_units u WHERE u.unit_id = '${UNIT_ID}';")"

echo "Invariant: available + booked = $actual (total_units = $total)"

if [[ "$actual" != "$total" ]]; then
  echo "INVARIANT VIOLATED: got $actual, expected $total" >&2
  exit 1
fi
echo "Invariant holds."

# ---- 7. server is killed automatically by the trap on exit ----
echo "Done."