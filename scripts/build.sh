#!/usr/bin/env bash
# Build one or all plugins as CPA native shared libraries.
#   scripts/build.sh            build every plugins/* and vendor/*/*
#   scripts/build.sh tea-combos build one
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

target="${1:-}"
fail=0
built=0

build_one() {
  local dir="$1"
  local id
  id="$(basename "$dir")"
  [ -f "$dir/go.mod" ] || return 0
  local ext="so"
  case "$(go env GOOS)" in
    darwin) ext="dylib" ;;
    windows) ext="dll" ;;
  esac
  echo "=== building $id ($dir) ==="
  ( cd "$dir" && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
      -ldflags "-s -w" -o "$id.$ext" . ) || { echo "BUILD FAILED: $id" >&2; fail=1; return 1; }
  rm -f "$dir/$id.h"
  ls -lh "$dir/$id.$ext" | awk '{print "  ->", $9, $5}'
  built=$((built + 1))
}

if [ -n "$target" ]; then
  for d in "plugins/$target" "vendor/$target"; do
    [ -d "$d" ] && build_one "$d"
  done
else
  for d in plugins/*/ vendor/*/*/; do
    [ -d "$d" ] && build_one "${d%/}"
  done
fi

[ "$built" -eq 0 ] && echo "no plugins found to build (nothing under plugins/ with a go.mod yet)"
exit "$fail"
