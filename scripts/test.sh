#!/usr/bin/env bash
# Run go test for one or all plugin modules.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

target="${1:-}"
fail=0
ran=0

test_one() {
  local dir="$1"
  [ -f "$dir/go.mod" ] || return 0
  echo "=== testing $(basename "$dir") ==="
  ( cd "$dir" && go test ./... ) || fail=1
  ran=$((ran + 1))
}

if [ -n "$target" ]; then
  for d in "plugins/$target" "vendor/$target" "internal/tea"; do
    [ -d "$d" ] && test_one "$d"
  done
else
  for d in plugins/*/ vendor/*/*/ internal/tea; do
    [ -d "$d" ] && test_one "${d%/}"
  done
fi

[ "$ran" -eq 0 ] && echo "no modules with tests found yet"
exit "$fail"
