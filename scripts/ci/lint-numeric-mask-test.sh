#!/usr/bin/env bash
# lint-numeric-mask-test.sh — fixture tests for scripts/ci/lint-numeric-mask.sh.
#
# Each case builds a throwaway root holding one source file and asserts the gate's exit code.
# LINT overrides the gate under test (used to prove these cases catch a broken copy).
#
# Run: bash scripts/ci/lint-numeric-mask-test.sh
# shellcheck disable=SC2016 # fixtures hold literal Go backticks
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="${LINT:-$PWD/scripts/ci/lint-numeric-mask.sh}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
n=0
# run_case <desc> <want-exit> <relative-path> <content>
run_case() {
  local desc="$1" want="$2" rel="$3" body="$4" root got
  n=$((n + 1))
  root="$TMP/case$n"
  mkdir -p "$root/$(dirname "$rel")"
  printf '%s\n' "$body" >"$root/$rel"
  LINT_ROOT="$root" bash "$LINT" >"$root/out" 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $desc — want exit $want, got $got" >&2
    sed 's/^/    /' "$root/out" >&2
  fi
}

if bash "$LINT" >"$TMP/real" 2>&1; then
  pass=$((pass + 1))
else
  fail=$((fail + 1))
  echo "FAIL: the repo's own tree must pass" >&2
  sed 's/^/    /' "$TMP/real" >&2
fi

G='internal/x/q.go'
run_case "FM fixed-width mask fails" 1 "$G" 'const q = `SELECT to_char(x, '"'FM999999990.00'"')`'
run_case "mask without FM fails" 1 "$G" 'const q = `SELECT to_char(x, '"'990.0'"')`'
run_case "group-separator mask fails" 1 "$G" 'const q = `SELECT to_char(x, '"'999,999'"')`'
run_case "sign and locale decimal mask fails" 1 "$G" 'const q = `SELECT to_char(x, '"'S999D99'"')`'
run_case "trailing MI / PR mask fails" 1 "$G" 'const q = `SELECT to_char(x, '"'9999MI'"'), to_char(y, '"'999PR'"')`'
run_case "upper-case TO_CHAR fails" 1 "$G" 'const q = `SELECT TO_CHAR(x, '"'FM90.00'"')`'
run_case "a call split across lines fails" 1 "$G" $'const q = `SELECT to_char(\n  round(x, 2),\n  \'FM99990.00\'\n)`'
run_case "a mask after a nested call fails" 1 "$G" 'const q = `SELECT to_char(coalesce(x, 0), '"'FM990.00'"')`'
run_case "a .sql file under cmd fails" 1 'cmd/x/q.sql' "SELECT to_char(x, 'FM990.00');"
run_case "a migration fails" 1 'migrations/0001_x.up.sql' "CREATE VIEW v AS SELECT to_char(x, '990.00') AS c;"
run_case "a date mask passes" 0 "$G" 'const q = `SELECT to_char(ts, '"'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'"')`'
run_case "a time mask with MI passes" 0 "$G" 'const q = `SELECT to_char(ts, '"'HH24:MI'"'), to_char(ts, '"'DD'"')`'
run_case "a concatenated mask passes" 0 "$G" 'const q = `SELECT to_char(date_trunc('"'"'` + t + `'"'"', ts), '"'"'` + format + `'"'"')`'
run_case "a numeric literal in the value argument passes" 0 "$G" 'const q = `SELECT to_char(coalesce(ts, '"'0'"'), '"'YYYY'"')`'
run_case "a whole-line comment naming a mask passes" 0 'migrations/0001_x.up.sql' $'-- identical to_char(x, \'FM999999990.00\')\nSELECT to_char(ts, \'YYYY\');'
run_case "a test file is not scanned" 2 'internal/x/q_test.go' 'const q = `SELECT to_char(x, '"'FM990.00'"')`'
run_case "an unclosed call fails closed" 1 "$G" 'const q = `SELECT to_char(ts, '"'YYYY'"'), to_char(x, '"'YYYY'"'`'
run_case "a tree with no to_char fails rather than pass vacuously" 2 "$G" 'package x'

echo "lint-numeric-mask-test: $pass passed, $fail failed ($((n + 1)) cases)"
[ "$fail" -eq 0 ]
