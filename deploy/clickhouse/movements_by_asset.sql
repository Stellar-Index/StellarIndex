-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically, so the DDL here is a mirror and
-- the runbook is the reason the file exists. Scope markers are enforced by
-- scripts/ci/lint-ch-apply-scope.sh.
--
-- movements_by_asset: account_movements re-keyed asset-first. Design and
-- column rationale: tier1_schema.sql.
--
-- Step 1 (table + MV, instant) -> Step 2 (partition walk, HEAVY) -> Step 3.

CREATE TABLE IF NOT EXISTS stellar.movements_by_asset
(
    address           String,
    ledger            UInt32,
    ledger_close_time DateTime64(0, 'UTC'),
    tx_hash           String,
    op_index          UInt32,
    leg_index         UInt32,
    direction         LowCardinality(String),
    movement_kind     LowCardinality(String),
    provenance        LowCardinality(String),
    asset             String,
    counterparty      String DEFAULT '',
    amount            Int128,
    attributes        String DEFAULT '{}',
    ingested_at       DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (asset, ledger, tx_hash, op_index, leg_index, direction, address);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.movements_by_asset_mv
TO stellar.movements_by_asset AS
SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
       movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
FROM stellar.account_movements;

-- ── Step 2: partition-at-a-time catch-up ────────────────────────────────────
-- ***Heavy job, r1 only.*** Sorting account_movements asset-first costs
-- ~2.35 us/row (~7 h sort+read for the full table; plan 8-14 h with insert
-- cost and sort spill; target 0.4-0.6 TiB compressed). Run AFTER the lake
-- backup and never beside another heavy job. account_movements is ordered by
-- address, so each statement is one 1M-ledger partition (up to ~518M rows)
-- through run-heavy-job.sh. Capture TIP when the MV is created; overlap with
-- the MV era is idempotent under the ReplacingMergeTree; re-running a window is safe.
--
-- A skipped partition drops a million ledgers of movements from a history the
-- marker (Step 3) certifies complete, so the loop is fail-closed as
-- ttl_live_until.sql's is: a bad TIP, a failed job, a wrapper exit 75 (a lock
-- was held and the job did not run) or a missing success marker aborts the
-- subshell, and COMPLETE prints only when every partition ran. Paste the
-- block WHOLE; after an abort fix the cause and re-run with the same TIP.
--
--   TIP=$(clickhouse-client --port 9300 -q "SELECT max(ledger) FROM stellar.account_movements") || TIP=""
--   (
--     case "$TIP" in ''|*[^0-9]*|0*)
--       echo "movements_by_asset: TIP='$TIP' is not a ledger number (probe failed) - aborting, NOTHING was copied" >&2; exit 1 ;;
--     esac
--     MBA_MARKS=$(mktemp -d) || exit 1
--     MBA_DONE=0; MBA_WANT=0
--     for P in $(seq 0 $((TIP / 1000000))); do
--       MBA_WANT=$((MBA_WANT + 1))
--       MBA_MARK="$MBA_MARKS/$P.ok"
--       /usr/local/sbin/run-heavy-job.sh "movements-by-asset-$P" bash -c 'clickhouse-client --port 9300 -q "$1" && : > "$2"' mba-job "
--         INSERT INTO stellar.movements_by_asset
--         SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
--                movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
--         FROM stellar.account_movements FINAL
--         WHERE ledger >= $((P * 1000000)) AND ledger < $(((P + 1) * 1000000))
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000,
--                  max_bytes_before_external_sort = 4000000000" "$MBA_MARK" </dev/null \
--         || { if [ $? -eq 75 ]; then echo "movements_by_asset: partition $P DID NOT RUN (the wrapper exited 75 - a lock was held and it did not run) - aborting"; else echo "movements_by_asset: partition $P FAILED - aborting"; fi >&2; exit 1; }
--       [ -e "$MBA_MARK" ] \
--         || { echo "movements_by_asset: partition $P DID NOT RUN (the wrapper exited 0 with no success marker) - aborting" >&2; exit 1; }
--       MBA_DONE=$((MBA_DONE + 1))
--       # Let this partition's merges drain before the next one. The count is
--       # per partition, like parts_to_delay_insert (1000): a table-wide count
--       # grows with the 65 settled partitions and would never fall below 100.
--       while [ "$(clickhouse-client --port 9300 -q "SELECT count() FROM system.parts
--           WHERE database = 'stellar' AND table = 'movements_by_asset' AND active
--             AND partition_id = '$P'")" -ge 100 ]; do
--         sleep 60
--       done
--     done
--     echo "movements_by_asset: $MBA_DONE of $MBA_WANT partitions ran to success"
--     [ "$MBA_WANT" -gt 0 ] && [ "$MBA_DONE" -eq "$MBA_WANT" ] \
--       || { echo "movements_by_asset: NOT every partition ran - aborting" >&2; exit 1; }
--     rm -rf "$MBA_MARKS"
--   ) && echo "movements_by_asset: COMPLETE - every partition 0..$((TIP / 1000000)) copied"
--
-- FINAL is mandatory: the target key includes asset, so an unmerged stale
-- source row (same key, superseded asset) would be copied in and never merge
-- away.
--
-- OPERATOR NOTE: a re-derive of account_movements that relabels assets (a
-- wrong -network SAC check in ch_cap67_movements.go, or CAP-0038 leg
-- renumbering) leaves the old-asset rows here forever. After such a re-derive,
-- for each affected partition P:
--   ALTER TABLE stellar.movements_by_asset DROP PARTITION P;
-- then repeat that partition's FINAL copy above.
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
-- Per partition, FINAL row counts and distinct keys (asset included) must match
-- on both sides; a count above the distinct-key count means leftover rows.
-- Every partition 0..TIP/1000000 is checked, each a heavy job, and the marker
-- is recorded only behind PASSED; until it exists
-- GET /v1/assets/{asset_id}/movements answers lower_bound=true. Run with the
-- same TIP as Step 2, in the same shell:
--
--   (
--     case "$TIP" in ''|*[^0-9]*|0*)
--       echo "movements_by_asset verify: TIP='$TIP' is not a ledger number - aborting" >&2; exit 1 ;;
--     esac
--     MBA_OUT=$(mktemp -d) || exit 1
--     for P in $(seq 0 $((TIP / 1000000))); do
--       LO=$((P * 1000000)); HI=$(((P + 1) * 1000000))
--       rm -f "$MBA_OUT/$P.tsv"
--       /usr/local/sbin/run-heavy-job.sh "movements-by-asset-verify-$P" bash -c 'clickhouse-client --port 9300 -q "$1" > "$2.tmp" && mv "$2.tmp" "$2"' mba-verify "
--         SELECT
--           (SELECT count() FROM stellar.account_movements FINAL WHERE ledger >= $LO AND ledger < $HI),
--           (SELECT count() FROM stellar.movements_by_asset FINAL WHERE ledger >= $LO AND ledger < $HI),
--           (SELECT uniqExact(address, ledger, tx_hash, op_index, leg_index, direction, asset)
--              FROM stellar.account_movements WHERE ledger >= $LO AND ledger < $HI),
--           (SELECT uniqExact(address, ledger, tx_hash, op_index, leg_index, direction, asset)
--              FROM stellar.movements_by_asset WHERE ledger >= $LO AND ledger < $HI)" "$MBA_OUT/$P.tsv" </dev/null \
--         || { echo "movements_by_asset verify: partition $P DID NOT RUN or FAILED (rc=$?) - aborting" >&2; exit 1; }
--       read -r SRC_ROWS DST_ROWS SRC_KEYS DST_KEYS < "$MBA_OUT/$P.tsv" \
--         || { echo "movements_by_asset verify: partition $P wrote no result - aborting" >&2; exit 1; }
--       [ -n "$DST_KEYS" ] && [ "$SRC_ROWS" = "$DST_ROWS" ] && [ "$SRC_KEYS" = "$DST_KEYS" ] \
--         || { echo "movements_by_asset verify: partition $P MISMATCH src_rows=$SRC_ROWS dst_rows=$DST_ROWS src_keys=$SRC_KEYS dst_keys=$DST_KEYS - aborting" >&2; exit 1; }
--     done
--     rm -rf "$MBA_OUT"
--   ) && echo "movements_by_asset verify: PASSED" \
--     && clickhouse-client --port 9300 -q "INSERT INTO stellar.cap67_movements_watermark (name, thru_ledger)
--          VALUES ('movements_by_asset_backfill', $TIP)"
--
-- An asset-relabelling re-derive (OPERATOR NOTE above) deletes the marker
-- until its partitions are re-copied and re-verified:
--   ALTER TABLE stellar.cap67_movements_watermark DELETE WHERE name = 'movements_by_asset_backfill'
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
--   DROP TABLE IF EXISTS stellar.movements_by_asset_mv;
--   DROP TABLE IF EXISTS stellar.movements_by_asset SYNC;
--   ALTER TABLE stellar.cap67_movements_watermark DELETE WHERE name = 'movements_by_asset_backfill'
