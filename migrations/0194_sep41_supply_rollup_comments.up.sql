-- 0194 up — re-issue sep41_supply_rollup's stored comments (GH #1086 remainder).
--
-- 0085's COMMENT ON TABLE / COLUMN text was written when the aggregator
-- rollup worker was the table's only writer and a full re-derive was told to
-- TRUNCATE it. Neither is true since a3dc3eb83/60431fce8 and the 0088 genesis
-- baseline: three writers share the fold columns, only the seed writes
-- genesis_*, and TRUNCATE would discard
-- the seeded genesis columns that no re-derive recreates. 0085's file header
-- was corrected in place, but pg_description on every database migrated
-- before that edit still carries the original text, which is what \d+ and the
-- schema docs show an operator deciding how to recover the table.
--
-- Metadata only: COMMENT takes a brief SHARE UPDATE EXCLUSIVE lock, rewrites
-- nothing, is idempotent and old-binary-safe (README rule 9).

COMMENT ON TABLE sep41_supply_rollup IS
    'Incremental per-contract mint/burn/clawback checkpoint for SEP-41 '
    'Algorithm-3 supply, read as rollup + sargable delta by '
    'SEP41KindTotalsAtOrBefore so the steady-state serving read sums only '
    'the tail above last_ledger, not a contract''s full '
    'sep41_supply_events history (incident 2026-07-06). Three writers; the '
    'genesis_* columns are the seed''s alone, the fold columns are shared: '
    'the aggregator rollup worker '
    '(Store.AdvanceSEP41SupplyRollup) folds mint_total / burn_total / '
    'clawback_total / last_ledger forward; `stellarindex-ops supply '
    'seed-sep41-genesis` (Store.UpsertSEP41GenesisBaseline) sets the '
    'genesis_* columns from the ClickHouse lake and rebuilds the fold '
    'beneath the new floor in the same transaction; '
    'Store.ResetSEP41SupplyRollupFold (`ch-rebuild -sep41 -write`, '
    '`projector-replay -source sep41_supply`) zeroes the fold columns when '
    'a re-derive or replay rewrites history below the checkpoint, so the '
    'worker re-folds over the corrected events as they settle. Never '
    'TRUNCATE or DELETE rows: that discards the genesis baseline, which no '
    're-derive recreates.';

COMMENT ON COLUMN sep41_supply_rollup.last_ledger IS
    'Highest SETTLED ledger folded into the fold columns; the reader adds '
    'the live delta above it up to the request ledger. 0 means nothing '
    'folded yet (a row created by a pass that found no settled events, or a '
    'reset by '
    'Store.ResetSEP41SupplyRollupFold after a re-derive): the reader then '
    'serves the exact full-history sum until the worker''s next pass '
    're-folds it — correct, but off the fast path.';
