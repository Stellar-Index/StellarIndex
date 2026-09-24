-- 0088 down — drop the pre-Soroban genesis-baseline columns.
--
-- Revert the code FIRST; this down is not safe under a binary that reads
-- the columns. The reader treats a NULL genesis_baseline_ledger as "not
-- seeded", but a dropped column is not NULL: sep41RollupCheckpoint's SELECT
-- names every genesis_* column, so with them gone it fails with 42703
-- (undefined_column) and every SEP-41 supply read errors, as do
-- SEP41GenesisBaselineSeeded and UpsertSEP41GenesisBaseline. Under a binary
-- predating 0088 the drop restores the Soroban-era-only read path, and the
-- negative-total guard reverts to reporting a range-scoped-missing baseline
-- the same as a genuine inconsistency, which is the pre-0088 behaviour.
ALTER TABLE sep41_supply_rollup
    DROP COLUMN IF EXISTS genesis_mint_total,
    DROP COLUMN IF EXISTS genesis_burn_total,
    DROP COLUMN IF EXISTS genesis_clawback_total,
    DROP COLUMN IF EXISTS genesis_baseline_ledger,
    DROP COLUMN IF EXISTS genesis_seeded_at;
