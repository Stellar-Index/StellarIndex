-- 0241 up — record when a completeness verdict rests on the operator's
-- declaration that this network has none of a source.
--
-- On a quiet test network a source with no activity at all (sdex on a
-- fresh futurenet: no trade ever) can never earn a projection claim:
-- compute-completeness refuses an empty served tier matching an empty
-- expectation as "no evidence" (ADR-0033), which is right on pubnet and
-- leaves the source red forever there. `compute-completeness
-- -absent-by-design <source,...>` lets the host declare it; the run then
-- reads the lake over the source's whole [genesis, tip] and accepts the
-- empty projection only when that read finds no row.
--
--   absent_by_design — this verdict's projection_ok rests on that
--     declaration and that zero-row lake read. /v1/coverage serves it so a
--     consumer can tell "verified empty by declaration" from "verified".
--   absent_declared_but_present — the source is declared absent, yet the
--     lake or the served tier holds its rows: the declaration was ignored
--     and is wrong for this network. data-freshness.sh exports it as
--     stellarindex_data_freshness_absent_declared_but_present.
--
-- Lock: ADD COLUMN with a constant DEFAULT is catalog-only on PG 11+ (no
-- table rewrite, no scan); it takes ACCESS EXCLUSIVE on this small table
-- (one row per source) for the catalog update only. lock_timeout bounds
-- the wait behind a long reader instead of queueing every writer.
--
-- Old-binary-safe (rule 9): the previous binary's upsert does not name
-- these columns, so its writes leave them as they are. Every row starts
-- false, and only a binary that knows the declaration sets them true.
-- No back-fill and no replay: the next compute-completeness run writes
-- both.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE completeness_snapshots
    ADD COLUMN IF NOT EXISTS absent_by_design boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS absent_declared_but_present boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN completeness_snapshots.absent_by_design IS
    'projection_ok rests on the operator declaring this network has none of '
    'the source (compute-completeness -absent-by-design) and a lake read over '
    '[genesis_ledger, tip_ledger] that found no row. false = an ordinary verdict.';

COMMENT ON COLUMN completeness_snapshots.absent_declared_but_present IS
    'The source is declared absent by design, yet the lake or the served tier '
    'holds its rows: the declaration was ignored and is wrong for this network.';

COMMIT;
