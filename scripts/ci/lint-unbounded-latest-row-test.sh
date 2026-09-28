#!/usr/bin/env bash
# shellcheck disable=SC2016  # the fixtures are Go source; `$1` and backticks must stay literal
# lint-unbounded-latest-row-test.sh — fixtures for the unbounded latest-row
# gate (scripts/ci/lint-unbounded-latest-row.py, #594).
#
# Pinned: the SupplyCoverageStats shape that shipped is caught; a time or
# ledger floor passes; a marker above the query or on the func doc passes,
# a bare marker with no reason does not; non-hypertables, comments outside
# raw strings (even with a stray backtick) and _test.go files are ignored; an empty root or a migrations
# dir with no hypertable FAILS rather than passing vacuously.
#
# Run: bash scripts/ci/lint-unbounded-latest-row-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-unbounded-latest-row.py"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/mig"
cat >"$TMP/mig/0001_x.up.sql" <<'SQL'
SELECT create_hypertable(
    'asset_supply_history',
    'time',
    chunk_time_interval => INTERVAL '7 days');
SELECT create_hypertable('trades', 'ts');
SQL

pass=0; fail=0
check() { # check <desc> <want-exit> <dir> [migrations]
  local desc="$1" want="$2" dir="$3" mig="${4:-$TMP/mig}" got
  MIGRATIONS_DIR="$mig" python3 "$LINT" "$dir" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then echo "  ok   $desc"; pass=$((pass + 1))
  else echo "  FAIL $desc (exit $got, want $want)"; fail=$((fail + 1)); fi
}
mk() { mkdir -p "$TMP/$1"; printf '%s\n' "$3" >"$TMP/$1/$2"; }

echo "lint-unbounded-latest-row-test: detection"

mk shipped diag.go 'package ts
func (s *Store) SupplyCoverageStats() {
	const q = `
		WITH latest AS (
		    SELECT DISTINCT ON (asset_key)
		        asset_key, time, ledger_sequence
		    FROM asset_supply_history
		    ORDER BY asset_key, ledger_sequence DESC, time DESC
		)
		SELECT COUNT(*) FROM latest
	`
}'
check "the shipped SupplyCoverageStats shape is caught" 1 "$TMP/shipped"

mk keyed t.go 'package ts
func f() {
	const q = `SELECT DISTINCT ON (source) source FROM trades WHERE base_asset = $1 ORDER BY source, ts DESC`
}'
check "a keyed but unfloored read is caught" 1 "$TMP/keyed"

echo "lint-unbounded-latest-row-test: bounds"

mk timefloor t.go 'package ts
func f() {
	const q = `SELECT DISTINCT ON (asset_key) asset_key FROM asset_supply_history WHERE "time" >= now() - $1::interval ORDER BY asset_key, "time" DESC`
}'
check "a quoted time-column floor passes" 0 "$TMP/timefloor"

mk ledgerfloor t.go 'package ts
func f() {
	const q = `SELECT DISTINCT ON (source) source FROM trades WHERE ledger BETWEEN $1 AND $2 ORDER BY source, ts DESC`
}'
check "a ledger BETWEEN bound passes" 0 "$TMP/ledgerfloor"

mk upperonly t.go 'package ts
func f() {
	const q = `SELECT DISTINCT ON (source) source FROM trades WHERE ledger <= $1 ORDER BY source, ts DESC`
}'
check "an upper bound alone is not a floor" 1 "$TMP/upperonly"

echo "lint-unbounded-latest-row-test: waivers"

mk waiveabove t.go 'package ts
func f() {
	// unbounded-latest-ok: a floor would drop sources that went quiet.
	const q = `SELECT DISTINCT ON (source) source FROM trades ORDER BY source, ts DESC`
}'
check "a marker in the comment block above the query passes" 0 "$TMP/waiveabove"

mk waivefunc t.go 'package ts
// f reads the latest trade per source.
// unbounded-latest-ok: a floor would drop sources that went quiet.
func f() {
	x := 1
	const q = `SELECT DISTINCT ON (source) source FROM trades ORDER BY source, ts DESC`
}'
check "a marker on the enclosing func doc passes" 0 "$TMP/waivefunc"

mk waivebare t.go 'package ts
func f() {
	// unbounded-latest-ok:
	const q = `SELECT DISTINCT ON (source) source FROM trades ORDER BY source, ts DESC`
}'
check "a bare marker with no reason does not waive" 1 "$TMP/waivebare"

mk waiveother t.go 'package ts
// unbounded-latest-ok: belongs to g, not f.
func g() {}

func f() {
	const q = `SELECT DISTINCT ON (source) source FROM trades ORDER BY source, ts DESC`
}'
check "a marker on a different func does not waive" 1 "$TMP/waiveother"

echo "lint-unbounded-latest-row-test: scope"

mk plain t.go 'package ts
func f() {
	const q = `SELECT DISTINCT ON (pool) pool FROM pool_registry ORDER BY pool, ledger DESC`
}'
check "a table that is not a hypertable is ignored" 0 "$TMP/plain"

mk comment t.go 'package ts
// SELECT DISTINCT ON (source) FROM trades — the shape this file avoids.
func f() {}'
check "a comment naming the shape is not a query" 0 "$TMP/comment"

mk oddtick t.go 'package ts
// the `DISTINCT ON shape named with one stray backtick
func f() {
	const q = `SELECT DISTINCT ON (source) source FROM trades ORDER BY source, ts DESC`
}'
check "a stray backtick in a comment does not hide a query" 1 "$TMP/oddtick"

mk testfile t_test.go 'package ts
func f() {
	const q = `SELECT DISTINCT ON (source) source FROM trades ORDER BY source, ts DESC`
}'
check "_test.go files are exempt" 2 "$TMP/testfile"

mkdir -p "$TMP/empty" "$TMP/nomig"
check "a root with no Go files FAILS rather than passing" 2 "$TMP/empty"
check "a migrations dir with no hypertable FAILS rather than passing" 2 "$TMP/shipped" "$TMP/nomig"

echo "lint-unbounded-latest-row-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
