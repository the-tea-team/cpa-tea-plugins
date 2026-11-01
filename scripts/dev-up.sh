#!/usr/bin/env bash
# Local dev loop: run a CPA server with this repo's built plugins available.
#   scripts/dev-up.sh            start CPA in the foreground with .dev/ state
#   CPA_CONFIG=/path/config.yaml scripts/dev-up.sh
#
# Credentials are never copied into the repo. Point CPA_AUTH_DIR at your real auth dir.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

state=".dev"
mkdir -p "$state/plugins" "$state/auths"

cpa_bin="${CPA_BIN:-/root/reference/CLIProxyAPI/cli-proxy-api}"
if [ ! -x "$cpa_bin" ]; then
  echo "CPA binary not found at $cpa_bin" >&2
  echo "build it: cd /root/reference/CLIProxyAPI && go build -o cli-proxy-api ./cmd/server" >&2
  exit 1
fi

# CPA discovers shared libraries in <dir>/<goos>/<goarch>/ and <dir> only — NOT in per-plugin
# subdirectories. Every built .so therefore goes flat into $state/plugins.
echo "building plugins into $state/plugins ..."
scripts/build.sh || exit 1
rm -f "$state/plugins"/*.so
for artifact in plugins/*/*.so vendor/*/*/*.so; do
  [ -f "$artifact" ] || continue
  cp -f "$artifact" "$state/plugins/"
done

config="${CPA_CONFIG:-$state/config.yaml}"
if [ ! -f "$config" ]; then
  cat > "$config" <<EOF
# Local development config. Not committed. Secrets stay out of this file.
server:
  host: "127.0.0.1"
  port: 8317
access:
  api-keys:
    - "dev-key"
management:
  allow-remote: false
  secret-key: "dev-management-key"
plugins:
  enabled: true
  dir: "$(pwd)/$state/plugins"
  configs: {}
EOF
  echo "wrote $config (edit plugins.configs to enable your plugin)"
fi

echo "starting CPA on 127.0.0.1:8317 with plugin dir $state/plugins"
exec "$cpa_bin" --config "$config"
