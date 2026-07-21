#!/usr/bin/env bash
# Drives the full-stack E2E: starts the host container, waits for the watcher's
# first poll, crosses the stock threshold, and asserts exactly one refill ticket
# lands in Postgres. Shared by both CI jobs via MODE.
#
# Assumes the workflow already:
#   - has Postgres up + seeded (seed.sql), reachable at $DATABASE_URL
#   - has datalevin-server (:8898) and the testharness MCP (:8765) running
#   - assembled $WORK with config.yaml + bin/opentalon-agents (+ agents.db for
#     the deterministic mode's pre-seeded agent)
#
# Env contract:
#   MODE           deterministic | real-llm
#   WORK           dir bind-mounted into the container as /work
#   HOST_IMAGE     published host image (ghcr.io/opentalon/opentalon:...)
#   DATABASE_URL   postgres DSN for psql assertions
#   MCP_LOG        path to the testharness MCP server's log file
#   DATALEVIN_LOG  path to datalevin-server's log file
#   BARCODE        watched barcode (default ABC-123)
#   PROMPT         real-llm only: the authoring prompt piped to console stdin
set -euo pipefail

BARCODE="${BARCODE:-ABC-123}"
CONTAINER=opentalon-host
FIFO=""

dump_logs() {
  echo "::group::host container logs"
  docker logs "$CONTAINER" 2>&1 | tail -300 || true
  echo "::endgroup::"
  echo "::group::datalevin log"; tail -100 "$DATALEVIN_LOG" 2>/dev/null || true; echo "::endgroup::"
  echo "::group::mcp log"; tail -100 "$MCP_LOG" 2>/dev/null || true; echo "::endgroup::"
  echo "::group::opentalon.log (in container)"
  docker exec "$CONTAINER" tail -200 /home/opentalon/.opentalon/opentalon.log 2>/dev/null || true
  echo "::endgroup::"
}
cleanup() {
  [ -n "$FIFO" ] && exec 3>&- 2>/dev/null || true
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  [ -n "$FIFO" ] && rm -f "$FIFO" || true
}
trap 'rc=$?; if [ $rc -ne 0 ]; then echo "FAIL (rc=$rc)"; dump_logs; fi; cleanup; exit $rc' EXIT

psql_q() { psql "$DATABASE_URL" -tAc "$1"; }

start_host() {
  FIFO="$(mktemp -u)"; mkfifo "$FIFO"
  # Open read-write first so the container's `< FIFO` doesn't block waiting for a
  # writer, and hold fd 3 open so the console channel's stdin stays connected —
  # on EOF the console reads Ctrl+D and the host shuts down. Both modes keep it
  # open; only real-llm writes the authoring prompt.
  #
  # Run backgrounded, NOT `-d`: detached mode drops the container's stdin the
  # moment the docker CLI returns, which is the EOF we must avoid. Keeping the
  # CLI alive in the background streams the FIFO into the container for its
  # whole lifetime.
  exec 3<>"$FIFO"
  docker run -i --name "$CONTAINER" --network host \
    -v "$WORK":/work -w /work "$HOST_IMAGE" -config /work/config.yaml < "$FIFO" >/dev/null 2>&1 &
  for _ in $(seq 1 30); do
    docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$" && break
    sleep 1
  done
  if [ "$MODE" = "real-llm" ]; then
    echo "waiting 30s for host + plugins to build before authoring"
    sleep 30
    printf '%s\n' "$PROMPT" >&3
  fi
}

# wait_for grep-pattern in file, up to N seconds.
wait_for_log() {
  local pat="$1" timeout="$2" waited=0
  until grep -q "$pat" "$MCP_LOG" 2>/dev/null; do
    if ! docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
      echo "host container exited early"; return 1
    fi
    sleep 3; waited=$((waited+3))
    if [ "$waited" -ge "$timeout" ]; then
      echo "timeout after ${timeout}s waiting for: $pat"; return 1
    fi
    [ $((waited % 30)) -eq 0 ] && echo "  ...still waiting (${waited}s) for: $pat"
  done
}

echo "== starting host ($MODE) =="
start_host

# First poll must observe the pre-drop stock (15) so a snapshot is established;
# only then does a later drop read as a downward crossing. The host clones and
# builds console/talon/mcp plugins on first run, so allow a generous window
# (real-llm also needs the model to author the agent first).
echo "== waiting for first get_item on $BARCODE =="
wait_for_log "get_item $BARCODE" 360

echo "== crossing threshold: stock -> 8 =="
psql_q "UPDATE items SET current_stock = 8 WHERE barcode = '$BARCODE'"

echo "== waiting for a ticket to be created =="
waited=0
until [ "$(psql_q 'SELECT count(*) FROM tickets')" -ge 1 ]; do
  sleep 3; waited=$((waited+3))
  if [ "$waited" -ge 120 ]; then echo "timeout: no ticket after 120s"; exit 1; fi
done

count="$(psql_q 'SELECT count(*) FROM tickets')"
qty="$(psql_q 'SELECT qty FROM tickets ORDER BY id LIMIT 1')"
bc="$(psql_q 'SELECT barcode FROM tickets ORDER BY id LIMIT 1')"
echo "tickets=$count barcode=$bc qty=$qty"

fail=0
[ "$count" = "1" ]   || { echo "FAIL: expected exactly 1 ticket, got $count"; fail=1; }
[ "$bc" = "$BARCODE" ] || { echo "FAIL: expected barcode $BARCODE, got $bc"; fail=1; }
[ "$qty" = "50" ]    || { echo "FAIL: expected qty 50, got $qty"; fail=1; }
[ "$fail" = "0" ] || exit 1
echo "PASS: one refill ticket for $BARCODE qty 50"
