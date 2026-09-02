-- 0152 up — drop six never-wired scaffold tables and the four orphaned
-- Stripe columns (#358 items 2-6, #357 F8).
--
-- These six tables were created in 2026-04 from the explorer data
-- inventory as the schema half of capabilities whose WRITER was never
-- built. Every one of them has been empty in every environment since
-- the day it was created, and each is a standing lie to anyone reading
-- the schema: `\dt` shows a table, so a reader assumes the capability
-- exists and the data is merely sparse.
--
--   wasm_versions           0017  — the WASM store shipped over the
--   contract_wasm_history   0017    ClickHouse lake instead
--                                   (internal/storage/clickhouse/
--                                   wasm_lake_reader.go). Never a Go
--                                   writer or reader.
--   tvl_observations        0021  — the per-protocol TVL ticker
--                                   (inventory §9.5, plan step 3.1) was
--                                   never built.
--   anchors                 0023  — SEP-1 anchor aggregation is served
--                                   from internal/metadata; nothing has
--                                   ever selected from or inserted into
--                                   this table.
--   classic_asset_stats_5m  0024  — the /v1/coins reader was moved to
--                                   the prices_1m UNION CTE in 2f06533a
--                                   (2026-05-06) precisely BECAUSE this
--                                   table is empty; asset_catalogue.go
--                                   :464 records that. No writer ever
--                                   existed.
--   aggregator_exposures    0025  — the DeFindex exposure ticker
--                                   (inventory §9.9, plan step 3.5) was
--                                   never built.
--
-- And on stripe_event_log, whose WRITERS were deleted in d2185560
-- (ADR-0049) leaving the schema describing a dead-letter/claim protocol
-- nothing implements: the four columns 0118 + 0121 added and their two
-- partial indexes. The TABLE stays — 0027 creates it and this migration
-- is not the place to break 0027's rollback symmetry — but it is
-- tombstoned with a comment so the next reader does not go looking for
-- the reconciliation worker.
--
-- Evidence this is safe (re-derived at HEAD 14626cd7, 2026-09-02):
--
--   * ZERO Go readers and ZERO Go writers, at HEAD and in the RELEASED
--     v0.57.0 tree. `git grep -E '<the five table names>' v0.57.0 --
--     '*.go'` returns three hits, all of them `//` comments
--     (external/registry.go:83, asset_catalogue.go:464 + :2402);
--     `anchors` matches only the English word. `stripe_event_log` and
--     the four column names return NOTHING in either tree. So the
--     previous released binary cannot be affected — that is the rule-9
--     `migration-compat:ok` justification on each statement below.
--   * r1 read-only pre-flight, 2026-09-02: all seven tables present,
--     `count(*) = 0` on every one; stripe_event_log has 0 rows and 0
--     non-NULL values in all four columns.
--
-- Fail-closed guard. A fork or a test net could have populated one of
-- these where our fleet did not, and a silent `DROP TABLE` would
-- destroy that data with no trace. The DO block below REFUSES the whole
-- migration if any of the six holds a row — loud, not silent, the same
-- stance 0092/0148's downs take.
--
-- ⚠ RECOVERY IS TWO STEPS, not one. The exception rolls the migration
-- back (nothing is dropped), but golang-migrate marks version 152
-- DIRTY, and it will not run anything else until that is cleared —
-- verified against TimescaleDB 2.26.4-pg15 on 2026-09-02:
--
--     stellarindex-migrate force 151       # clear the dirty flag
--     <inspect the named table; keep what matters; empty it>
--     stellarindex-migrate up              # re-run 0152
--
-- The exception message names every offending table and its row count,
-- so the middle step is unambiguous.
--
-- The DOWN re-creates every table with its ORIGINAL DDL (0017/0021/
-- 0023/0024/0025) and re-adds the Stripe columns/indexes with their
-- 0118/0121 comments, so `TestMigrationsRoundTrip` and a
-- down-then-up cycle both land back on the same schema.
--
-- Sibling edits that ship WITH this migration (both enumerate these
-- table names and would otherwise break):
--   scripts/ops/add-missing-compression-policies.sql — `ON_ERROR_STOP`
--     + `add_compression_policy` on a dropped table aborts the whole
--     script, so the tables AFTER it silently never get a policy.
--   scripts/ops/config-assertions.sh — `compression_policies_applied`
--     counts want-list tables with no policy job; a dropped table has
--     none, so the assertion would fail forever on r1.

BEGIN;

-- Refuse rather than destroy: these are supposed to be empty everywhere.
DO $$
DECLARE
    t   text;
    n   bigint;
    bad text := '';
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'wasm_versions', 'contract_wasm_history', 'tvl_observations',
        'anchors', 'classic_asset_stats_5m', 'aggregator_exposures'
    ] LOOP
        EXECUTE format('SELECT count(*) FROM %I', t) INTO n;
        IF n > 0 THEN
            bad := bad || format('%s=%s ', t, n);
        END IF;
    END LOOP;
    IF bad <> '' THEN
        RAISE EXCEPTION '0152_drop_unwired_scaffold_tables.up.sql: refusing to drop a NON-EMPTY scaffold table (%) — these have no Go writer in any released binary, so rows here came from somewhere this migration does not know about. Inspect them, keep what matters, empty the table, then re-run. Dropping data silently is not something a schema-tidy migration gets to do (#358).', trim(bad);
    END IF;
