#!/usr/bin/env bash
# D3 — ledger_entries_current version rebuild:
# ReplacingMergeTree(ledger_seq) → ReplacingMergeTree(version),
# version = (ledger_seq << 32) | intra_ledger_seq.
#
# Runs deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql (the
# operator-run migration artifact) as a phased, resumable script; read that
# file's header and rehearsal note first. The tie-break only works where
# intra_ledger_seq is populated in the source append-log (D2 partitions 39–53,
# Phase-0 re-derived ranges, live ingest ≥~63,550,000); `probe-ordinals` shows
# actual coverage. cutover swaps the SERVED current-state table: ATTENDED ONLY.
# Acceptance: `reconcile-balances -sample 50` reports 0 mismatches.
#
# Phases (in order; each resumable/idempotent):
#   probe-ordinals            per-partition sample of ordinal coverage
#   setup                     Step 0 (ADD COLUMN) + Step 1 (v2 table + MV);
#                             records the MV-creation tip in the state dir
#   reproject <from> <to>     Step 2: windowed INSERT of [from,to) into v2;
#                             overlapping re-runs are safe (RMT). Resume state
#                             is keyed by <from>, so windows never share progress
#   verify                    Step 3: v1-vs-v2 divergence sample + coverage
#   cutover                   Step 4: REFUSES unless v2 covers v1 (count,
#                             min/max ledger_seq) unless D3_FORCE_CUTOVER=yes;
#                             then drop MVs, double-RENAME, recreate MV,
#                             catch up from the pre-cutover tip
#   finalize                  Step 5: DROP _old (requires D3_FORCE_DROP_OLD=yes)
#   rollback-precutover       drop v2 + its MV (v1 never stopped serving)
#
# Heavy phases run under the wrapper:
#   run-heavy-job.sh d3-reproject /usr/local/sbin/d3-lecur-v2-rebuild.sh reproject <v1-floor> <mv-created-at-tip + 1>
# <v1-floor> = SELECT min(ledger_seq) FROM stellar.ledger_entries_current;
# <mv-created-at-tip> = cat $STATE_DIR/mv-created-at-tip. The +1 matters: TO is
# exclusive, and that ledger's rows predate the MV rather than coming from it.
set -euo pipefail

# Optional ops-user credentials (STELLARINDEX_CLICKHOUSE_OPS_USER/_PASSWORD,
# e.g. from /etc/default/stellarindex-ops), passed to clickhouse-client via
# CLICKHOUSE_USER/CLICKHOUSE_PASSWORD env, never argv (visible to ps/journal).
if [ -n "${STELLARINDEX_CLICKHOUSE_OPS_USER:-}" ]; then
  export CLICKHOUSE_USER="$STELLARINDEX_CLICKHOUSE_OPS_USER"
  export CLICKHOUSE_PASSWORD="${STELLARINDEX_CLICKHOUSE_OPS_PASSWORD:-}"
fi
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
# DROP TABLE above max_table_size_to_drop (50 GiB, ansible
# 21-clickhouse-drop-guard.yml) is refused unless the force_drop_table flag
# exists. Armed for exactly one statement and removed right after (an
# unconsumed flag would permit the NEXT big drop). Only phases that already
# demand an explicit D3_FORCE_DROP_* acknowledgement call this.
CH_FLAGS_DIR="${CH_FLAGS_DIR:-/var/lib/clickhouse/flags}"
guarded_ddl() {
  # a SIGTERM between touch and rm must not leave the flag armed
  trap 'rm -f "$CH_FLAGS_DIR/force_drop_table"' EXIT
  touch "$CH_FLAGS_DIR/force_drop_table"; chown clickhouse:clickhouse "$CH_FLAGS_DIR/force_drop_table" 2>/dev/null || true
  log "force_drop_table armed for: $1"
  local rc=0; q "$1" || rc=$?
  rm -f "$CH_FLAGS_DIR/force_drop_table"
  return $rc
}

# num <value> <what> — fail closed unless a plain non-negative integer.
# clickhouse-client reports errors as TEXT and a failed query yields an EMPTY
# capture, so an unchecked $(…) in `[ -gt ]` would die with a raw shell error
# or be interpolated into the next statement.
num() {
  case "$1" in
    ''|*[!0-9]*) log "refusing: $2 is not a non-negative integer ('$1')"; exit 1 ;;
  esac
}

