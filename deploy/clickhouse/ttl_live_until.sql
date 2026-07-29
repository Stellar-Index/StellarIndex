-- ttl_live_until: slim TTL-liveness projection — key_hash → liveUntilLedgerSeq,
-- MV-maintained from stellar.ledger_entry_changes (v0.21.4 headline fix).
--
-- Why this table exists: internal/storage/clickhouse/ttl_liveness.go's
-- ClassifyTTLLiveness used to resolve LIVE/ARCHIVED/UNKNOWN by scanning
-- `stellar.ledger_entries_current WHERE entry_type = 'ttl'` (586M rows) per
-- 1,500-key batch, computing liveUntilLedgerSeq from the WIDE `entry_xdr`
-- column for EVERY ttl row per batch. Six production attempts (2026-07-29)
-- failed across four mechanisms — IN-list over the 256 KiB parse cap, two
-- aggregation OOMs against the client's 10 GiB pin, thread fan-out read
-- amplification, and terminally an OOM of the query's own 8 GiB pin inside
-- AggregatingTransform driven by that wide-column read. Tuning was exhausted;
-- the design was wrong. This projection replaces the scan with a primary-key
-- lookup, bounded by construction: three tiny columns, one row per TTL change,
-- ~20-30 GB total vs the 590 GB source. The old scan path is DELETED from the
-- binary — v0.21.4+ ClassifyTTLLiveness reads ONLY this table and returns a
-- hard error naming this file if it is absent (no silent fallback).
--
-- This file is the operator-run migration artifact for an EXISTING deployment
-- (r1) plus the windowed-backfill runbook. A FRESH deployment gets the table +
-- MV from deploy/clickhouse/tier1_schema.sql (canonical DDL, kept in lockstep
-- with this file — ttl_liveness_test.go pins the extraction expressions in
-- BOTH); a fresh deployment still needs the Step-2 backfill for any
-- ledger_entry_changes history ingested before the MV existed.
--
-- Extraction (production-validated 2026-07-28 ~07:55Z against the lake and by
-- the pre-v0.21.4 classifier's ttlLiveUntilExpr — ported faithfully):
--   * A TTL LedgerKey is 36 bytes: type=00000009 (4) | sha256(LedgerKey) (32)
--     → key_hash = bytes [5,32] of the decoded key_xdr (1-indexed substring).
--   * A TTLEntry is 48 bytes: lastModified(4) | data.type(4) | keyHash(32) |
--     liveUntilLedgerSeq(4) | ext(4) → live_until = the 4 bytes at decoded
--     offset 41 (1-indexed). XDR integers are big-endian and
--     reinterpretAsUInt32 is little-endian, hence the reverse().
--   * version = (ledger_seq << 32) | intra_ledger_seq — the same composite
--     ReplacingMergeTree version as ledger_entries_current (audit-2026-07-16
--     C2-4c), so the latest same-ledger change wins deterministically.
--
-- Malformed-row guard (fail-open, same contract as the Go code): rows whose
-- decoded key is not exactly 36 bytes or whose decoded entry is not exactly 48
-- bytes are SKIPPED, never inserted — a protocol change to the TTLEntry layout
-- degrades to "key absent → TTLUnknown → caller keeps the entry", never to a
-- garbage live_until that would read as "archived" and silently delete live
-- balances. tryBase64Decode, NOT base64Decode: inside an MV a decode THROW
-- would fail the whole source INSERT into ledger_entry_changes and block
-- ingest; try* yields '' and the length guards drop the row. The guards also
-- skip change_type='removed' ttl rows (the extractor stores only the key for
-- removals — extract_entry_changes.go — so entry_xdr is ''): the table then
-- retains the last parseable live_until, which for a removed TTL has lapsed
-- anyway, so the verdict stays conservative.
--
-- Reader shape (internal/storage/clickhouse/ttl_liveness.go):
--   SELECT lower(hex(key_hash)), argMax(live_until, version)
--   FROM stellar.ttl_live_until WHERE key_hash IN (...) GROUP BY key_hash
--
-- Operator sequence (v0.21.4 deploy order — the binary path REFUSES until the
-- table exists): Step 1 (table + MV) → Step 2 (windowed backfill) → Step 3
-- (verify) → deploy the v0.21.4 binaries → re-run `supply seed-sac-balances`
-- (restores USDC's 48,505 dormant holders → supply reconcile 8/8).

-- ── Step 1: create the projection + MV ───────────────────────────────────────
-- The MV starts capturing every LIVE ledger_entry_changes insert the moment it
-- exists; the Step-2 backfill only needs to cover ledgers ingested before this
-- point. Run both statements together; there is no cutover (this is a NEW
-- table, nothing serves from the old scan path once the v0.21.4 binary ships).

CREATE TABLE IF NOT EXISTS stellar.ttl_live_until
(
    key_hash   FixedString(32),
    live_until UInt32,
    version    UInt64
)
ENGINE = ReplacingMergeTree(version)
ORDER BY key_hash;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ttl_live_until_mv
TO stellar.ttl_live_until AS
SELECT
    toFixedString(substring(tryBase64Decode(key_xdr), 5, 32), 32) AS key_hash,
    reinterpretAsUInt32(reverse(substring(tryBase64Decode(entry_xdr), 41, 4))) AS live_until,
    bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq AS version
FROM stellar.ledger_entry_changes
WHERE entry_type = 'ttl'
  AND length(tryBase64Decode(key_xdr)) = 36
  AND length(tryBase64Decode(entry_xdr)) = 48;

-- ── Step 2: windowed historical backfill ─────────────────────────────────────
-- ***Heavy op.*** Run under run-heavy-job.sh on r1, ONE window at a time
-- (CLAUDE.md heavy-job doctrine). ledger_entry_changes is PARTITION BY
-- intDiv(ledger_seq, 1000000), so bounding every window by ledger_seq prunes
-- partitions instead of scanning the whole append-log. TTL entries are Soroban
-- state and cannot predate protocol 20 (~ledger 50.45M), so start at
-- 50,000,000 — earlier windows are provably empty. Cap the final window at the
-- ledger_seq that was live when the Step-1 MV was created; overlapping it is
-- harmless (ReplacingMergeTree dedups on version), so re-running any window is
-- SAFE/idempotent. The SELECT is the MV's, verbatim.
--
--   INSERT INTO stellar.ttl_live_until (key_hash, live_until, version)
--   SELECT
--       toFixedString(substring(tryBase64Decode(key_xdr), 5, 32), 32) AS key_hash,
--       reinterpretAsUInt32(reverse(substring(tryBase64Decode(entry_xdr), 41, 4))) AS live_until,
--       bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq AS version
--   FROM stellar.ledger_entry_changes
--   WHERE entry_type = 'ttl'
--     AND length(tryBase64Decode(key_xdr)) = 36
--     AND length(tryBase64Decode(entry_xdr)) = 48
--     AND ledger_seq >= {window_start} AND ledger_seq < {window_end}
--   SETTINGS max_threads = 4,
--            max_memory_usage = 8000000000,
--            max_bytes_before_external_group_by = 4000000000,
--            max_bytes_before_external_sort = 4000000000;
--
-- Operator loop (r1; native port 9300; 2M-ledger windows ≈ 2 partitions each;
-- capture TIP from the MV-creation moment first):
--
--   TIP=$(clickhouse-client --port 9300 --query "SELECT max(ledger_seq) FROM stellar.ledger_entry_changes")
--   for W in $(seq 50000000 2000000 "$TIP"); do
--     /usr/local/sbin/run-heavy-job.sh "ttl-backfill-$W" clickhouse-client --port 9300 --query "
--       INSERT INTO stellar.ttl_live_until (key_hash, live_until, version)
--       SELECT toFixedString(substring(tryBase64Decode(key_xdr), 5, 32), 32),
--              reinterpretAsUInt32(reverse(substring(tryBase64Decode(entry_xdr), 41, 4))),
--              bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq
--       FROM stellar.ledger_entry_changes
--       WHERE entry_type = 'ttl'
--         AND length(tryBase64Decode(key_xdr)) = 36
--         AND length(tryBase64Decode(entry_xdr)) = 48
--         AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000,
--                max_bytes_before_external_group_by = 4000000000,
--                max_bytes_before_external_sort = 4000000000"
--   done
--
-- (seq's last window may extend past $TIP — that overlap is the deliberate
-- MV-era overlap and is idempotent under the RMT.)

-- ── Step 3: verify against the old source of truth ───────────────────────────
-- Spot-check N sampled ttl keys: the projection's argMax(live_until, version)
-- must equal the same extraction computed over ledger_entries_current
-- (bounded: the sample IN-list makes both sides primary-key lookups, and the
-- sample read itself is a contiguous PK-range read — ledger_entries_current is
-- ORDER BY (entry_type, key_xdr)). Rows where cur_lu = 0 are the fail-open
-- skip class (unparseable shape on the current-state side) and are excluded —
-- the projection deliberately holds the last PARSEABLE value for those.
-- Expect mismatched = 0.
--
--   WITH sample AS (
--       SELECT DISTINCT key_xdr FROM stellar.ledger_entries_current
--       WHERE entry_type = 'ttl' LIMIT 10000
--   )
--   SELECT count() AS checked, countIf(cur_lu != tll_lu) AS mismatched
--   FROM (
--       SELECT substring(tryBase64Decode(key_xdr), 5, 32) AS kh,
--              argMax(if(length(tryBase64Decode(entry_xdr)) = 48,
--                        reinterpretAsUInt32(reverse(substring(tryBase64Decode(entry_xdr), 41, 4))), 0),
--                     bitShiftLeft(toUInt64(ledger_seq), 32) + intra_ledger_seq) AS cur_lu
--       FROM stellar.ledger_entries_current
--       WHERE entry_type = 'ttl' AND key_xdr IN (SELECT key_xdr FROM sample)
--         AND length(tryBase64Decode(key_xdr)) = 36
--       GROUP BY kh
--   ) cur
--   INNER JOIN (
--       SELECT toString(key_hash) AS kh, argMax(live_until, version) AS tll_lu
--       FROM stellar.ttl_live_until
--       WHERE key_hash IN (
--           SELECT toFixedString(substring(tryBase64Decode(key_xdr), 5, 32), 32) FROM sample
--       )
--       GROUP BY key_hash
--   ) tll USING (kh)
--   WHERE cur_lu != 0
--   SETTINGS max_threads = 4, max_memory_usage = 8000000000;
--
-- Size sanity (expect low-tens of GB):
--   SELECT sum(bytes_on_disk) FROM system.parts
--   WHERE database = 'stellar' AND table = 'ttl_live_until' AND active;

-- ── ROLLBACK ─────────────────────────────────────────────────────────────────
-- The projection is additive — nothing else writes or reads it — so rollback
-- is a plain drop. BUT the v0.21.4+ binary hard-fails every TTL-liveness read
-- (SAC seed, soroswap pair-state) without the table, so dropping it must be
-- paired with rolling the binaries back to a pre-v0.21.4 scan-path build:
--   DROP TABLE IF EXISTS stellar.ttl_live_until_mv;
--   DROP TABLE IF EXISTS stellar.ttl_live_until SYNC;
