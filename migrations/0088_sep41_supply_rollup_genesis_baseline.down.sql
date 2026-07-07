-- 0088 down — drop the pre-Soroban genesis-baseline columns.
--
-- Correctness-safe: the reader treats a NULL genesis_baseline_ledger as "not
-- seeded" and adds no genesis contribution, so removing the columns only
-- restores the (Soroban-era-only) pre-0088 read path. The negative-total guard
-- reverts to reporting a range-scoped-missing baseline the same as a genuine
-- inconsistency, which is the pre-fix behaviour.
ALTER TABLE sep41_supply_rollup
    DROP COLUMN IF EXISTS genesis_mint_total,
    DROP COLUMN IF EXISTS genesis_burn_total,
    DROP COLUMN IF EXISTS genesis_clawback_total,
    DROP COLUMN IF EXISTS genesis_baseline_ledger,
    DROP COLUMN IF EXISTS genesis_seeded_at;
