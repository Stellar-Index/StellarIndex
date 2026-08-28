#!/usr/bin/env bash
# D3 — ledger_entries_current version rebuild:
# ReplacingMergeTree(ledger_seq) → ReplacingMergeTree(version),
# version = (ledger_seq << 32) | intra_ledger_seq  (audit C2-4c / CS-021).
#
# Executes deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql — the
# operator-run migration artifact — as a phased, resumable script. Read that
# file's header + the 2026-07-18 rehearsal note before running: the tie-break
# is only effective where intra_ledger_seq is populated in the source
# append-log (D2 partitions 39–53, Phase-0 re-derived ranges, live ingest
# ≥~63,550,000). Run `probe-ordinals` first to see actual coverage.
#
# Phases (run in order; each is independently resumable/idempotent):
#   probe-ordinals            cheap per-partition sample of ordinal coverage
#   setup                     Step 0 (ADD COLUMN) + Step 1 (v2 table + MV);
#                             records the MV-creation tip in the state dir
#   reproject <from> <to>     Step 2: windowed INSERT of [from,to) into v2;
#                             resumable, overlapping re-runs are safe (RMT)
#   verify                    Step 3: v1-vs-v2 divergence sample + coverage
#   cutover                   Step 4: drop MVs, double-RENAME, recreate MV,
#                             catch-up window from the recorded pre-cutover tip
#   finalize                  Step 5: DROP _old (requires D3_FORCE_DROP_OLD=yes)
#   rollback-precutover       drop v2 + its MV (v1 never stopped serving)
#
# Heavy phases (reproject) run under the wrapper:
#   run-heavy-job.sh d3-reproject /usr/local/sbin/d3-lecur-v2-rebuild.sh reproject 38000000 63700000
set -euo pipefail

CH="${CH:-clickhouse-client --port 9300}"
STATE_DIR="${D3_STATE:-/var/lib/ch-backfill/d3}"
CHUNK="${D3_CHUNK:-100000}"        # ledgers per INSERT window. Plain filter-
                                   # project (no join, no window fn) — streams,
                                   # unlike D2's sort-heavy reproject.
D3_THREADS="${D3_THREADS:-10}"     # same rationale as D2: half the box.
mkdir -p "$STATE_DIR"
log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) d3: $*"; }
q()   { $CH --max_execution_time 3600 \
            --max_memory_usage 20000000000 \
            --max_bytes_before_external_sort 4000000000 \
            --max_bytes_before_external_group_by 4000000000 \
            --max_threads "$D3_THREADS" \
            -q "$1"; }

# Destructive-DDL size guard (docs/operations/clickhouse-destructive-ddl.md):
# the DROP TABLE of ledger_entries_current_old / _v2 is refused above
# max_table_size_to_drop (50 GiB, ansible 21-clickhouse-drop-guard.yml)
# unless /var/lib/clickhouse/flags/force_drop_table exists. Armed for
# exactly one statement and removed right after (the server only consumes
# it when the drop was oversize; an unconsumed flag would silently permit
# the NEXT big drop). Only the phases that already demand an explicit
# D3_FORCE_DROP_* acknowledgement call this — never silently.
CH_FLAGS_DIR="${CH_FLAGS_DIR:-/var/lib/clickhouse/flags}"
guarded_ddl() {
  touch "$CH_FLAGS_DIR/force_drop_table"; chown clickhouse:clickhouse "$CH_FLAGS_DIR/force_drop_table" 2>/dev/null || true
  log "force_drop_table armed for: $1"
  local rc=0; q "$1" || rc=$?
  rm -f "$CH_FLAGS_DIR/force_drop_table"
  return $rc
}

BASE_COLS="entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr, intra_ledger_seq"

phase="${1:?phase: probe-ordinals|setup|reproject|verify|cutover|finalize|rollback-precutover}"

case "$phase" in

probe-ordinals)
  # Sample 1000 ledgers mid-band per 1M partition of the source append-log.
  # max_ord=0 for a band with real traffic ⇒ that band is un-ordinaled and the
  # v2 tie-break degrades to v1 behavior there (NOT worse — same arbitrary pick).
  TIP=$(q "SELECT max(ledger_seq) FROM stellar.ledger_entry_changes")
  MIN=$(q "SELECT min(ledger_seq) FROM stellar.ledger_entry_changes")
  log "append-log range: [$MIN, $TIP]"
  echo "partition_band  sample_rows  max_ord"
  for (( P=MIN/1000000; P<=TIP/1000000; P++ )); do
    LO=$(( P * 1000000 + 500000 )); HI=$(( LO + 999 ))
    q "SELECT '$P', count(), max(intra_ledger_seq) FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN $LO AND $HI FORMAT TSV"
  done
  ;;

