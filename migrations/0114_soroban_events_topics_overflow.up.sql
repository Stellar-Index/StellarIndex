-- 0114 up — store the COMPLETE ordered topic list for soroban_events
-- (audit-2026-07-16 C2-11 — the >4-topic decode-loss).
--
-- The 0041 schema pins topics into FOUR fixed columns
-- (topic_0_xdr .. topic_3_xdr). The capture path (internal/sources/
-- sorobanevents.Capture → decodeTopics) truncated at 4 and the replay
-- path (Reconstruct → reconstructTopics) capped `want > 4 → 4`, so a
-- Soroban event carrying 5+ topics (e.g. an Aquarius multi-token pool
-- event) landed with topics 5+ SILENTLY DROPPED — a decode-loss. Only
-- topic_count recorded that anything was lost; the topic bytes were
-- gone. Any future per-source decoder that keys on topic[4]+ would
-- backfill wrong from this landing zone.
--
-- Fix: add `topics_xdr bytea[]` — the COMPLETE ordered list of every
-- topic's raw XDR bytes, in emit order. This is the same shape the
-- ClickHouse raw lake already stores (stellar.contract_events.topics_xdr,
-- an Array — see internal/storage/clickhouse/sink.go), so the historical
-- re-ingest below is a straight column copy; and it matches the repo's
-- existing bytea[] precedent (0027 mfa_recovery_codes_hashed).
--
-- topic_0_xdr .. topic_3_xdr and topic_0_sym are RETAINED and still
-- populated (the first four topics) for back-compat: the topic_0_sym
-- index fast-paths (soroban_events_topic_sym_ts_idx /
-- soroban_events_contract_topic_idx) and every existing SQL reader keep
-- working unchanged. New readers that need full fidelity read topics_xdr;
-- reconstructTopics prefers it and falls back to topic_0..3 for legacy
-- rows.
--
-- NOT NULL DEFAULT '{}' — old-binary-safe (README rule 9): the currently
-- deployed pre-0114 binary's INSERT column list does not mention
-- topics_xdr, so a row it writes takes the empty-array default. Every
-- real Soroban contract event has >=1 topic, so an EMPTY topics_xdr can
-- only mean "written before 0114 / by a pre-0114 binary" — that is
-- exactly the fallback signal the reader uses (len(topics_xdr) == 0 =>
-- read topic_0..3). A row written by the >=0114 binary always has >=1
-- element and is authoritative.
--
-- TimescaleDB 2.11+ (r1 runs 2.26) supports ADD COLUMN with a constant
-- DEFAULT on a compressed hypertable directly — no decompress/recompress
-- dance (same as 0111). '{}' is a constant, so this is a metadata-only
-- default (no chunk rewrite).
--
-- ── Historical recovery (POST-DEPLOY OP — do NOT run from this migration) ──
-- This migration is the FORWARD fix: from deploy on, 5+-topic events are
-- captured whole. Rows already in soroban_events keep topics_xdr = '{}'
-- and, for the 5+-topic ones, permanently lost topics 5+ IN POSTGRES —
-- but the ClickHouse raw lake (stellar.contract_events) is topic-complete
-- to genesis (ADR-0034), so history is recoverable. Recover by
-- re-projecting the affected ledger range from the lake AFTER this
-- migration + the new binary are deployed, e.g.:
--
--     stellarindex-ops projector-replay -ch \
--       -from <soroban-genesis> -to <lake-tip>
--
-- (the -ch replay re-derives soroban_events from the lake). Scope can be
-- narrowed to the contracts known to emit 5+ topics if a full re-project
-- is too heavy. Left to the operator; NOT executed here.

BEGIN;

ALTER TABLE soroban_events
    ADD COLUMN topics_xdr bytea[] NOT NULL DEFAULT '{}';

COMMENT ON COLUMN soroban_events.topics_xdr IS
    'Complete ordered list of every topic''s raw XDR bytes, in emit order '
    '(audit-2026-07-16 C2-11). Supersedes the four fixed topic_0..3 columns, '
    'which are retained + still populated (first four topics) for back-compat '
    'and the topic_0_sym index fast-path. Empty ''{}'' marks a legacy row '
    'written before this column existed — read topic_0..3 for those. Events '
    'with 5+ topics (e.g. Aquarius multi-token pool events) no longer '
    'truncate. Recover pre-0114 rows via a ClickHouse-lake re-project.';

COMMIT;