# coverage_of <table> — TSV "count min max" over ledger_seq. Raw rows, not
# FINAL: the gate needs only aggregates, and the FINAL scan is `verify`'s cost.
coverage_of() {
  q "SELECT count(), min(ledger_seq), max(ledger_seq) FROM stellar.$1 FORMAT TSV"
}

BASE_COLS="entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr, intra_ledger_seq"

phase="${1:?phase: probe-ordinals|setup|reproject|verify|cutover|finalize|rollback-precutover}"

case "$phase" in

probe-ordinals)
  # Sample 1000 ledgers mid-band per 1M partition of the source append-log.
  # max_ord=0 for a band with real traffic means it is un-ordinaled and the v2
  # tie-break degrades to v1 behavior there (not worse).
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
  # The MV captures everything from here on; the reproject only needs to reach
  # this ledger. Recorded once, not clobbered on re-run.
  if [ ! -f "$STATE_DIR/mv-created-at-tip" ]; then
    q "SELECT max(ledger_seq) FROM stellar.ledger_entry_changes" > "$STATE_DIR/mv-created-at-tip"
  fi
  log "setup done; MV capturing from tip=$(cat "$STATE_DIR/mv-created-at-tip")"
  ;;

reproject)
  FROM="${2:?reproject from-ledger}"; TO="${3:?reproject to-ledger (exclusive)}"
  num "$FROM" "reproject from-ledger"; num "$TO" "reproject to-ledger"
  if [ "$FROM" -ge "$TO" ]; then
    log "refusing: [$FROM,$TO) is an empty window — from must be below to"; exit 1
  fi
  # Not a refusal: windows are chunked, so a lower TO in an earlier chunk is normal.
  if [ -f "$STATE_DIR/mv-created-at-tip" ]; then
    MVTIP=$(cat "$STATE_DIR/mv-created-at-tip")
    num "$MVTIP" "mv-created-at-tip"
    if [ "$TO" -le "$MVTIP" ]; then
      log "warning: to-ledger $TO does not exceed mv-created-at-tip=$MVTIP — this window alone will not close the MV-creation gap; a later window must reach at least $((MVTIP + 1))"
    fi
  fi
  REQ_FROM=$FROM
  # Progress is keyed by the window's FROM; a mark means "[FROM,mark) is
  # inserted". A shared unkeyed file let a LOWER window adopt a HIGHER window's
  # mark, iterate zero chunks and log "complete" — and lower-after-higher is the
  # normal order (phaseD-backfill.sh walks [54000000,63050000] then
  # [2,38000000]). TO is deliberately not in the key: the runbook's tip moves
  # between runs, and a TO-keyed file would restart the window each time.
  PROG="$STATE_DIR/reproject-progress.from-$FROM"
  LEGACY="$STATE_DIR/reproject-progress"
  if [ ! -f "$PROG" ] && [ -f "$LEGACY" ]; then
    log "ignoring unkeyed legacy progress file $LEGACY (=$(cat "$LEGACY")): it names no window, so it cannot be read as progress through [$FROM,$TO). Re-inserting an already-covered chunk is idempotent under the RMT; the cost is time, not correctness."
  fi
  if [ -f "$PROG" ]; then
    FROM_RESUME=$(cat "$PROG")
    num "$FROM_RESUME" "progress mark in $PROG"
    if [ "$FROM_RESUME" -lt "$FROM" ]; then
      log "refusing: progress mark $FROM_RESUME in $PROG is below that file's own window start $FROM — the state is corrupt; delete the file to redo the window"; exit 1
    fi
    if [ "$FROM_RESUME" -ge "$TO" ]; then
      log "nothing to do: [$REQ_FROM,$TO) is already covered (mark=$FROM_RESUME in $PROG)"; exit 0
    fi
    log "resuming at $FROM_RESUME (state file $PROG)"; FROM=$FROM_RESUME
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
  log "reproject [$REQ_FROM,$TO) complete (this run inserted [$FROM,$TO))"
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
  # ── Coverage gate (runs BEFORE anything is dropped or renamed) ──────
  # RENAME is where rollback-precutover stops applying: an incomplete v2 over a
  # complete v1 is data loss no later phase can undo. The floor rule is the
  # artifact's Step 2 (deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql):
  # min(ledger_seq) in v1 preserves the coverage floor; going lower closes it.
  # A fixed 38,000,000 start therefore RAISES the floor when v1 already reaches
  # below it (Phase D fills [2,38000000]). Refusal is the default;
  # D3_FORCE_CUTOVER=yes is the same explicit acknowledgement finalize and
  # rollback-precutover demand.
  read -r V1C V1MIN V1MAX <<< "$(coverage_of ledger_entries_current)"
  read -r V2C V2MIN V2MAX <<< "$(coverage_of ledger_entries_current_v2)"
  num "$V1C"   "v1 count()";          num "$V2C"   "v2 count()"
  num "$V1MIN" "v1 min(ledger_seq)";  num "$V2MIN" "v2 min(ledger_seq)"
  num "$V1MAX" "v1 max(ledger_seq)";  num "$V2MAX" "v2 max(ledger_seq)"
  log "v1 coverage: count=$V1C min=$V1MIN max=$V1MAX"
  log "v2 coverage: count=$V2C min=$V2MIN max=$V2MAX"
  NVIOL=0
  viol() { log "  cutover check FAILED: $1"; NVIOL=$(( NVIOL + 1 )); }
  if [ "$V2C" -eq 0 ]; then
    viol "v2 is EMPTY — the reproject never ran against this table"
  elif [ "$V2C" -lt "$V1C" ]; then
    # Both are ReplacingMergeTrees on the same ORDER BY, so counts converge at
    # full merge; a fresh v2 normally holds MORE rows (unmerged parts). A small
    # deficit can be pure merge state, which is what the override is for.
    viol "v2 holds fewer rows than v1 ($V2C < $V1C)"
  fi
  if [ "$V2MIN" -gt "$V1MIN" ]; then
    viol "coverage floor would REGRESS: v2 min(ledger_seq)=$V2MIN is above v1's $V1MIN — reproject down to $V1MIN first"
  fi
  if [ "$V2MAX" -lt "$V1MAX" ]; then
    viol "v2 lags the tip: v2 max(ledger_seq)=$V2MAX is below v1's $V1MAX — the v2 MV is not capturing live ingest"
  fi
  # The MV-creation ledger's rows predate the MV, so only a reproject past it,
  # never the min/max/count gate, closes that hole (r1 2026-09: 221 contract_data,
  # 221 ttl, 6 stale offers).
  if [ -f "$STATE_DIR/mv-created-at-tip" ]; then
    MVTIP=$(cat "$STATE_DIR/mv-created-at-tip")
    num "$MVTIP" "mv-created-at-tip"
    COVERED=0
    for f in "$STATE_DIR"/reproject-progress.from-*; do
      [ -f "$f" ] || continue
      MARK=$(cat "$f")
      WFROM=${f##*/reproject-progress.from-}
      case "$MARK$WFROM" in ''|*[!0-9]*) continue ;; esac
      if [ "$WFROM" -le "$MVTIP" ] && [ "$MARK" -gt "$MVTIP" ]; then COVERED=1; break; fi
    done
    if [ "$COVERED" -ne 1 ]; then
      viol "no reproject window spans mv-created-at-tip=$MVTIP (from <= tip < mark) — ledger $MVTIP was inserted before the MV existed and no reproject has covered it yet (reproject <v1-floor> $((MVTIP + 1)) closes the gap)"
    fi
  fi
  if [ "$NVIOL" -gt 0 ]; then
    if [ "${D3_FORCE_CUTOVER:-}" != "yes" ]; then
      log "refusing cutover: $NVIOL coverage check(s) failed. Nothing was dropped or renamed and v1 is still serving. Close the gap (reproject the missing window), or re-run with D3_FORCE_CUTOVER=yes once \`verify\` explains the difference."
      exit 1
    fi
    log "D3_FORCE_CUTOVER=yes — proceeding over $NVIOL coverage violation(s)"
  fi
  # Capture the tip BEFORE dropping the MVs (the DDL gap loses MV inserts; the
  # catch-up below re-covers from this ledger).
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
