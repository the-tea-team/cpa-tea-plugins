#!/bin/sh
# Reproduce the tea-combos streaming proof against a real CPA binary.
#
# Usage: run-stream-proof.sh [workdir] [cpa-binary] [tea-combos.so]
#
# Starts the deterministic mock upstream from this directory, runs CPA with the plugin in a
# flat plugin dir, and prints the mock's own request log plus timestamped SSE output for:
#   (a) stream-fallback [stream-bad, stream-good]      -> bad is tried, then good serves
#   (b) stream-cut-combo [stream-cut, stream-good]     -> cut sends one chunk then fails;
#                                                        good is never called
#
# Ports stay in CPA's test range. Only the PIDs started here are stopped.
set -u

WORKDIR=${1:-/var/tmp/tea-combos-stream-proof}
CPA_BIN=${2:-/root/reference/CLIProxyAPI/cli-proxy-api}
PLUGIN_SO=${3:-$(dirname "$0")/../tea-combos.so}
MOCK_PORT=${MOCK_PORT:-18342}
CPA_PORT=${CPA_PORT:-18343}
KEY="proof-key"

HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
mkdir -p "$WORKDIR/plugins"
cp "$PLUGIN_SO" "$WORKDIR/plugins/tea-combos.so" || exit 1

cat > "$WORKDIR/config.yaml" <<EOF
server:
  host: "127.0.0.1"
  port: $CPA_PORT
access:
  api-keys:
    - "$KEY"
management:
  allow-remote: false
  secret-key: "change-me"
observability:
  logs:
    debug: true
openai-compatibility:
  - name: "mock"
    base-url: "http://127.0.0.1:$MOCK_PORT/v1"
    request-retry: 0
    keys:
      - api-key: "mock-key"
    models:
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
        stream-fallback:
          strategy: failover
          members:
            - model: stream-bad
            - model: stream-good
        stream-cut-combo:
          strategy: failover
          members:
            - model: stream-cut
            - model: stream-good
EOF

LOG="$WORKDIR/mock.log"
: > "$LOG"
python3 -u "$HERE/mock-upstream.py" "$LOG" "$MOCK_PORT" > "$WORKDIR/mock.out" 2>&1 &
MOCK_PID=$!
"$CPA_BIN" --config "$WORKDIR/config.yaml" > "$WORKDIR/cpa.log" 2>&1 &
CPA_PID=$!
cleanup() {
  kill "$CPA_PID" "$MOCK_PID" 2>/dev/null
  wait "$CPA_PID" "$MOCK_PID" 2>/dev/null
}
trap cleanup EXIT INT TERM

i=0
while [ "$i" -lt 40 ]; do
  if curl -s -m 2 -H "Authorization: Bearer $KEY" "http://127.0.0.1:$CPA_PORT/v1/models" > /dev/null; then
    break
  fi
  i=$((i + 1))
  sleep 0.5
done

request() {
  model="$1"
  : > "$LOG"
  echo "=============== $model ==============="
  curl -sN -m 30 "http://127.0.0.1:$CPA_PORT/v1/chat/completions" \
    -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d "{\"model\":\"$model\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
    | while IFS= read -r line; do
        printf '%s %s\n' "$(date +%H:%M:%S.%3N)" "$line"
      done
  echo "--- upstream call order (mock log) ---"
  sed 's/^/   /' "$LOG"
}

request stream-fallback
request stream-cut-combo
