-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). A FRESH host gets lending_interest_hourly from
-- deploy/clickhouse/tier1_schema.sql, which declares it identically —
-- scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- lending_interest_hourly holds, per Blend/BLNT pool reserve and hour, the
-- interest accrued between consecutive ResData versions, summed exactly in
-- the reserve's base units. Planned writer, not yet wired: `stellarindex-ops
-- ch-entry-history -lending-only -write`, resuming from entry_history_watermark
-- name 'lending_interest'. Until it lands the table stays empty.
--
-- Runbook:
--   1. Apply this file.
--
-- Amounts are Int256: each accrual is an i128, and sum(Int128) wraps silently,
-- so an hourly sum or a served sum over hours needs the headroom. Never float.
--
-- An hour's row is written only once the hour is complete, so ledgers never
-- compete for it: the newest derive wins (derived_at, ms so two runs in one
-- second cannot tie). Every aggregating read goes through FINAL.
--
-- A replay cannot remove an hour that a lake re-derive emptied. After a lake
-- re-derive of ledger_entry_changes over [lo, hi], delete the overlapping
-- hours first, wait for system.mutations, then replay and prove the range:
--   ALTER TABLE stellar.lending_interest_hourly
--     DELETE WHERE last_ledger >= {lo} AND first_ledger <= {hi};

CREATE TABLE IF NOT EXISTS stellar.lending_interest_hourly
(
    contract_id           String,                  -- pool C-strkey
    protocol              LowCardinality(String),  -- blend_v1 | blend_v2 | blnt: the arithmetic used
    asset                 String,                  -- reserve asset C-strkey
    hour                  DateTime('UTC'),
    first_ledger          UInt32,                  -- replay DELETEs key on first/last_ledger
    last_ledger           UInt32,
    versions              UInt32,                  -- ResData versions in the hour
    accrued_versions      UInt32,                  -- versions with a known predecessor
    gross                 Int256,
    protocol_fee          Int256,                  -- 0 on Blend
    backstop              Int256,
    supplier_intended     Int256,
    supplier_realised     Int256,
    backstop_credit_delta Int256,
    unreconciled_versions UInt32,                  -- any of the b_rate / backstop / fee checks failed
    brate_mismatches      UInt32,
    backstop_residual     Int256,
    supply_residual       Int256,
    fee_mismatches        UInt32,
    fee_credit_residual   Int256,
    residual_explained    Int256,                  -- amount-matched to an event
    residual_attributed   Int256,                  -- same-tx event whose amount cannot be checked
    explained_versions    UInt32,
    attributed_versions   UInt32,
    unordered_versions    UInt32,                  -- predecessor order unknown: counted, not summed
    accrual_errors        UInt32,                  -- accrual refused the pair: counted, not summed
    error_samples         Array(String),           -- at most 5
    derive_rev            UInt16,                  -- bumped on any change to the derive arithmetic
    derived_at            DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(derived_at)
PARTITION BY toYYYYMM(hour)
ORDER BY (contract_id, asset, hour);
