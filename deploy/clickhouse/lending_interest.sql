-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). A FRESH host gets lending_interest_hourly from
-- deploy/clickhouse/tier1_schema.sql, which declares it identically —
-- scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- lending_interest_hourly holds, per Blend/BLNT pool reserve and hour, the
-- interest accrued between consecutive ResData versions, summed exactly in
-- the reserve's base units. Written by `stellarindex-ops ch-entry-history
-- -lending-only -write` after its lending_state pass, resuming from
-- entry_history_watermark name 'lending_interest' and never past
-- 'lending_state'. Until -interest-init has run the pass only logs, so the
-- table stays empty and the 10-minute timer never starts a backfill; once
-- initialised, the timer's pass catches up at most one 100,000-ledger window
-- and logs "backfill incomplete" beyond that. Nothing serves it yet.
--
-- Reader contract: a reader clips every range to the shared
-- 'lending_interest' watermark AND to each pool's 'lending_interest:<pool>'
-- coverage row; a pool added after the watermark has no rows below its own.
--
-- Runbook:
--   1. Apply this file.
--   2. Dry run one window (reads, folds and checks; writes nothing):
--      stellarindex-ops ch-entry-history -lending-only -interest-only
--        -interest-init -interest-max-windows 1
--   3. Backfill in chunks under the heavy-job wrapper, the first with
--      -interest-init, then without it until the run reports the tip:
--      run-heavy-job.sh lending-interest stellarindex-ops ch-entry-history
--        -lending-only -interest-only [-interest-init]
--        -interest-max-windows 20 -write
--      Between chunks let one lending-state.service run complete without
--      "skipped, an interest run is reading lending_state" (journalctl -u
--      lending-state.service); if state stops advancing,
--      stellarindex_lending_state_lagging fires: pause the chunks.
--   4. From then on lending-state.timer's run extends it every 10 minutes.
--      Interest runs take their own lock (ch-entry-history-lending-interest.lock
--      beside -lock-file) and hold -lock-file shared, so lending_state cannot
--      change under one; while a chunk runs, the timer's state and interest
--      passes skip, and a lending_state replay (-from) refuses.
--   5. Prove the backfill (reads only; exit 0 only with no mismatch and no
--      INCOMPLETE hour), then set lending_interest_prove_enabled for the host
--      so lending-interest-prove.timer repeats it daily over the last week
--      (-prove-recent 120960) and lending-interest-prove-full.timer weekly
--      over the whole range:
--      stellarindex-ops ch-entry-history -lending-only -prove -interest
--        [-from {lo} | -prove-recent {ledgers}] [-to {hi}] [-interest-pool {pool}]
--      It holds -lock-file and the interest lock shared (waiting up to 15
--      minutes for a writer), and reports the hours under a pending redo, or
--      past a seed the lake does not prove, INCOMPLETE.
--
-- Seeds come from lending_state_hourly and are proved against the lake: a
-- seed that is not its key's last stored version before the run (rows a
-- lending_state replay deleted and has not rewritten) refuses the run. After
-- any lending_state replay from {lo}, re-derive from {lo} THROUGH THE TIP: the
-- replay arms that redo, and until a re-derive reaches the watermark the
-- timer's pass skips and every other run refuses (-interest-pool excepted):
--   ch-entry-history -lending-only -interest-only -from {lo} -write
-- in chunks if need be, each from where the last stopped.
--
-- A window is refused, and the watermark holds, if a pool runs a WASM hash
-- not listed in lendingInterestWASM, or if the fold's rows disagree with
-- lending_state_hourly (backstop credit, hours, ledgers, BLNT fee carry).
-- Re-derive (ingest-pipeline.md, the replay decision rule):
--   ch-entry-history -lending-only -interest-only -from {lo} [-to {hi}] -write
--
-- Residual classes. Each unreconciled version is explained (a same-transaction
-- event of the pool states its backstop_credit change exactly: an interest
-- fill's lot, a gulp's token_delta, BLNT reconcile_loss's backstop_credit_loss),
-- attributed (a Blend V1 interest fill, whose event carries no lot, beside a
-- draw) or unexplained; unexplained = backstop_residual − residual_explained −
-- residual_attributed, and versions likewise. A version whose b_rate or fee
-- entry also moved stays unexplained. The pass reads the pools' contract_events
-- for each window and refuses it unless every ledger holding a version it
-- classifies by those events has, in contract_events, exactly the events
-- stellar.ledgers declares. A pool's cause event that does not decode is
-- logged and counted (stellarindex_lending_interest_undecodable_events on the
-- run's heartbeat), and every residual of its transaction stays unexplained.
-- Rows of an older derive_rev prove INCOMPLETE: after deploying a revision,
-- re-derive from the floor (entry_history_watermark name
-- 'lending_interest_floor') through the tip, in chunks if need be:
--   run-heavy-job.sh lending-interest stellarindex-ops ch-entry-history
--     -lending-only -interest-only -from {floor} -interest-max-windows 20 -write
-- then the same with -from L+1, L the ledger of the run's "stopping after N
-- windows at L" line, until a run reaches the tip without that line.
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
