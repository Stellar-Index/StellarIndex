#!/usr/bin/env bash
# lint-migration-compat-test.sh — fixture tests for the rule-9 gate.
#
# The gate in lint-migration-compat.sh is the only thing standing
# between a `DROP COLUMN` and a deploy whose rollback cannot undo it
# A gate that silently stops matching is worse than no gate,
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
#     but are tolerated under `--staged` (older tag, fewer files);
#   - `dml` (DELETE/TRUNCATE/UPDATE) needs a file-level RAISE EXCEPTION
#     row-count guard — the inline marker alone does not clear it
#     (0137's bare `DELETE FROM comet_liquidity;` passed every
#     other class).
#
# Run: bash scripts/ci/lint-migration-compat-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-migration-compat.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# shellcheck source=scripts/ci/lib/test-expect.sh
. "$PWD/scripts/ci/lib/test-expect.sh"

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

run 'CREATE UNIQUE INDEX IF NOT EXISTS hooks_account_url ON hooks (account_id, url);' ''
expect 'a unique index fires' 1 'add-unique'

run 'ALTER TABLE hooks ADD UNIQUE (account_id, url);' ''
expect 'ADD UNIQUE fires' 1 'add-unique'

run 'ALTER TABLE hooks ADD PRIMARY KEY (id);' ''
expect 'ADD PRIMARY KEY fires' 1 'add-unique'

run 'ALTER TABLE hooks ADD CHECK (kind > 0);' ''
expect 'ADD CHECK fires' 1 'add-unique'

run 'CREATE INDEX hooks_account ON hooks (account_id);' ''
expect 'a plain index is additive' 0 'passed'

run 'ALTER TABLE trades ADD COLUMN venue text NOT NULL;' ''
expect 'add-not-null-column (no default) fires' 1 'add-not-null-column'

# 2. Additive / old-binary-safe shapes must NOT fire.
run 'ALTER TABLE trades ADD COLUMN venue text NOT NULL DEFAULT '"'"'sdex'"'"';' ''
expect 'NOT NULL DEFAULT is additive-safe' 0 'passed'

run 'ALTER TABLE trades ADD COLUMN venue text;
CREATE INDEX trades_venue_idx ON trades (venue);' ''
expect 'add-column / create-index not flagged' 0 'passed'

run '-- Historically we would DROP COLUMN legacy_price here.
CREATE TABLE t (id bigint);' ''
expect 'commented-out DDL not flagged' 0 'passed'

# 3. Inline escape hatch: marker alone is not enough — a reason is.
run 'ALTER TABLE trades DROP COLUMN legacy_price;  -- migration-compat:ok added in 0104, no released binary reads it' ''
expect 'inline marker + reason passes' 0 'passed'

run 'ALTER TABLE trades DROP COLUMN legacy_price;  -- migration-compat:ok' ''
expect 'inline marker without a reason still fails' 1 'drop-column'

# 1b. Forms the first eight classes read past.
run 'ALTER TABLE trades DROP legacy_price;' ''
expect 'DROP without the COLUMN keyword fires' 1 'drop-column'

run 'ALTER TABLE trades DROP IF EXISTS legacy_price;' ''
expect 'DROP IF EXISTS without COLUMN fires' 1 'drop-column'

run 'ALTER TABLE trades RENAME price TO px;' ''
expect 'RENAME without the COLUMN keyword fires' 1 'rename'

run 'ALTER TABLE hooks DROP CONSTRAINT hooks_account_url_key;' ''
expect 'DROP CONSTRAINT fires' 1 'drop-arbiter'

run 'DROP INDEX IF EXISTS hooks_account_url;' ''
expect 'DROP INDEX fires' 1 'drop-arbiter'

run 'ALTER TABLE hooks RENAME CONSTRAINT hooks_key TO hooks_account_url_key;' ''
expect 'RENAME CONSTRAINT fires' 1 'drop-arbiter'

run 'ALTER TABLE hooks DROP CONSTRAINT hooks_old_check; -- migration-compat:ok no binary names it' ''
expect 'a marked DROP CONSTRAINT passes' 0 'passed'

run 'ALTER TABLE trades ALTER COLUMN source DROP DEFAULT;' ''
expect 'DROP DEFAULT fires' 1 'drop-default'

run 'DROP FUNCTION IF EXISTS forget(text);' ''
expect 'DROP FUNCTION fires' 1 'drop-function'

run 'DROP TYPE trade_kind;' ''
expect 'DROP TYPE fires' 1 'drop-function'

