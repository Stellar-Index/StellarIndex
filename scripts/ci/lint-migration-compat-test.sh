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

run 'UPDATE trades
   SET usd_volume = NULL
 WHERE ts < now();' ''
expect 'dml (UPDATE with SET on the next line) fires' 1 'dml'

run 'UPDATE classic_assets ca SET slug = lower(ca.code);' ''
expect 'dml (aliased UPDATE) fires' 1 'dml'

run 'UPDATE ONLY trades SET usd_volume = NULL;' ''
expect 'dml (UPDATE ONLY) fires' 1 'dml'

run 'DELETE
  FROM trades WHERE ts < now();' ''
expect 'dml (DELETE with FROM on the next line) fires' 1 'dml'

run 'DELETE FROM trades WHERE ts < now(); -- no RAISE EXCEPTION guard needed' ''
expect 'a trailing comment naming RAISE EXCEPTION is not a guard' 1 'dml'

run '/* we deliberately do not
   RAISE EXCEPTION here */
UPDATE trades SET usd_volume = 0 WHERE ts < now();' ''
expect 'a block comment naming RAISE EXCEPTION is not a guard' 1 'dml'

# shellcheck disable=SC2016  # $fn$ is SQL dollar quoting, not a shell expansion
run 'CREATE FUNCTION refuse() RETURNS void LANGUAGE plpgsql AS $fn$
BEGIN
    RAISE EXCEPTION '"'"'no'"'"';
END
$fn$;
UPDATE trades SET usd_volume = 0 WHERE ts < now();' ''
expect 'a RAISE EXCEPTION inside a tag-quoted function body is not a guard' 1 'dml'

# DML that does not open its line is still a statement.
# shellcheck disable=SC2016  # $$ is SQL dollar quoting, not a shell expansion
run 'DO $$ BEGIN DELETE FROM trades; END $$;' ''
expect 'dml inside a one-line DO block fires' 1 'dml'
run 'WITH gone AS (DELETE FROM trades RETURNING 1) SELECT count(*) FROM gone;' ''
expect 'dml in a data-modifying CTE fires' 1 'dml'
run "SET LOCAL lock_timeout = '5s'; UPDATE trades SET price = 0;" ''
expect 'dml after another statement on the same line fires' 1 'dml'
run 'MERGE INTO trades t USING fix f ON t.id = f.id WHEN MATCHED THEN DELETE;' ''
expect 'MERGE fires as dml' 1 'dml'
run "COMMENT ON TABLE audit_log IS 'TRUNCATE of audit_log is forbidden';" ''
expect 'TRUNCATE in a COMMENT ON string stays quiet' 0 'passed'

# A held UPDATE must end at its real `;`, not swallow the next statement.
# shellcheck disable=SC2016
guard='DO $$ BEGIN IF (SELECT count(*) FROM t) > 0 THEN RAISE EXCEPTION '"'"'x'"'"'; END IF; END $$;'
run "$guard
UPDATE t SET slug = replace(slug, '--', '-');
DROP TRIGGER t_guard ON t;" ''
expect "a '--' inside a string does not hide the next DROP" 1 'drop-other'
run "$guard
UPDATE t SET slug = lower(slug); /* normalise */
REVOKE SELECT ON trades FROM stellarindex_api;" ''
expect 'a trailing block comment does not hide the next REVOKE' 1 'revoke'

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

run 'CREATE OR REPLACE PROCEDURE refuse(job_id int, config jsonb) LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '"'"'no'"'"';
END
$$;
DELETE FROM comet_liquidity;' ''
expect 'a RAISE EXCEPTION inside a procedure body is not a guard' 1 'dml'

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

# A statement split over lines is still one statement.
run 'ALTER TABLE trades
    DROP COLUMN legacy_price;' ''
expect 'a multi-line ALTER fires' 1 'drop-column'

run 'ALTER TABLE trades
    ADD COLUMN note text,
    ALTER COLUMN source SET NOT NULL;' ''
expect 'a later clause of a multi-line ALTER fires' 1 'set-not-null'

run 'ALTER TABLE trades
    DROP COLUMN legacy_price;  -- migration-compat:ok added in 0104, no released binary reads it' ''
expect 'a marker on a continuation line clears the statement' 0 'passed'

run 'ALTER TABLE trades
    -- we used to DROP COLUMN legacy_price here
    ADD COLUMN note text;' ''
