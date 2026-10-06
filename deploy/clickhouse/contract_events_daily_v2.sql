-- si-apply-scope: operator
-- si-cutover-object: stellar.contract_events_daily_v2
-- si-cutover-object: stellar.contract_events_daily_v2_mv
--
-- NOT applied by any bootstrap. The objects named above are the CUT-OVER
-- halves of an r1 migration: they exist only for its duration and are
-- renamed onto stellar.contract_events_daily (and the v2 names dropped)
-- when it completes. That is why they are deliberately ABSENT from
-- tier1_schema.sql — codifying them would make a COMPLETED cut-over read as
-- schema drift forever. si-cutover-object is what exempts them from the
-- lint's "every operator-created object is also declared fresh-host" rule.
--
-- HISTORY — this file DID auto-apply until 2026-09-09. The archival-node
-- role executed every deploy/clickhouse/*.sql it could glob wherever
-- clickhouse_apply_schema is true (testnet.yml, futurenet.yml), so the
-- paragraph below was false on those hosts and every fresh test-net
-- provision built the v2 pair as an exact duplicate of
-- stellar.contract_events_daily: same column list and order, same
-- AggregatingMergeTree, same ORDER BY, an MV reading the same
-- stellar.contract_events with the same SELECT into a second target. The
-- live test nets still carry them; the drop is an operator step, not
-- something this file or the role does.
--
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- contract_events_daily uniqExact → uniqCombined(17) rebuild (2026-07-09
-- incident).
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
-- one window at a time, 1M-ledger windows from 2 to <swap_ledger>, each
-- wrapped as `run-heavy-job.sh contract-events-daily-v2-backfill
-- clickhouse-client --query ...`; resumable). Bound
-- every window by ledger_seq (contract_events is PARTITION BY
-- intDiv(ledger_seq,1000000), so a bounded window prunes partitions
-- instead of scanning the full ~12B-row table) and cap the upper bound
-- at the ledger_seq that was live at v2-MV-creation time (<swap_ledger>;
-- read it back from stellar.contract_events before step 1) — no need to
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
-- contract_id/day pairs — expect rel_err well under 1%, measured 0.1-0.5%;
-- v2's (contract_id, day) key set should be a superset of v1's):
--
--   SELECT v1.contract_id, v1.day, v1.c AS v1_exact, v2.c AS v2_approx,
--          abs(v2.c - v1.c) / v1.c AS rel_err
--   FROM (SELECT contract_id, day, uniqExactMerge(events) AS c
--         FROM stellar.contract_events_daily GROUP BY contract_id, day) v1
--   JOIN (SELECT contract_id, day, uniqCombinedMerge(17)(events) AS c
--         FROM stellar.contract_events_daily_v2 GROUP BY contract_id, day) v2
--     USING (contract_id, day)
--   ORDER BY v1_exact DESC LIMIT 20;
--
-- ── Step 4: cutover. Capture the ledger_seq tip first (<swap_ledger>).
-- Drop both MVs (a renamed table does NOT drag its MV's stored target
-- reference along — verified; the MV would error INSERTs with "Target
-- table ... doesn't exist" otherwise), atomically double-RENAME
-- (v1 → _old, v2 → canonical), recreate the MV under the canonical
-- name/target, then run ONE small overlapping catch-up backfill for the
-- brief DDL gap (safe if the gap was zero-width):
--
--   DROP VIEW stellar.contract_events_daily_v2_mv;
--   DROP VIEW stellar.contract_events_daily_mv;
--   RENAME TABLE
--     stellar.contract_events_daily TO stellar.contract_events_daily_old,
--     stellar.contract_events_daily_v2 TO stellar.contract_events_daily;
--   CREATE MATERIALIZED VIEW stellar.contract_events_daily_mv
--   TO stellar.contract_events_daily AS <the step-1 v2 MV SELECT>;
--   INSERT INTO stellar.contract_events_daily <the step-2 SELECT>
--   WHERE ledger_seq >= <swap_ledger> GROUP BY ...;
--
-- Then restart stellarindex-api (its DailyActivityAvailable probe is a
-- sync.Once) and confirm GET /v1/protocols/{name} still returns
-- event_breakdown.
--
-- Rollback: up to step 3, DROP TABLE contract_events_daily_v2_mv,
-- contract_events_daily_v2 (v1 never stopped serving). After step 4 and
-- before step 5, re-run the drop/rename/create in reverse (_old is
-- untouched data, so this is lossless).
--
-- ── Step 5: DROP TABLE stellar.contract_events_daily_old SYNC — and only
-- now is it safe to consider the incident's `max_bytes_to_merge_at_max_-
-- space_in_pool=1` merge-park (applied to the OLD table only) moot: it
-- goes away with the table. The new canonical table was never parked
-- (a fresh CREATE TABLE has default merge settings) — merges resume
-- automatically; watch `system.parts` part-count trend down over the
-- following merge cycles to confirm.
