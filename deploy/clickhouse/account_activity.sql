-- account_activity: per-account activity watermark (#31) — (account → the
-- max ledger the account was EVER named in, across every role the
-- account-history readers serve), MV-maintained from stellar.operations,
-- stellar.transactions and stellar.operation_participants.
--
-- Why this table exists: GET /v1/accounts/{g}/operations resolves each of its
-- two keyset arms with a reverse primary-key read over stellar.operations
-- (10.6B rows, ORDER BY (ledger_seq, tx_index, op_index)) — `ORDER BY pk DESC
-- LIMIT n` streams granules backwards FROM THE TIP until it accumulates the
-- account's rows. For a long-idle account that walk covers every granule
-- between the tip and the account's last activity: MEASURED ~4s live
-- (2026-08-24, r1) for a 46d-idle account
-- (GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4), worse under
-- load — the scan that ate the 8s request budget (the tx-outcome starvation
-- SYMPTOM was separately fixed by PR #155's detached budget; this fixes the
-- scan itself). With the watermark, the reader first does a point lookup here
-- (primary-key read over ~53M single-column-keyed rows, ms) and adds
-- `ledger_seq <= last_ledger` to each arm's resolve — partition + primary-key
-- pruning then starts the reverse read AT the account's real last activity.
--
-- ╔════════════════════════════════════════════════════════════════════════╗
-- ║ CORRECTNESS INVARIANT — DATA-HIDING (do not weaken):                  ║
-- ║ the watermark must be an UPPER bound on the ledger_seq of EVERY row   ║
-- ║ the account-history queries can return for the account, in ANY role.  ║
-- ║ Soroban/SAC token movements can postdate a classic account's own      ║
-- ║ sourced activity — the account then appears ONLY as a non-source      ║
-- ║ participant. That is why THREE MVs feed this table: they fire on      ║
-- ║ inserts to the SAME tables that feed the readers' key sources         ║
-- ║ (operations → ops_by_source ops arm, operation_participants,          ║
-- ║ transactions → ops_by_source tx-sentinel arm), so any row that can    ║
-- ║ surface also raised the watermark to at least its own ledger — an     ║
-- ║ EXACT upper bound by construction, immune to drift in participant     ║
-- ║ extraction. A too-HIGH watermark only costs scan range; a too-LOW     ║
-- ║ one silently HIDES history rows (unacceptable). Never narrow the MV   ║
-- ║ set; readers take max() over un-merged RMT rows, never FINAL-trust a  ║
-- ║ single row; a missing row falls back to the UNBOUNDED scan.           ║
-- ╚════════════════════════════════════════════════════════════════════════╝
--
-- Engine: ReplacingMergeTree(last_ledger) keyed by account_id — merges keep
-- the max-last_ledger row per account; readers max() over whatever is not
-- yet merged, so merge timing never changes an answer. No first_ledger
-- column: RMT keeps the max-version row, which would silently discard a
-- min-aggregate — if "first active" is ever wanted, that is a separate
-- min-semantics table, not a column here.
--
-- Bonus the shape gives for free: max(last_ledger)/argMax(last_seen,
-- last_ledger) is the account's "last active" fact for directory surfaces
-- (not yet wired into the account payload — API/OpenAPI follow-up).
--
-- Operator sequence (deploy order matters — see the hazard note in Step 2):
--   Step 1 (table + MVs, instant) → Step 2 (windowed backfill, heavy)
--   → Step 3 (verify) → deploy the binary that reads it.
-- The reader degrades gracefully (no row → unbounded scan), but Step 2 must
-- complete BEFORE the reading binary deploys: see the Step-2 hazard.

-- ── Step 1: table + the three MVs ───────────────────────────────────────────

CREATE TABLE IF NOT EXISTS stellar.account_activity
(
    account_id  String,
    last_ledger UInt32,
    last_seen   DateTime('UTC')
)
ENGINE = ReplacingMergeTree(last_ledger)
ORDER BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_ops_mv
TO stellar.account_activity AS
SELECT source_account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.operations
WHERE source_account != ''
GROUP BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_tx_mv
TO stellar.account_activity AS
SELECT source_account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.transactions
WHERE source_account != ''
GROUP BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_participants_mv
TO stellar.account_activity AS
SELECT account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.operation_participants
GROUP BY account_id;

-- ── Step 2: windowed historical backfill ────────────────────────────────────
-- ***Heavy op.*** run-heavy-job.sh, ONE window at a time, 2M-ledger windows
-- (all three source tables are PARTITION BY intDiv(ledger_seq, 1000000); the
-- selects are 2-3 narrow columns + a GROUP BY, no XDR decode). Capture TIP at
-- MV-creation time; overlapping the MV era is idempotent (max-wins RMT +
-- max() reads). Re-running any window is SAFE.
--
-- HAZARD (why Step 2 precedes the reader deploy): until the backfill covers
-- an account, a post-MV re-ingest of OLD ledgers can create the account's
-- ONLY watermark row with a ledger far below its true (pre-MV-ingested) last
-- activity — a too-LOW bound, i.e. hidden rows. Run the backfill immediately
-- after Step 1 and before deploying a binary that reads the bound; from then
-- on the invariant holds unconditionally (backfill + MVs jointly cover all
-- history, and max() only ever rises).
--
--   TIP=$(clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.ledgers")
--   for W in $(seq 2 2000000 "$TIP"); do
--     /usr/local/sbin/run-heavy-job.sh "acct-activity-ops-$W" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.account_activity
--       SELECT source_account, max(ledger_seq), max(close_time)
--       FROM stellar.operations
--       WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--       GROUP BY source_account
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--     /usr/local/sbin/run-heavy-job.sh "acct-activity-tx-$W" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.account_activity
--       SELECT source_account, max(ledger_seq), max(close_time)
--       FROM stellar.transactions
--       WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--       GROUP BY source_account
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--     /usr/local/sbin/run-heavy-job.sh "acct-activity-part-$W" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.account_activity
--       SELECT account, max(ledger_seq), max(close_time)
--       FROM stellar.operation_participants
--       WHERE ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--       GROUP BY account
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--   done
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
-- Spot-check N recently-active accounts: the watermark must be >= the true
-- max ledger over BOTH key sources (too-high is fine; ANY too-low row is a
-- data-hiding bug — expect 0):
--
--   SELECT countIf(wm < truth) FROM (
--     SELECT
--       (SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = t.sa) AS wm,
--       greatest(
--         (SELECT max(ledger_seq) FROM stellar.ops_by_source WHERE source_account = t.sa),
--         (SELECT max(ledger_seq) FROM stellar.operation_participants WHERE account = t.sa)) AS truth,
--       sa
--     FROM (SELECT DISTINCT source_account AS sa FROM stellar.transactions
--           WHERE ledger_seq > (SELECT max(ledger_seq) - 10000 FROM stellar.ledgers)
--           LIMIT 20) t)
--
-- Expect 0.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
-- Additive watermark; the reader falls back to the unbounded scan when the
-- table is absent (schema probe) — no binary rollback required, though the
-- idle-account pages regress to the tip-walk latency:
--   DROP TABLE IF EXISTS stellar.account_activity_ops_mv;
--   DROP TABLE IF EXISTS stellar.account_activity_tx_mv;
--   DROP TABLE IF EXISTS stellar.account_activity_participants_mv;
--   DROP TABLE IF EXISTS stellar.account_activity SYNC;