expect 'a comment inside a multi-line ALTER is not the statement' 0 'passed'

run 'ALTER TABLE trades
    ADD COLUMN note text;
ALTER TABLE hooks ADD COLUMN x int;
DROP TABLE classic_movements;' ''
expect 'joining stops at the semicolon' 1 'drop-table'

# A function the file calls runs at migrate time.
run 'CREATE FUNCTION pg_temp.purge() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM trades;
END $$;
SELECT pg_temp.purge();' ''
expect 'DML in a function the file calls fires' 1 'dml'

run 'CREATE OR REPLACE FUNCTION purge() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    ALTER TABLE trades DROP COLUMN usd_volume;
END $$;
DO $$ BEGIN PERFORM purge(); END $$;' ''
expect 'DDL in a function a DO block performs fires' 1 'drop-column'

run 'CREATE FUNCTION purge() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM trades;
END $$;
-- SELECT purge(); is run by the operator
CREATE TRIGGER t AFTER INSERT ON marks EXECUTE FUNCTION purge();' ''
expect 'a function only named in a comment or a trigger is not called' 0 'passed'

# Tightening with no DDL keyword the older classes know.
run 'REVOKE SELECT ON trades FROM stellarindex_api;' ''
expect 'revoke fires' 1 'revoke'

run 'REVOKE SELECT ON trades FROM stellarindex_api; -- migration-compat:ok role created in this release' ''
expect 'revoke with a marker and reason passes' 0 'passed'

run 'CREATE TRIGGER marks_guard
    BEFORE INSERT OR UPDATE ON marks
    FOR EACH ROW EXECUTE FUNCTION refuse_negative();' ''
expect 'a BEFORE trigger fires' 1 'before-trigger'

run 'CREATE TRIGGER marks_track
    AFTER INSERT ON marks
    FOR EACH ROW EXECUTE FUNCTION track();' ''
expect 'an AFTER trigger is not flagged' 0 'passed'

# DROP forms and row-deleting calls the named classes do not spell.
run 'DROP TRIGGER IF EXISTS trustline_balance_current_trg ON trustline_balance_observations;' ''
expect 'DROP TRIGGER fires' 1 'drop-other'
run 'DROP SCHEMA legacy CASCADE;' ''
expect 'DROP SCHEMA fires' 1 'drop-other'
run 'DROP SEQUENCE IF EXISTS trades_id_seq CASCADE;' ''
expect 'DROP SEQUENCE fires' 1 'drop-other'
run 'DROP OWNED BY stellarindex_api;' ''
expect 'DROP OWNED fires' 1 'drop-other'
run 'ALTER TABLE trades DISABLE TRIGGER ALL;' ''
expect 'DISABLE TRIGGER fires' 1 'drop-other'
run 'ALTER TABLE trades OWNER TO someone_else;' ''
expect 'OWNER TO fires' 1 'owner'
run "SELECT drop_""chunks('trades', older_than => now() - INTERVAL '30 days');" ''
expect 'drop_chunks fires as dml' 1 'dml'
dc="drop_""chunks"
run "DO \$\$ BEGIN
    PERFORM ${dc}('trades', older_than => now());
END \$\$;" ''
expect 'PERFORM drop_chunks in a DO block fires as dml' 1 'dml'
run "SELECT count(*) FROM public.${dc}('trades', older_than => now());" ''
expect 'SELECT … FROM drop_chunks fires as dml' 1 'dml'
run 'DROP TABLE classic_movements;' ''
expect 'DROP TABLE is one class, not also drop-other' 1 'drop-table'
case "$OUT" in *drop-other*) echo "FAIL: DROP TABLE double-flagged"; fail=$((fail+1)) ;; esac

# An EXCLUDE or a marker applies to its own clause, not to the whole ALTER.
run 'ALTER TABLE trades
    ADD COLUMN venue_id integer NOT NULL,
    ADD COLUMN note text DEFAULT '"'x'"';' ''
expect 'a DEFAULT on another clause does not clear add-not-null-column' 1 'add-not-null-column'

run 'ALTER TABLE trades
    DROP CONSTRAINT IF EXISTS old_ck, -- migration-compat:ok old_ck was never relied on
    ALTER COLUMN price TYPE double precision,
    DROP COLUMN usd_volume;' ''
expect 'a marker on one clause leaves the others flagged' 1 'alter-type'

echo
echo "lint-migration-compat-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
