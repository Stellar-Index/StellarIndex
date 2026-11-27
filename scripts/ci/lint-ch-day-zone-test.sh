#!/usr/bin/env bash
# lint-ch-day-zone-test.sh — fixture tests for scripts/ci/lint-ch-day-zone.sh.
#
# Each case builds a throwaway root holding one source file and asserts the gate's exit code.
# LINT overrides the gate under test (used to prove these cases catch a broken copy).
#
# Run: bash scripts/ci/lint-ch-day-zone-test.sh
# shellcheck disable=SC2016 # fixtures hold literal Go backticks
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="${LINT:-$PWD/scripts/ci/lint-ch-day-zone.sh}"

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

G='internal/storage/clickhouse/q.go'
run_case "today() fails" 1 "$G" 'const q = `SELECT count() FROM t WHERE day >= today() - 90`'
run_case "yesterday() fails" 1 "$G" 'const q = `SELECT count() FROM t WHERE day = yesterday()`'
run_case "toStartOfDay(now()) fails" 1 "$G" 'const q = `SELECT 1 FROM t WHERE close_time < toStartOfDay(now())`'
run_case "toDate(now()) fails" 1 "$G" 'const q = `SELECT 1 FROM t WHERE day < toDate(now())`'
run_case "toDate(now64(3)) fails" 1 "$G" 'const q = `SELECT toDate(now64(3))`'
run_case "toYYYYMMDD of a one-argument toDateTime fails" 1 "$G" 'const q = `SELECT toYYYYMMDD(toDateTime(?))`'
run_case "toDateTime(max(day)) fails" 1 "$G" 'const q = `SELECT toDateTime(max(day)) AS last_seen FROM t`'
run_case "toDateTime(toStartOfMonth(x)) fails" 1 "$G" 'const q = `SELECT toDateTime(toStartOfMonth(first_at)) FROM t`'
run_case "a .go outside the package that imports the driver fails" 1 'cmd/x/main.go' $'import _ "github.com/ClickHouse/clickhouse-go/v2"\nconst q = `SELECT today()`'
run_case "a deploy/clickhouse .sql fails" 1 'deploy/clickhouse/x.sql' 'SELECT toStartOfDay(now()) AS d;'
run_case "now('UTC') passes" 0 "$G" 'const q = `SELECT 1 FROM t WHERE close_time < toStartOfDay(now('"'UTC'"')) AND day < toDate(now('"'UTC'"'))`'
run_case "toDateTime(max(day), 'UTC') passes" 0 "$G" 'const q = `SELECT toDateTime(max(day), '"'UTC'"') FROM t`'
run_case "toDateTime(toStartOfMonth(x), 'UTC') passes" 0 "$G" 'const q = `SELECT toDateTime(toStartOfMonth(first_at), '"'UTC'"') FROM t`'
run_case "an instant comparison with now() passes" 0 "$G" 'const q = `SELECT 1 FROM t WHERE close_time >= now() - INTERVAL 7 DAY`'
run_case "a DEFAULT now() column passes" 0 'deploy/clickhouse/x.sql' 'CREATE TABLE t (ingested_at DateTime DEFAULT now()) ENGINE = Memory;'
run_case "toDate of a zone-bearing column passes" 0 "$G" 'const q = `SELECT toDate(close_time) FROM t`'
run_case "Go time.Now() passes" 0 "$G" 'func f() { _ = toDate(time.Now()) }'
run_case "the chzone:ok marker allows a line" 0 "$G" 'const q = `SELECT today()` // chzone:ok fixture'
run_case "a whole-line comment passes" 0 "$G" $'// today() is the server day\nconst q = `SELECT toDate(close_time)`'
run_case "a Postgres file without the driver is not scanned" 2 'internal/platform/x/q.go' 'const q = `SELECT today()`'
run_case "a test file is not scanned" 2 'internal/storage/clickhouse/q_test.go' 'const q = `SELECT today()`'
run_case "a tree with no date call fails rather than pass vacuously" 2 "$G" 'package clickhouse'

echo "lint-ch-day-zone-test: $pass passed, $fail failed ($((n + 1)) cases)"
[ "$fail" -eq 0 ]