setup)
  q "ALTER TABLE stellar.ledger_entry_changes ADD COLUMN IF NOT EXISTS intra_ledger_seq UInt32 DEFAULT 0 AFTER balance"
  q "CREATE TABLE IF NOT EXISTS stellar.ledger_entries_current_v2
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
     ORDER BY (entry_type, key_xdr)"
  q "CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.ledger_entries_current_v2_mv
     TO stellar.ledger_entries_current_v2 AS
     SELECT $BASE_COLS FROM stellar.ledger_entry_changes"
  # The MV captures everything from here forward; the historical reproject only
  # needs to reach this ledger. Record it once (do not clobber on re-run).
  if [ ! -f "$STATE_DIR/mv-created-at-tip" ]; then
    q "SELECT max(ledger_seq) FROM stellar.ledger_entry_changes" > "$STATE_DIR/mv-created-at-tip"
  fi
  log "setup done; MV capturing from tip=$(cat "$STATE_DIR/mv-created-at-tip")"
  ;;

reproject)
  FROM="${2:?reproject from-ledger}"; TO="${3:?reproject to-ledger (exclusive)}"
  PROG="$STATE_DIR/reproject-progress"
  [ -f "$PROG" ] && FROM_RESUME=$(cat "$PROG") || FROM_RESUME=$FROM
  if [ "$FROM_RESUME" -gt "$FROM" ]; then
    log "resuming at $FROM_RESUME (state file)"; FROM=$FROM_RESUME
  fi
  for (( CLO=FROM; CLO<TO; CLO+=CHUNK )); do
    CHI=$(( CLO + CHUNK )); [ "$CHI" -gt "$TO" ] && CHI=$TO
    q "INSERT INTO stellar.ledger_entries_current_v2 ($BASE_COLS)
       SELECT $BASE_COLS
       FROM stellar.ledger_entry_changes
       WHERE ledger_seq >= $CLO AND ledger_seq < $CHI"
    echo "$CHI" > "$PROG"
    log "  window [$CLO,$CHI) inserted"
  done
  log "reproject [$FROM,$TO) complete"
  ;;

verify)
  log "v1 coverage:"; q "SELECT count(), min(ledger_seq), max(ledger_seq) FROM stellar.ledger_entries_current"
  log "v2 coverage:"; q "SELECT count(), min(ledger_seq), max(ledger_seq) FROM stellar.ledger_entries_current_v2"
  log "divergence sample (v2_ils=0 rows are UNRESOLVED legacy ties, not corrections — rehearsal note):"
  q "SELECT key_xdr,
            v1.change_type AS v1_ct, v1.ledger_seq AS v1_ledger,
            v2.change_type AS v2_ct, v2.ledger_seq AS v2_ledger, v2.intra_ledger_seq AS v2_ils
     FROM (SELECT * FROM stellar.ledger_entries_current      FINAL) v1
     JOIN (SELECT * FROM stellar.ledger_entries_current_v2   FINAL) v2 USING (entry_type, key_xdr)
     WHERE v1.change_type != v2.change_type
     LIMIT 50
     FORMAT Vertical"
  ;;

cutover)
  # Capture the pre-cutover tip BEFORE dropping the MVs (the DDL gap loses MV
  # inserts; the catch-up below re-covers from this ledger).
  q "SELECT max(ledger_seq) FROM stellar.ledger_entry_changes" > "$STATE_DIR/pre-cutover-tip"
  TIPC=$(cat "$STATE_DIR/pre-cutover-tip")
  log "pre-cutover tip: $TIPC"
  q "DROP TABLE IF EXISTS stellar.ledger_entries_current_mv"
  q "DROP TABLE IF EXISTS stellar.ledger_entries_current_v2_mv"
  q "RENAME TABLE stellar.ledger_entries_current    TO stellar.ledger_entries_current_old,
                  stellar.ledger_entries_current_v2 TO stellar.ledger_entries_current"
  q "CREATE MATERIALIZED VIEW stellar.ledger_entries_current_mv
     TO stellar.ledger_entries_current AS
     SELECT $BASE_COLS FROM stellar.ledger_entry_changes"
  q "INSERT INTO stellar.ledger_entries_current ($BASE_COLS)
     SELECT $BASE_COLS
     FROM stellar.ledger_entry_changes
     WHERE ledger_seq >= $TIPC"
  log "cutover DONE (old table retained as ledger_entries_current_old; finalize drops it after a settling period)"
  ;;

finalize)
  if [ "${D3_FORCE_DROP_OLD:-}" != "yes" ]; then
    log "refusing: take a ZFS snapshot of data/clickhouse, then set D3_FORCE_DROP_OLD=yes to drop ledger_entries_current_old"; exit 1
  fi
  guarded_ddl "DROP TABLE stellar.ledger_entries_current_old SYNC"
  log "old table dropped"
  ;;

rollback-precutover)
  if [ "${D3_FORCE_DROP_V2:-}" != "yes" ]; then
    log "refusing: set D3_FORCE_DROP_V2=yes to drop ledger_entries_current_v2 (v1 is still serving; nothing is lost by waiting)"; exit 1
  fi
  q "DROP TABLE IF EXISTS stellar.ledger_entries_current_v2_mv"
  guarded_ddl "DROP TABLE IF EXISTS stellar.ledger_entries_current_v2 SYNC"
  log "v2 dropped; v1 never stopped serving"
  ;;

*) log "unknown phase: $phase"; exit 2 ;;
esac
