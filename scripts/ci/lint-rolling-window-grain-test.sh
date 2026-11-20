#!/usr/bin/env bash
# shellcheck disable=SC2016 # fixtures are literal Go source with backticks
# Prove each lint-rolling-window-grain check can fail, against a fixture tree outside any git checkout.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
LINT="$PWD/scripts/ci/lint-rolling-window-grain.sh"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
PASS=0; FAIL=0

reset() { rm -rf "$T/r"; mkdir -p "$T/r/internal/storage/timescale"; }
src() { printf '%b\n' "$1" > "$T/r/internal/storage/timescale/${2:-a.go}"; }
check() { # check <name> <ok|red> [expected output substring]
  local rc; bash "$LINT" --root "$T/r" >"$T/out" 2>&1; rc=$?
  if { [ "$2" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$2" = red ] && [ "$rc" -ne 0 ] && grep -qF -- "${3:-}" "$T/out"; }; then
    printf '  ok   %s\n' "$1"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$1" "$rc" "$2"; sed 's/^/       /' "$T/out"; FAIL=$((FAIL+1))
  fi
}

reset; src 'package t\nconst q = `SELECT sum(trade_count) FROM pools_per_source_1h r\n WHERE r.bucket >= NOW() - INTERVAL '"'"'24 hours'"'"'`'
check "rolling bound on an hourly view is rejected, at its literal's line" red "a.go:2"
reset; src 'const q = `SELECT x FROM prices_1d WHERE bucket > now() - interval '"'"'90 minutes'"'"'`'; check "lower-case now() and > are rejected" red "a.go:1"
reset; src 'const q = `SELECT x FROM twap_1h WHERE bucket\n  >=\n  NOW() - INTERVAL '"'"'2 hours'"'"'`'; check "a bound split over lines is rejected" red "a.go:1"
reset; src 'const q = `SELECT x FROM prices_1m WHERE bucket >= NOW() - INTERVAL '"'"'24 hours'"'"'`'; check "a minute view on a rolling bound passes" ok
reset; src 'const q = `SELECT x FROM prices_1h WHERE bucket >= time_bucket('"'"'1 hour'"'"', NOW() - INTERVAL '"'"'24 hours'"'"')`'; check "a time_bucket-aligned bound passes" ok
reset; src 'const q = `SELECT a FROM (SELECT x FROM prices_1m WHERE bucket >= NOW() - INTERVAL '"'"'1 hour'"'"') m\n UNION ALL SELECT y FROM prices_1h WHERE bucket >= time_bucket('"'"'1 hour'"'"', NOW())`'
check "each SELECT is judged on its own" ok
reset; src 'const q = `SELECT x FROM prices_1w WHERE bucket >= NOW() - INTERVAL '"'"'30 days'"'"'`'; check "a day-or-wider interval passes" ok
reset; src 'const q = `SELECT x FROM prices_1hz WHERE bucket >= NOW() - INTERVAL '"'"'1 hour'"'"'`'; check "a view name is matched whole" ok
reset; src 'const q = `SELECT x FROM prices_4h WHERE bucket >= NOW() - INTERVAL '"'"'8 hours'"'"'`' a_test.go; check "test files are ignored" ok
reset; src 'const a = `SELECT 1`\nconst q = `SELECT x FROM prices_1mo WHERE bucket >= NOW() - INTERVAL '"'"'720 hours'"'"'`' b.go; check "a later literal in a file is scanned" red "b.go:2"

echo "lint-rolling-window-grain self-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
