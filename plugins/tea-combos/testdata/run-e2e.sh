#!/usr/bin/env bash
# End-to-end proof for tea-combos against a real CPA binary and the mock upstream.
#
# Usage: run-e2e.sh [workdir] [cpa-binary] [tea-combos.so]
#
# The script builds nothing. Build the plugin first (scripts/build.sh tea-combos) and pass the
# .so, or let it default to plugins/tea-combos/tea-combos.so.
#
# It starts the mock upstream and CPA from a flat plugin directory with a scratch config, then
# proves, from the mock's own request log:
#   (a) round-robin distribution with no session identity (30 requests, 3 members)
#   (b) session stickiness for two independent X-Session-Id values
#   (c) affinity disabled with session headers still rotates
#   (d) rebind on a retryable failure and chain stop on a client error
#   (e) bindings survive a CPA restart with the same state file
#   (f) which session-identity rungs fire on the real binary
#
# Ports are in the 183xx test range. Only the PIDs started here are stopped.
set -uo pipefail

WORKDIR=${1:-/var/tmp/tea-combos-e2e}
CPA_BIN=${2:-/root/reference/CLIProxyAPI/cli-proxy-api}
HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
PLUGIN_SO=${3:-$HERE/../tea-combos.so}
MOCK_PORT=${MOCK_PORT:-18361}
CPA_PORT=${CPA_PORT:-18362}
MKEY=$(python3 -c 'import secrets; print(secrets.token_hex(16))')
CKEY=$(python3 -c 'import secrets; print(secrets.token_hex(16))')
BASE="http://127.0.0.1:$CPA_PORT"

rm -rf "$WORKDIR"
mkdir -p "$WORKDIR/plugins"
cp "$PLUGIN_SO" "$WORKDIR/plugins/tea-combos.so" || { echo "missing plugin $PLUGIN_SO" >&2; exit 1; }
LOG="$WORKDIR/mock.log"
: > "$LOG"

cat > "$WORKDIR/config.yaml" <<EOF
server:
  host: "127.0.0.1"
  port: $CPA_PORT
access:
  api-keys:
    - "$CKEY"
management:
  allow-remote: true
  secret-key: "$MKEY"
observability:
  logs:
    debug: true
  usage:
    usage-statistics-enabled: true
openai-compatibility:
  - name: "mock"
    base-url: "http://127.0.0.1:$MOCK_PORT/v1"
    request-retry: 0
    keys:
      - api-key: "mock-key"
    models:
      - name: "rr-a"
        alias: "rr-a"
      - name: "rr-b"
        alias: "rr-b"
      - name: "rr-c"
        alias: "rr-c"
      - name: "bad400"
        alias: "bad400"
      - name: "bad503"
        alias: "bad503"
      - name: "stream-good"
        alias: "stream-good"
      - name: "stream-bad"
        alias: "stream-bad"
      - name: "stream-cut"
        alias: "stream-cut"
plugins:
  enabled: true
  dir: "$WORKDIR/plugins"
  configs:
    tea-combos:
      enabled: true
      statePath: "$WORKDIR/state.json"
      combos:
        rr3:
          strategy: round-robin
          members:
            - model: rr-a
            - model: rr-b
            - model: rr-c
        sticky3:
          strategy: round-robin
          members:
            - model: rr-a
            - model: rr-b
            - model: rr-c
        noaff:
          strategy: round-robin
          sessionAffinity: false
          members:
            - model: rr-a
            - model: rr-b
            - model: rr-c
        rebind:
          strategy: round-robin
          members:
            - model: rr-a
            - model: rr-b
            - model: rr-c
        stopchain:
          strategy: failover
          members:
            - model: rr-c
            - model: bad400
EOF

MOCK_PID=""
CPA_PID=""

stop_mock() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null
  MOCK_PID=""
}

stop_cpa() {
  [ -z "$CPA_PID" ] && return
  kill "$CPA_PID" 2>/dev/null
  local i=0
  while [ "$i" -lt 100 ] && kill -0 "$CPA_PID" 2>/dev/null; do i=$((i + 1)); sleep 0.1; done
  kill -9 "$CPA_PID" 2>/dev/null
  wait "$CPA_PID" 2>/dev/null
  CPA_PID=""
}

cleanup() { stop_cpa; stop_mock; }
trap cleanup EXIT INT TERM

start_cpa() {
  "$CPA_BIN" --config "$WORKDIR/config.yaml" >> "$WORKDIR/cpa.log" 2>&1 &
  CPA_PID=$!
  local i=0
  while [ "$i" -lt 120 ]; do
    if curl -s -m 2 -H "Authorization: Bearer $CKEY" "$BASE/v1/models" > "$WORKDIR/models.json"; then
      return 0
    fi
    i=$((i + 1)); sleep 0.25
  done
  echo "CPA did not become ready; see $WORKDIR/cpa.log" >&2
  tail -40 "$WORKDIR/cpa.log" >&2
  return 1
}

counts() { sort "$LOG" | uniq -c | awk '{printf "  %-12s %d\n", $2, $1}'; }
reset_log() { : > "$LOG"; }

# request <model> [session-id] [prompt] -> prints the JSON body
request() {
  local model="$1" sid="${2:-}" text="${3:-hi}"
  if [ -n "$sid" ]; then
    curl -s -m 30 "$BASE/v1/chat/completions" \
      -H "Authorization: Bearer $CKEY" -H "Content-Type: application/json" \
      -H "X-Session-Id: $sid" \
      -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$text\"}]}"
  else
    curl -s -m 30 "$BASE/v1/chat/completions" \
      -H "Authorization: Bearer $CKEY" -H "Content-Type: application/json" \
      -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$text\"}]}"
  fi
}