END $$;

-- Child first: contract_wasm_history REFERENCES wasm_versions(wasm_hash).
DROP TABLE IF EXISTS contract_wasm_history;  -- migration-compat:ok never referenced by any Go code, incl. released v0.57.0; 0 rows on r1
DROP TABLE IF EXISTS wasm_versions;          -- migration-compat:ok never referenced by any Go code, incl. released v0.57.0; 0 rows on r1
DROP TABLE IF EXISTS tvl_observations;       -- migration-compat:ok never referenced by any Go code, incl. released v0.57.0; 0 rows on r1
DROP TABLE IF EXISTS anchors;                -- migration-compat:ok never referenced by any Go code, incl. released v0.57.0; 0 rows on r1
DROP TABLE IF EXISTS classic_asset_stats_5m; -- migration-compat:ok last reader removed in 2f06533a (2026-05-06); no reference in released v0.57.0; 0 rows on r1
DROP TABLE IF EXISTS aggregator_exposures;   -- migration-compat:ok never referenced by any Go code, incl. released v0.57.0; 0 rows on r1

-- ─── stripe_event_log: drop the orphaned dead-letter + claim shape ───

DROP INDEX IF EXISTS stripe_event_log_open_dead_letters_idx;
DROP INDEX IF EXISTS stripe_event_log_claimed_idx;

ALTER TABLE stripe_event_log
    DROP COLUMN IF EXISTS dead_lettered_at,        -- migration-compat:ok writers deleted in d2185560; no reference in HEAD or released v0.57.0; 0 non-NULL on r1
    DROP COLUMN IF EXISTS dead_letter_reason,      -- migration-compat:ok writers deleted in d2185560; no reference in HEAD or released v0.57.0; 0 non-NULL on r1
    DROP COLUMN IF EXISTS dead_letter_resolved_at, -- migration-compat:ok writers deleted in d2185560; no reference in HEAD or released v0.57.0; 0 non-NULL on r1
    DROP COLUMN IF EXISTS claimed_at;              -- migration-compat:ok writers deleted in d2185560; no reference in HEAD or released v0.57.0; 0 non-NULL on r1

COMMENT ON TABLE stripe_event_log IS
    'INERT. The Stripe webhook writers were deleted in d2185560 (ADR-0049); '
    'nothing in Go reads or writes this table today. Retained only for 0027 '
    'rollback symmetry. Migration 0152 removed the dead-letter (0118) and '
    'claim (0121) columns + their partial indexes, which described a '
    'reconciliation protocol no code implements. If billing is revived, add '
    'the shape back in the migration that adds the writer.';

COMMIT;