run 'ALTER TABLE hooks ADD FOREIGN KEY (account_id) REFERENCES accounts (id);' ''
expect 'ADD FOREIGN KEY fires' 1 'add-foreign-key'

run 'ALTER TABLE trades ALTER COLUMN source DROP NOT NULL;' ''
expect 'DROP NOT NULL stays additive-safe' 0 'passed'

run 'ALTER TABLE trades ADD COLUMN venue text NOT NULL; -- no DEFAULT yet' ''
expect 'a trailing comment naming DEFAULT does not clear a bare NOT NULL column' 1 'not-null'

# 4. Baseline grandfathering + shrink-only.
run 'ALTER TABLE trades DROP COLUMN legacy_price;' '0001_fixture.up.sql:drop-column'
expect 'baselined violation passes' 0 'passed'

run 'ALTER TABLE trades DROP COLUMN legacy_price;' '0001_fixture.up.sql:drop-table'
expect 'baseline entry for a different class does not grandfather' 1 'drop-column'

run 'CREATE TABLE t (id bigint);' '0099_gone.up.sql:drop-table'
expect 'stale baseline entry fails in repo mode' 1 'stale baseline entry'

run 'CREATE TABLE t (id bigint);' '0099_gone.up.sql:some-later-class'
expect 'an entry for a class this copy does not know is not stale' 0 'passed'

run 'CREATE TABLE t (id bigint);' '0099_gone.up.sql:drop-table' --staged
expect 'stale baseline entry tolerated under --staged' 0 'passed'

# 5. New violations still fail under --staged (the deploy-time gate).
run 'ALTER TABLE trades DROP COLUMN legacy_price;' '' --staged
expect 'staged mode still blocks a new violation' 1 'drop-column'

# 6. `dml`: unguarded DELETE/TRUNCATE/UPDATE, and its
#    file-level RAISE EXCEPTION escape (stronger than the inline marker
#    — see escape hatch 3 in lint-migration-compat.sh's header).
run 'DELETE FROM comet_liquidity;' ''
expect 'dml (bare DELETE) fires' 1 'dml'

run 'TRUNCATE comet_liquidity;' ''
expect 'dml (TRUNCATE) fires' 1 'dml'

run 'UPDATE comet_liquidity SET event_index = 0;' ''
expect 'dml (bare UPDATE) fires' 1 'dml'

run 'DELETE FROM comet_liquidity;  -- migration-compat:ok replay repairs it' ''
expect 'inline marker alone does not clear dml (needs a RAISE EXCEPTION guard)' 1 'dml'

run 'DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) FROM comet_liquidity INTO n;
    IF n > 100000 THEN
        RAISE EXCEPTION '"'"'refusing to delete % rows'"'"', n;
    END IF;
    DELETE FROM comet_liquidity;
END $$;' ''
expect 'RAISE EXCEPTION guard clears dml (no inline marker needed)' 0 'passed'

run 'DELETE FROM comet_liquidity;' '0001_fixture.up.sql:dml'
expect 'baselined dml entry passes' 0 'passed'

run '-- a real guard would RAISE EXCEPTION on a large table
DELETE FROM comet_liquidity;' ''
expect 'a comment naming RAISE EXCEPTION is not a guard' 1 'dml'

run 'CREATE FUNCTION refuse() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '"'"'no'"'"';
END
$$;
DELETE FROM comet_liquidity;' ''
expect 'a RAISE EXCEPTION inside a function body is not a guard' 1 'dml'

run 'CREATE FUNCTION forget(p text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM marks WHERE k = p;
    TRUNCATE marks_seeded;
END
$$;' ''
expect 'DML inside a CREATE FUNCTION body is not migrate-time dml' 0 'passed'

run 'CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $$ SELECT 1 $$;
DELETE FROM marks;' ''
expect 'DML after a one-line function body still fires' 1 'dml'

run '-- unlike a CREATE FUNCTION body, this runs now
DO $$
BEGIN
    DELETE FROM marks;
END $$;' ''
expect 'a comment naming CREATE FUNCTION does not hide a DO block' 1 'dml'

# shellcheck disable=SC2016 # $fn$ is SQL dollar quoting
run 'CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $fn$ SELECT 1 $fn$;
DO $$
BEGIN
    DELETE FROM marks;
END $$;' ''
expect 'a tag-quoted function does not hide a later DO block' 1 'dml'

echo
echo "lint-migration-compat-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