request_status() {
  local model="$1" sid="${2:-}" text="${3:-hi}"
  if [ -n "$sid" ]; then
    curl -s -o /dev/null -w '%{http_code}' -m 30 "$BASE/v1/chat/completions" \
      -H "Authorization: Bearer $CKEY" -H "Content-Type: application/json" \
      -H "X-Session-Id: $sid" \
      -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$text\"}]}"
  else
    curl -s -o /dev/null -w '%{http_code}' -m 30 "$BASE/v1/chat/completions" \
      -H "Authorization: Bearer $CKEY" -H "Content-Type: application/json" \
      -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$text\"}]}"
  fi
}

served_by() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["choices"][0]["message"]["content"])'; }

control() {
  curl -s -m 5 -X POST "http://127.0.0.1:$MOCK_PORT/control" \
    -H "Content-Type: application/json" -d "{\"model\":\"$1\",\"mode\":\"$2\"}" > /dev/null
}

python3 -u "$HERE/mock-upstream.py" "$LOG" "$MOCK_PORT" > "$WORKDIR/mock.out" 2>&1 &
MOCK_PID=$!
sleep 0.3
start_cpa || exit 1

echo "=============================================================="
echo " tea-combos end-to-end against CPA"
echo " mock=$MOCK_PORT cpa=$CPA_PORT workdir=$WORKDIR"
echo "=============================================================="

echo
echo "== (a) round-robin distribution: 30 requests to rr3, no session headers, distinct prompts =="
reset_log
i=0
while [ "$i" -lt 30 ]; do
  request rr3 "" "prompt-$i" > /dev/null
  i=$((i + 1))
done
echo "mock log per member:"
counts
echo "total: $(wc -l < "$LOG")"

echo
echo "== (b) stickiness: 10 requests X-Session-Id s-one, then 10 X-Session-Id s-two =="
reset_log
i=0
while [ "$i" -lt 10 ]; do request sticky3 "s-one" "hello" > /dev/null; i=$((i + 1)); done
echo "after s-one:"; counts
i=0
while [ "$i" -lt 10 ]; do request sticky3 "s-two" "hello" > /dev/null; i=$((i + 1)); done
echo "after s-two (both sessions):"; counts
echo "s-one body: $(request sticky3 s-one hello | served_by)"
echo "s-two body: $(request sticky3 s-two hello | served_by)"

echo
echo "== (b2) X-Claude-Code-Session-Id also binds (rung 1) =="
reset_log
i=0
while [ "$i" -lt 5 ]; do
  curl -s -m 30 "$BASE/v1/chat/completions" -H "Authorization: Bearer $CKEY" \
    -H "Content-Type: application/json" -H "X-Claude-Code-Session-Id: claude-one" \
    -d '{"model":"sticky3","messages":[{"role":"user","content":"hello"}]}' > /dev/null
  i=$((i + 1))
done
counts

echo
echo "== (c) affinity off: 9 requests to noaff with X-Session-Id s-off =="
reset_log
i=0
while [ "$i" -lt 9 ]; do request noaff "s-off" "hello" > /dev/null; i=$((i + 1)); done
counts

echo
echo "== (f) identity rungs on the real binary =="
reset_log
echo "no headers, same prompt twice -> message hash fallback:"
request rr3 "" "same prompt" | served_by
request rr3 "" "same prompt" | served_by
echo "no headers, different prompts -> independent (RR continues):"
request rr3 "" "prompt-a" | served_by
request rr3 "" "prompt-b" | served_by
echo "mock log:"; counts

echo
echo "== (d1) rebind on retryable failure =="
reset_log
BOUND=$(request rebind "s-rebind" "hello" | served_by)
echo "first request bound to: $BOUND"
control "${BOUND#served-by:}" 503
echo "after making ${BOUND#served-by:} return 503, same session:"
REBOUND=$(request rebind "s-rebind" "hello" | served_by)
echo "response served by: $REBOUND"
echo "mock log (bound member first, then fallback):"; counts
control "${BOUND#served-by:}" ok
echo
echo "== (d2) client error stops the chain and leaves the binding =="
reset_log
FIRST=$(request stopchain "s-stop" "hello" | served_by)
echo "first request bound to: $FIRST"
control "${FIRST#served-by:}" 400
echo "status after making ${FIRST#served-by:} return 400: $(request_status stopchain s-stop hello)"
echo "mock log (only the bound member, bad400 never called):"; counts
control "${FIRST#served-by:}" ok
echo "after restoring the member, same session served by: $(request stopchain s-stop hello | served_by)"
echo "mock log (binding retained):"; counts

echo
echo "== (e) restart persistence =="
reset_log
PERSIST=$(request rebind "s-persist" "keep me" | served_by)
echo "before restart bound to: $PERSIST"
echo "stopping CPA with SIGTERM (pid $CPA_PID)"
stop_cpa
echo "bindings in state file after shutdown:"
python3 - "$WORKDIR/state.json" <<'PY'
import json, sys
state = json.load(open(sys.argv[1]))
print("  binding keys:", len(state.get("bindings", {})))
PY
start_cpa || exit 1
reset_log
AFTER=$(request rebind "s-persist" "keep me" | served_by)
echo "after restart same session served by: $AFTER"
echo "mock log:"; counts

echo
echo "== (g) /v1/models as the client sees it =="
curl -s -H "Authorization: Bearer $CKEY" "$BASE/v1/models" \
  | python3 -c 'import json,sys; print("\n".join(m["id"] for m in json.load(sys.stdin)["data"]))' | sort

echo
echo "done. transcript saved by the caller; scratch state in $WORKDIR"
