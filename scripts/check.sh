#!/usr/bin/env bash
# The gate that CI runs and that should pass before any push.
# gofmt check, go vet, tests, c-shared build, registry validation.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
fail=0

echo "=== gofmt ==="
unformatted="$(gofmt -l plugins vendor internal 2>/dev/null)"
if [ -n "$unformatted" ]; then
  echo "these files need gofmt:" >&2
  echo "$unformatted" >&2
  fail=1
else
  echo "ok"
fi

echo "=== go vet + tests ==="
scripts/test.sh || fail=1

echo "=== c-shared build ==="
scripts/build.sh || fail=1

echo "=== registry ==="
scripts/registry-build.sh --check || fail=1

if [ "$fail" -ne 0 ]; then
  echo "CHECK FAILED" >&2
else
  echo "ALL CHECKS PASSED"
fi
exit "$fail"
