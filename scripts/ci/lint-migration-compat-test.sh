#!/usr/bin/env bash
# lint-migration-compat-test.sh — fixture tests for the rule-9 gate.
#
# The gate in lint-migration-compat.sh is the only thing standing
# between a `DROP COLUMN` and a deploy whose rollback cannot undo it
# (CID-14). A gate that silently stops matching is worse than no gate,
# so its behaviour is pinned here rather than assumed:
#
#   - each violation class actually fires (including `rename`, whose
#     regex contains an alternation — an earlier single-string
#     `class|regex` encoding split ON that alternation and produced an
#     unbalanced-paren grep whose error was swallowed by `|| true`);
#   - the inline escape needs a REASON, not just the marker;
#   - `NOT NULL DEFAULT …` is not flagged (it is old-binary-safe);
#   - a commented-out DDL line is not flagged;
#   - baseline entries grandfather, and stale ones fail (repo mode)
#     but are tolerated under `--staged` (older tag, fewer files).
#
# Run: bash scripts/ci/lint-migration-compat-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-migration-compat.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# run <fixture-sql> <baseline-content> [extra args…] → sets RC + OUT
run() {
  local sql="$1" baseline="$2"; shift 2
  rm -rf "$TMP/migrations"
  mkdir -p "$TMP/migrations"
  printf '%s\n' "$sql" > "$TMP/migrations/0001_fixture.up.sql"
  printf '%s\n' "$baseline" > "$TMP/baseline"
  OUT="$(MIGRATIONS_DIR="$TMP/migrations" MIGRATION_COMPAT_BASELINE="$TMP/baseline" \
    bash "$LINT" "$@" 2>&1)"
  RC=$?
}

# expect <name> <want-rc> <want-substring-or-empty>
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -q -- "$want_sub"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# 1. Each destructive class fires with its own label.
run 'ALTER TABLE trades DROP COLUMN legacy_price;' ''
expect 'drop-column fires' 1 'drop-column'

run 'DROP TABLE classic_movements;' ''
expect 'drop-table fires' 1 'drop-table'

run 'ALTER TABLE trades RENAME COLUMN price TO px;' ''
expect 'rename fires (regex alternation intact)' 1 'rename'

run 'ALTER TABLE trades ALTER COLUMN amount TYPE bigint;' ''
expect 'alter-type fires' 1 'alter-type'

run 'ALTER TABLE trades ALTER COLUMN source SET NOT NULL;' ''
expect 'set-not-null fires' 1 'set-not-null'

run 'DROP MATERIALIZED VIEW ohlc_extremes;' ''
expect 'drop-view fires' 1 'drop-view'

run 'ALTER TABLE trades ADD CONSTRAINT trades_kind_check CHECK (kind IN (1));' ''
expect 'add-constraint fires' 1 'add-constraint'

run 'ALTER TABLE trades ADD COLUMN venue text NOT NULL;' ''
expect 'add-not-null-column (no default) fires' 1 'add-not-null-column'

# 2. Additive / old-binary-safe shapes must NOT fire.
run 'ALTER TABLE trades ADD COLUMN venue text NOT NULL DEFAULT '"'"'sdex'"'"';' ''
expect 'NOT NULL DEFAULT is additive-safe' 0 'passed'

run 'ALTER TABLE trades ADD COLUMN venue text;
CREATE INDEX trades_venue_idx ON trades (venue);
ALTER TABLE trades DROP CONSTRAINT trades_ledger_check;
DROP INDEX IF EXISTS trades_unused_idx;' ''
expect 'add-column / create-index / loosening not flagged' 0 'passed'

run '-- Historically we would DROP COLUMN legacy_price here.
CREATE TABLE t (id bigint);' ''
expect 'commented-out DDL not flagged' 0 'passed'

# 3. Inline escape hatch: marker alone is not enough — a reason is.
run 'ALTER TABLE trades DROP COLUMN legacy_price;  -- migration-compat:ok added in 0104, no released binary reads it' ''
expect 'inline marker + reason passes' 0 'passed'

run 'ALTER TABLE trades DROP COLUMN legacy_price;  -- migration-compat:ok' ''
expect 'inline marker without a reason still fails' 1 'drop-column'

# 4. Baseline grandfathering + shrink-only.
run 'ALTER TABLE trades DROP COLUMN legacy_price;' '0001_fixture.up.sql:drop-column'
expect 'baselined violation passes' 0 'passed'

run 'ALTER TABLE trades DROP COLUMN legacy_price;' '0001_fixture.up.sql:drop-table'
expect 'baseline entry for a different class does not grandfather' 1 'drop-column'

run 'CREATE TABLE t (id bigint);' '0099_gone.up.sql:drop-table'
expect 'stale baseline entry fails in repo mode' 1 'stale baseline entry'

run 'CREATE TABLE t (id bigint);' '0099_gone.up.sql:drop-table' --staged
expect 'stale baseline entry tolerated under --staged' 0 'passed'

# 5. New violations still fail under --staged (the deploy-time gate).
run 'ALTER TABLE trades DROP COLUMN legacy_price;' '' --staged
expect 'staged mode still blocks a new violation' 1 'drop-column'

echo
echo "lint-migration-compat-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
