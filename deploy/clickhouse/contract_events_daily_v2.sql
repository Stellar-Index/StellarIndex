-- contract_events_daily uniqExact → uniqCombined(17) rebuild (2026-07-09
-- incident). Full design + reader evidence + measured numbers:
--   docs/architecture/contract-events-daily-redesign.md
-- Exact r1 apply sequence (step-by-step, with verification queries):
--   docs/architecture/contract-events-daily-redesign.md § Procedure
--
-- This file is NOT auto-applied by any bootstrap and is NOT idempotent
-- re-run tooling — it is the operator-run migration artifact for an
-- EXISTING deployment (r1) whose `stellar.contract_events_daily` already
-- exists in the old uniqExact shape (`CREATE TABLE IF NOT EXISTS` in
-- tier1_schema.sql is a no-op against it). A FRESH deployment doesn't
-- need this file at all — tier1_schema.sql's canonical
-- `contract_events_daily` definition already uses uniqCombined(17).
--
-- Why a side-by-side v2 table+MV instead of an in-place fix: an
-- AggregateFunction column's on-disk state format is tied to its
-- declared function+parameters. uniqExact and uniqCombined(17) states
-- are different binary formats — there is no ALTER TABLE ... MODIFY
-- COLUMN path between them (same reason the earlier t0_xdr addition,
-- which only added a column, still needed a full recreate: t0_xdr sits
-- in the ORDER BY). Building v2 alongside the live v1 table means the
-- fast path (DailyActivityAvailable) never goes down during the
-- migration — v1 keeps serving reads with zero interruption while v2
-- backfills, and the cutover at the end is a few milliseconds of DDL,
-- not a data-copying window.
--
-- ── Step 1 of the runbook: create v2 (this immediately starts capturing
-- LIVE contract_events inserts going forward — the historical backfill
-- below only needs to cover ledger_seq up to the moment this MV was
-- created) ──────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS stellar.contract_events_daily_v2
(
    day          Date,
    contract_id  String,
    event_type   LowCardinality(String),
    topic_0_sym  LowCardinality(String),
    t1_xdr       String,
    t0_xdr       String,
    events       AggregateFunction(uniqCombined(17), UInt32, String, UInt32, UInt32)
)
ENGINE = AggregatingMergeTree
ORDER BY (contract_id, day, event_type, topic_0_sym, t1_xdr, t0_xdr);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_events_daily_v2_mv
TO stellar.contract_events_daily_v2 AS
SELECT
    toDate(close_time) AS day,
    contract_id,
    event_type,
    topic_0_sym,
    if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
    if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
    uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index) AS events
FROM stellar.contract_events
GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr;

-- ── Step 2: windowed historical backfill (run under run-heavy-job.sh,
-- one window at a time — see the runbook for the exact loop). Bound
-- every window by ledger_seq (contract_events is PARTITION BY
-- intDiv(ledger_seq,1000000), so a bounded window prunes partitions
-- instead of scanning the full ~12B-row table) and cap the upper bound
-- at the ledger_seq that was live at v2-MV-creation time (see the
-- runbook for how to read that back from system.tables) — no need to
-- re-cover ledgers the v2 MV already captured live, though doing so is
-- SAFE (uniqCombinedMerge is a set-union merge: re-inserting an
-- overlapping/duplicate window does not inflate the estimate — verified
-- against a live container while writing this runbook). Example window:
--
--   INSERT INTO stellar.contract_events_daily_v2
--   SELECT toDate(close_time) AS day, contract_id, event_type,
--          topic_0_sym, if(topic_0_sym = '', topics_xdr[2], '') AS t1_xdr,
--          if(topic_0_sym = '', topics_xdr[1], '') AS t0_xdr,
--          uniqCombinedState(17)(ledger_seq, tx_hash, op_index, event_index)
--   FROM stellar.contract_events
--   WHERE ledger_seq >= {window_start} AND ledger_seq < {window_end}
--   GROUP BY day, contract_id, event_type, topic_0_sym, t1_xdr, t0_xdr;
--
-- ── Step 3: verify v2 against v1 (spot-check a handful of hot
-- contract_id/day pairs — expect v2 within ~0.5% of v1's exact count):
--
--   SELECT v1.contract_id, v1.day, v1.c AS v1_exact, v2.c AS v2_approx
--   FROM (SELECT contract_id, day, uniqExactMerge(events) AS c
--         FROM stellar.contract_events_daily GROUP BY contract_id, day) v1
--   JOIN (SELECT contract_id, day, uniqCombinedMerge(17)(events) AS c
--         FROM stellar.contract_events_daily_v2 GROUP BY contract_id, day) v2
--     USING (contract_id, day)
--   ORDER BY v1_exact DESC LIMIT 20;
--
-- ── Step 4: cutover (see the runbook for the FULL sequence — capturing
-- the pre-cutover ledger_seq tip, the exact DROP/RENAME/CREATE ordering,
-- and the post-cutover gap-closing catch-up insert). Short version: drop
-- both MVs (a renamed table does NOT drag its MV's stored target
-- reference along — verified; the MV would error INSERTs with "Target
-- table ... doesn't exist" otherwise), atomically double-RENAME
-- (v1 → _old, v2 → canonical), recreate the MV under the canonical
-- name/target, then run ONE small overlapping catch-up backfill for the
-- brief DDL gap.
--
-- ── Step 5: DROP TABLE stellar.contract_events_daily_old SYNC — and only
-- now is it safe to consider the incident's `max_bytes_to_merge_at_max_-
-- space_in_pool=1` merge-park (applied to the OLD table only) moot: it
-- goes away with the table. The new canonical table was never parked
-- (a fresh CREATE TABLE has default merge settings) — merges resume
-- automatically; watch `system.parts` part-count trend down over the
-- following merge cycles to confirm.
