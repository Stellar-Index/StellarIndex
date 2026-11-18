#!/usr/bin/env bash
# lint-i128-sum-test.sh — fixture tests for scripts/ci/lint-i128-sum.sh.
#
# Pins each Int128 spelling the lint must catch, the marker scope, the
# ClickHouse-only reach of the bare-column rule, and that the column list
# follows tier1_schema.sql.
#
# Run: bash scripts/ci/lint-i128-sum-test.sh
# The fixtures are Go source with backtick raw strings, kept literal on purpose.
# shellcheck disable=SC2016
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-i128-sum.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

SCHEMA='CREATE TABLE IF NOT EXISTS stellar.t
(
    amount       Int128,
    holders      UInt64
)'

pass=0
fail=0
# run_case <desc> <want-exit> <relative path> <source> [schema]
run_case() {
  local desc="$1" want="$2" rel="$3" src="$4" schema="${5:-$SCHEMA}" root got
  root="$(mktemp -d "$TMP/case.XXXXXX")"
  mkdir -p "$root/scripts/ci" "$root/internal/storage/clickhouse" "$root/cmd" "$root/deploy/clickhouse" "$(dirname "$root/$rel")"
  cp "$LINT" "$root/scripts/ci/lint-i128-sum.sh"
  printf '%s\n' "$schema" >"$root/deploy/clickhouse/tier1_schema.sql"
  printf '%s\n' "$src" >"$root/$rel"
  bash "$root/scripts/ci/lint-i128-sum.sh" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (want exit $want, got $got)"
    fail=$((fail + 1))
  fi
}

CH=internal/storage/clickhouse/x.go

run_case "clean tree passes" 0 "$CH" 'q := `SELECT sum(toInt256(amount)) FROM t`'
run_case "sum(toInt128(...)) is flagged" 1 "$CH" 'q := `SELECT sum(toInt128(x)) FROM t`'
run_case "sumIf(toInt128(...)) is flagged" 1 "$CH" 'q := `SELECT sumIf(toInt128(x), y > 0) FROM t`'
run_case "sumState(toInt128(...)) is flagged" 1 "$CH" 'q := `SELECT sumState(toInt128(x)) FROM t`'
run_case "sumMerge over an Int128 cast is flagged" 1 "$CH" 'q := `SELECT sumMerge(toInt128(x)) FROM t`'
run_case "CAST(... AS Int128) is flagged" 1 "$CH" 'q := `SELECT sum(CAST(x AS Int128)) FROM t`'
run_case "CAST(..., 'Int128') is flagged" 1 "$CH" "q := \`SELECT sum(CAST(x, 'Int128')) FROM t\`"
run_case "...::Int128 is flagged" 1 "$CH" 'q := `SELECT sum(x::Int128) FROM t`'
run_case "a cast outside internal/storage/clickhouse is flagged" 1 "internal/ops/y.go" 'q := `SELECT sum(toInt128(x)) FROM t`'
run_case "a bare tier1 Int128 column is flagged" 1 "$CH" 'q := `SELECT sum(amount) FROM t`'
run_case "a qualified bare tier1 Int128 column is flagged" 1 "$CH" 'q := `SELECT sumIf(m.amount, d = 1) FROM t`'
run_case "a bare Int128 column in deploy/clickhouse SQL is flagged" 1 "deploy/clickhouse/x.sql" 'SELECT sum(amount) FROM stellar.t;'
run_case "a bare Int128 column in a Go file importing clickhouse-go is flagged" 1 "internal/ops/y.go" 'import _ "github.com/ClickHouse/clickhouse-go/v2"
q := `SELECT sum(amount) FROM t`'
run_case "a bare column name in Postgres code is not flagged" 0 "internal/storage/timescale/y.go" 'q := `SELECT sum(amount) FROM t`'
run_case "a non-Int128 column is not flagged" 0 "$CH" 'q := `SELECT sum(holders) FROM t`'
run_case "a column tier1 newly declares Int128 is flagged" 1 "$CH" 'q := `SELECT sum(widget_total) FROM t`' \
  "$SCHEMA
CREATE TABLE stellar.u (widget_total Int128,
    widget_total Int128
)"
run_case "same-line marker exempts" 0 "$CH" 'q := `SELECT sum(toInt128(x)) -- i128sum:ok Int64 stroops'
run_case "marker alone on the line above exempts" 0 "$CH" 'q := `SELECT
    -- i128sum:ok Int64 stroops
    sum(toInt128(x)) FROM t`'
run_case "marker without a reason does not exempt" 1 "$CH" 'q := `SELECT sum(toInt128(x)) -- i128sum:ok'
run_case "marker trailing a code line does not exempt the next line" 1 "$CH" 'q := `SELECT sum(toInt128(x)), -- i128sum:ok Int64 stroops
    sum(toInt128(y)) FROM t`'
run_case "a comment line mentioning the pattern is not flagged" 0 "$CH" '// never sum(toInt128(x)) an i128 amount'
run_case "a schema with no Int128 column fails the instrument check" 1 "$CH" 'q := 1' 'CREATE TABLE t (a UInt64)'

echo "lint-i128-sum-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
