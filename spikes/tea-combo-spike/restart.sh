#!/bin/sh
# Restart the spike CPA instance cleanly. The old process holds the port, and killing by
# pattern from inside ssh kills our own remote shell, so use an exact pgrep on the binary.
pids=$(pgrep -x cli-proxy-api)
if [ -n "$pids" ]; then
  kill $pids 2>/dev/null
  sleep 2
  pids=$(pgrep -x cli-proxy-api)
  [ -n "$pids" ] && kill -9 $pids 2>/dev/null
fi
rm -f <spike-dir>/cpa.log
cd <spike-dir> || exit 1
setsid /root/reference/CLIProxyAPI/cli-proxy-api --config <spike-dir>/config.yaml \
  > <spike-dir>/cpa.log 2>&1 < /dev/null &
sleep 7
grep -i pluginhost <spike-dir>/cpa.log | head -4
