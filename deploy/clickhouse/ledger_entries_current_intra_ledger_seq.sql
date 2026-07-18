-- ledger_entries_current version rebuild: ReplacingMergeTree(ledger_seq) →
-- ReplacingMergeTree(version), version = (ledger_seq << 32) | intra_ledger_seq
-- (audit-2026-07-16 C2-4c / CS-021 broadened). Full rationale + reader blast
-- radius: deploy/clickhouse/tier1_schema.sql (the ledger_entries_current DDL
-- header) and internal/storage/clickhouse/extract_entry_changes.go.
--
-- This file is NOT auto-applied by any bootstrap and is NOT idempotent re-run
-- tooling — it is the operator-run migration artifact for an EXISTING
-- deployment (r1) whose stellar.ledger_entries_current already exists in the
-- old ReplacingMergeTree(ledger_seq) shape (`CREATE TABLE IF NOT EXISTS` in
-- tier1_schema.sql is a no-op against it). A FRESH deployment does NOT need
-- this file — tier1_schema.sql's canonical DDL already uses the composite
-- version. It is the direct sibling of deploy/clickhouse/contract_events_daily_v2.sql
-- (that one changed an AggregateFunction state format; this one changes the
-- ReplacingMergeTree version column). ***FREEZE-GATED: do NOT run under the
-- current deploy freeze — this is the codified migration, executed by an
-- operator as a separate, post-freeze step.***
--
-- The defect: ledger_seq alone is not unique per key within a ledger — a single
-- ledger can hold several changes to one storage key (update-then-remove,
-- remove-then-recreate; change_index is only a per-TRANSACTION counter, so it
-- repeats across a ledger's txs — see extract_entry_changes.go). With ledger_seq
-- as the sole ReplacingMergeTree version those same-ledger rows tied, so FINAL
-- kept an ARBITRARY one — it could resurrect a deleted entry (a stale
-- before-image beating a later 'removed') or serve a mid-ledger state to
-- account_state / account_balance / soroswap_pair_state / the SAC seed. Folding
-- the per-ledger intra_ledger_seq (canonical within-ledger walk position) into
-- the version's low 32 bits makes it strictly monotonic in apply order, so FINAL
-- deterministically keeps the LAST change (including a removal).
--
-- Why a side-by-side v2 table+MV instead of an in-place fix: ClickHouse has no
-- ALTER TABLE ... MODIFY ENGINE — the ReplacingMergeTree version column is fixed
-- at CREATE and cannot be changed on an existing table (same constraint the
-- contract_events_daily uniqExact→uniqCombined rebuild hit). Building v2
-- alongside the live v1 means current-state reads (account_state / asset-holder
-- / SAC seed) keep serving with ZERO downtime while v2 reprojects; the cutover
-- at the end is a few milliseconds of DDL, not a data-copying window.
--
-- ***Heavy op.*** The Step-2 reproject rewrites the entire current-state
-- projection from the multi-billion-row ledger_entry_changes append-log. Run it
-- windowed, under run-heavy-job.sh on r1, off-peak (CLAUDE.md heavy-job
-- doctrine) — never in one statement.

-- ── Step 0: add intra_ledger_seq to the source append-log ────────────────────
-- Additive, DEFAULT 0 → old-binary-safe (the currently-deployed indexer's 12-col
-- INSERT keeps working; the post-fix 13-col INSERT populates it). Metadata-only
-- for existing parts — no rewrite. MUST precede Step 1: the v2 MV selects this
-- column. Existing rows read back 0 (same as pre-fix; same-ledger ties among
-- them stay unbroken until a full re-derive of ledger_entry_changes repopulates
-- intra_ledger_seq — a separate, even heavier op, tracked but NOT required for
-- the tie-break to be effective on all NEW ingest immediately).
ALTER TABLE stellar.ledger_entry_changes
    ADD COLUMN IF NOT EXISTS intra_ledger_seq UInt32 DEFAULT 0 AFTER balance;

-- ── Step 1: create v2 (this immediately starts capturing LIVE ledger_entry_changes
-- inserts going forward — the historical reproject below only needs to cover the
-- ledger_seq range that was live at MV-creation time) ────────────────────────
CREATE TABLE IF NOT EXISTS stellar.ledger_entries_current_v2
(
    entry_type  LowCardinality(String),
    key_xdr     String,
    account_id  String DEFAULT '',
    asset       String DEFAULT '',
    balance     Int64 DEFAULT 0,
    change_type LowCardinality(String),
    ledger_seq  UInt32,
    close_time  DateTime('UTC'),
    entry_xdr   String,
    intra_ledger_seq UInt32 DEFAULT 0,
    version     UInt64 MATERIALIZED bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq,
    INDEX idx_lecur_account_id account_id TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_lecur_asset asset TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(version)
ORDER BY (entry_type, key_xdr);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ledger_entries_current_v2_mv
TO stellar.ledger_entries_current_v2 AS
SELECT entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr, intra_ledger_seq
FROM stellar.ledger_entry_changes;

-- ── Step 2: windowed historical reproject (run under run-heavy-job.sh, ONE
-- window at a time). Bound every window by ledger_seq — ledger_entry_changes is
-- PARTITION BY intDiv(ledger_seq, 1000000), so a bounded window prunes
-- partitions instead of scanning the whole append-log. version is MATERIALIZED,
-- so the SELECT lists only the base columns and never mentions version. RMT
-- dedups on merge/FINAL to the highest version per (entry_type, key_xdr), so
-- re-running an overlapping window is SAFE/idempotent. Cap the upper bound at
-- the ledger_seq that was live when the v2 MV was created (Step 1) — the MV
-- already captured everything above it — though re-covering it is harmless.
-- Choose {window_start} to match the coverage you want: the min(ledger_seq)
-- currently in stellar.ledger_entries_current preserves today's coverage floor;
-- going lower additionally closes that floor (see StreamSACBalanceSeedsFullHistory
-- in internal/storage/clickhouse/sac_balance_seed.go for why the floor exists).
--
--   INSERT INTO stellar.ledger_entries_current_v2
--       (entry_type, key_xdr, account_id, asset, balance, change_type,
--        ledger_seq, close_time, entry_xdr, intra_ledger_seq)
--   SELECT entry_type, key_xdr, account_id, asset, balance, change_type,
--          ledger_seq, close_time, entry_xdr, intra_ledger_seq
--   FROM stellar.ledger_entry_changes
--   WHERE ledger_seq >= {window_start} AND ledger_seq < {window_end};
--
-- ── Step 3: verify v2 against v1 (spot-check a handful of hot keys — v2 must
-- match v1 for keys whose last change is unambiguous, and must CORRECT v1 for
-- keys with a same-ledger update-then-remove: v2 shows change_type='removed',
-- v1 may show the resurrected before-image):
-- *** REHEARSAL (2026-07-18, scratch CH 24.8) — READ THIS. The reproject is a
-- SHAPE change; it makes the tie-break deterministic ONLY where intra_ledger_seq
-- is populated (new ingest + a re-derived ledger_entry_changes range). A LEGACY
-- same-ledger tie whose two source rows are BOTH intra_ledger_seq=0 STILL
-- resolves ARBITRARILY, and this verify query will FALSELY report it as
-- "corrected" (v1 updated -> v2 removed) identically to a genuine fix. THE ONLY
-- TELL: v2 intra_ledger_seq (v2_ils below) = 0  ⇒  UNRESOLVED, not corrected.
-- To actually fix HISTORICAL resurrections you must first re-derive
-- ledger_entry_changes (idempotent-corrective under this RMT — no truncate):
--   stellarindex-ops ch-backfill -from 38000000 -to <tip>   (new binary, windowed)
-- Range is PROVEN: genesis(287404)→38M has ZERO same-ledger ties (full scan);
-- they begin in (38M,40M]. So [38M→tip] is the complete tie-range. Run it BEFORE
-- the Step-2 windows. Go-forward correctness needs neither. The DDL itself ran
-- clean end-to-end (cutover + both rollbacks verified). See the runbook
-- docs/operations/runbooks/post-phase0-deploy-sequence.md Step 3a. ***
--
--   SELECT key_xdr,
--          v1.change_type AS v1_ct, v1.ledger_seq AS v1_ledger,
--          v2.change_type AS v2_ct, v2.ledger_seq AS v2_ledger, v2.intra_ledger_seq AS v2_ils
--   FROM (SELECT * FROM stellar.ledger_entries_current      FINAL) v1
--   JOIN (SELECT * FROM stellar.ledger_entries_current_v2   FINAL) v2 USING (entry_type, key_xdr)
--   WHERE v1.change_type != v2.change_type
--   LIMIT 50;
--
-- ── Step 4: cutover (drop both MVs first — a renamed table does NOT drag its
-- MV's stored target reference along, so the MV would error INSERTs with
-- "Target table doesn't exist" otherwise), atomically double-RENAME, recreate
-- the MV under the canonical name/target, then run ONE small overlapping
-- catch-up reproject for the brief DDL gap (capture the pre-cutover tip first):
--
--   DROP TABLE IF EXISTS stellar.ledger_entries_current_mv;
--   DROP TABLE IF EXISTS stellar.ledger_entries_current_v2_mv;
--   RENAME TABLE stellar.ledger_entries_current    TO stellar.ledger_entries_current_old,
--                stellar.ledger_entries_current_v2 TO stellar.ledger_entries_current;
--   CREATE MATERIALIZED VIEW stellar.ledger_entries_current_mv
--     TO stellar.ledger_entries_current AS
--     SELECT entry_type, key_xdr, account_id, asset, balance, change_type,
--            ledger_seq, close_time, entry_xdr, intra_ledger_seq
--     FROM stellar.ledger_entry_changes;
--   -- catch-up for [pre_cutover_tip, now]:
--   INSERT INTO stellar.ledger_entries_current
--       (entry_type, key_xdr, account_id, asset, balance, change_type,
--        ledger_seq, close_time, entry_xdr, intra_ledger_seq)
--   SELECT entry_type, key_xdr, account_id, asset, balance, change_type,
--          ledger_seq, close_time, entry_xdr, intra_ledger_seq
--   FROM stellar.ledger_entry_changes
--   WHERE ledger_seq >= {pre_cutover_tip};
--
-- ── Step 5: DROP TABLE stellar.ledger_entries_current_old SYNC — only after
-- Step 3 confirms v2 is correct and reads have been served from the canonical
-- (now-v2) table for a settling period.
--
-- ── ROLLBACK ─────────────────────────────────────────────────────────────────
-- The post-fix indexer binary writes intra_ledger_seq into ledger_entry_changes
-- (a DEFAULT-0 column, harmless to a pre-fix binary) and the recreated MV; a
-- rollback of the version-column change must be paired with a rollback of the
-- binary to the pre-C2-4c writers only if you also drop the column.
--   * BEFORE Step 4 (no cutover yet): just drop v2 — fully reversible, v1 never
--     stopped serving:
--       DROP TABLE IF EXISTS stellar.ledger_entries_current_v2_mv;
--       DROP TABLE IF EXISTS stellar.ledger_entries_current_v2 SYNC;
--   * AFTER Step 4 (cutover done): recreate the original ledger_seq-versioned
--     shape as _v1restore, reproject, and RENAME-swap back (the inverse of
--     Steps 1-4), OR — if Step 5 has not run — RENAME the retained _old table
--     back to canonical:
--       DROP TABLE IF EXISTS stellar.ledger_entries_current_mv;
--       RENAME TABLE stellar.ledger_entries_current     TO stellar.ledger_entries_current_v2,
--                    stellar.ledger_entries_current_old TO stellar.ledger_entries_current;
--       CREATE MATERIALIZED VIEW stellar.ledger_entries_current_mv
--         TO stellar.ledger_entries_current AS
--         SELECT entry_type, key_xdr, account_id, asset, balance, change_type,
--                ledger_seq, close_time, entry_xdr
--         FROM stellar.ledger_entry_changes;
--   * The Step-0 column add reverses with
--       ALTER TABLE stellar.ledger_entry_changes DROP COLUMN IF EXISTS intra_ledger_seq;
--     (only after the binary is rolled back to a version that does not name it).
