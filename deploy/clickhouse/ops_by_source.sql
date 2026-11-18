-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically — the gate named below pins that,
-- so the DDL here is a mirror and the runbook is the reason the file exists.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- ops_by_source: slim sourced-history projection — (source_account →
-- ledger/tx/op keys), MV-maintained from BOTH stellar.operations and
-- stellar.transactions.
--
-- WHY: the account-history readers' "sourced" arms (explorer_reader.go
-- accountTransactionsQuery / accountOperationsQuery arm 1) resolved
-- `source_account = ?` via the bloom skip-index over the full transactions /
-- operations tables: granule-pruned but scan-shaped (110x slower than the
-- PK-prefixed operation_participants arm). This extracts the lookup once per
-- row at ingest into a slim PK-ordered table, like stellar.ttl_live_until, so
-- the read is a primary-index range.
--
-- ONE table serves BOTH readers via an op_index sentinel:
--   * rows from stellar.operations carry the OP-EFFECTIVE source (an op
--     can override its tx's source) with the real op_index → the
--     OPERATIONS arm reads them (op_index != sentinel);
--   * rows from stellar.transactions carry the TX source with
--     op_index = 4294967295 (the sentinel) → the TRANSACTIONS arm reads
--     DISTINCT (ledger_seq, tx_index) over ALL rows, so a tx is found
--     whether the account sourced the tx itself or any single op in it;
--   * a fee bump adds a second sentinel row for its PAYER (fee_account;
--     source_account is the inner source). fee_account is populated
--     go-forward only (transactions_fee_bump.sql), so a fee bump ingested
--     before that column is in the inner source's history only, until its
--     range is re-derived.
-- Both streams preserve the readers' semantics: a tx sourced by A whose every
-- op overrides to another source is still A's; an op sourced by B inside A's
-- tx is still B's.
--
-- Engine: ReplacingMergeTree over the full sort key — rows are identity
-- facts (no versions to race); a backfill re-run or MV/backfill overlap
-- dedups to one row. Readers keep their LIMIT 1 BY guards, so un-merged
-- duplicates never shorten a page.
--
-- Operator sequence (deploy order matters — the binary REFUSES
-- account-history reads until this table exists; no scan fallback):
--   Step 1 (table + MVs, instant) → Step 2 (windowed backfills, heavy)
--   → Step 3 (verify) → deploy the binary that reads it.

-- ── Step 1: table + both MVs ────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS stellar.ops_by_source
(
    source_account String,
    ledger_seq     UInt32,
    tx_index       UInt32,
    op_index       UInt32
)
ENGINE = ReplacingMergeTree
ORDER BY (source_account, ledger_seq, tx_index, op_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ops_by_source_ops_mv
TO stellar.ops_by_source AS
SELECT source_account, ledger_seq, tx_index, op_index
FROM stellar.operations
WHERE source_account != '';

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ops_by_source_tx_mv
TO stellar.ops_by_source AS
SELECT source_account, ledger_seq, tx_index, 4294967295 AS op_index
FROM stellar.transactions
WHERE source_account != '';

-- A fee bump's payer is in fee_account, not source_account (that is the
-- inner source). The payer gets the same tx-sentinel row, so the
-- transaction it paid for is in its history. No `!= source_account`
-- filter: the alias below shadows that column in WHERE, and a payer that
-- is also the inner source writes a row the engine collapses.
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ops_by_source_feepayer_mv
TO stellar.ops_by_source AS
SELECT fee_account AS source_account, ledger_seq, tx_index, 4294967295 AS op_index
FROM stellar.transactions
WHERE fee_account != '';

-- ── Step 2: windowed historical backfill ────────────────────────────────────
-- ***Heavy op.*** run-heavy-job.sh, ONE window at a time, 2M-ledger windows
-- (both source tables are PARTITION BY intDiv(ledger_seq, 1000000); the
-- selects are 4 narrow columns, no XDR decode — far lighter per-row than the
-- ttl backfill). Capture TIP at MV-creation time; overlapping the MV era is
-- idempotent under the RMT. Re-running any window is SAFE.
--
-- Fail-closed as account_activity.sql Step 2 is: a bad TIP, a failed job, a
-- wrapper exit 75 (a lock was held and the job did not run) or a missing
-- success marker aborts the subshell; COMPLETE prints only when every job
-- ran. The fee-payer arm reads rows with fee_account set, so windows before
-- fee_account was populated insert nothing. Paste the block WHOLE.
--
--   TIP=$(clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.ledgers") || TIP=""
--   (
--     case "$TIP" in ''|*[^0-9]*|0*)
--       echo "ops_by_source backfill: TIP='$TIP' is not a ledger number (probe failed) - aborting, NOTHING was backfilled" >&2; exit 1 ;;
--     esac
--     OBS_MARKS=$(mktemp -d) || exit 1
--     OBS_DONE=0; OBS_WANT=0
--     obs_job() {
--       OBS_MARK="$OBS_MARKS/$1-$2.ok"
--       OBS_WANT=$((OBS_WANT + 1))
--       /usr/local/sbin/run-heavy-job.sh "obs-backfill-$1-$2" bash -c 'clickhouse-client --port 9300 -q "$1" && : > "$2"' obs-job "$3" "$OBS_MARK" </dev/null \
--         || { if [ $? -eq 75 ]; then echo "ops_by_source backfill: $1 window $2 DID NOT RUN (the wrapper exited 75 - a lock was held and it did not run) - aborting"; else echo "ops_by_source backfill: $1 window $2 FAILED - aborting"; fi >&2; exit 1; }
--       [ -e "$OBS_MARK" ] \
--         || { echo "ops_by_source backfill: $1 window $2 DID NOT RUN (the wrapper exited 0 with no success marker) - aborting" >&2; exit 1; }
--       OBS_DONE=$((OBS_DONE + 1))
--     }
--     W=2
--     while [ "$W" -le "$TIP" ]; do
--       obs_job ops "$W" "
--         INSERT INTO stellar.ops_by_source
--         SELECT source_account, ledger_seq, tx_index, op_index
--         FROM stellar.operations
--         WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--       obs_job tx "$W" "
--         INSERT INTO stellar.ops_by_source
--         SELECT source_account, ledger_seq, tx_index, 4294967295
--         FROM stellar.transactions
--         WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--       obs_job feepayer "$W" "
--         INSERT INTO stellar.ops_by_source
--         SELECT fee_account, ledger_seq, tx_index, 4294967295
--         FROM stellar.transactions
--         WHERE fee_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--       W=$((W + 2000000))
--     done
--     echo "ops_by_source backfill: $OBS_DONE of $OBS_WANT jobs ran to success"
--     [ "$OBS_WANT" -gt 0 ] && [ "$OBS_DONE" -eq "$OBS_WANT" ] \
--       || { echo "ops_by_source backfill: NOT every job ran (or TIP=$TIP yields no window) - aborting" >&2; exit 1; }
--     rm -rf "$OBS_MARKS"
--   ) && echo "ops_by_source backfill: COMPLETE - every window 2..$TIP covered"
--
-- A host that already ran the ops and tx arms needs only the fee-payer arm:
-- run the block with the ops and tx obs_job calls removed.
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
-- Spot-check N accounts: the projection's per-account key set must equal a
-- direct read of the source table.
-- ***Heavy op.*** fee_account has no skip index, so the OR defeats
-- idx_tx_source and each of the 20 subqueries scans all of transactions.
-- Never run it as a bare query on the serving host; run it under the
-- heavy-job lock, off-peak:
--
--   /usr/local/sbin/run-heavy-job.sh obs-verify \
--     clickhouse-client --port 9300 --max_threads 4 -q "
--   SELECT countIf(a != b) FROM (
--     SELECT
--       (SELECT countDistinct(ledger_seq, tx_index) FROM stellar.transactions WHERE source_account = t.sa OR fee_account = t.sa) AS a,  -- countDistinct, NOT count(): raw counts include un-merged RMT duplicate parts (a first verify run flagged 20/20 'mismatches' that were exactly this)
--       (SELECT countDistinct(ledger_seq, tx_index) FROM stellar.ops_by_source
--         WHERE source_account = t.sa AND op_index = 4294967295) AS b,
--       sa
--     FROM (SELECT DISTINCT source_account AS sa FROM stellar.transactions
--           WHERE ledger_seq > (SELECT max(ledger_seq) - 10000 FROM stellar.ledgers)
--           LIMIT 20) t)"
--
-- Expect 0. Then prove the whole table, window by window, and record the
-- proof watermark the API probes (ops_by_source_proof):
--
--   /usr/local/sbin/run-heavy-job.sh obs-proof stellarindex-ops ch-index-proof -ch-addr 127.0.0.1:9300 -write
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
-- Additive projection; nothing else writes or reads it. Pair a drop with
-- rolling the binaries back to a pre-projection build:
--   DROP TABLE IF EXISTS stellar.ops_by_source_ops_mv;
--   DROP TABLE IF EXISTS stellar.ops_by_source_tx_mv;
--   DROP TABLE IF EXISTS stellar.ops_by_source_feepayer_mv;
--   DROP TABLE IF EXISTS stellar.ops_by_source SYNC;
