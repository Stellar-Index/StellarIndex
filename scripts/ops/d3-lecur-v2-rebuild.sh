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
#   cutover                   Step 4: REFUSES unless v2 covers v1 (count,
#                             min/max ledger_seq) — D3_FORCE_CUTOVER=yes to
#                             override; then drop MVs, double-RENAME,
#                             recreate MV, catch-up from the pre-cutover tip
#   finalize                  Step 5: DROP _old (requires D3_FORCE_DROP_OLD=yes)
#   rollback-precutover       drop v2 + its MV (v1 never stopped serving)
#
# Heavy phases (reproject) run under the wrapper:
#   run-heavy-job.sh d3-reproject /usr/local/sbin/d3-lecur-v2-rebuild.sh reproject 38000000 63700000
set -euo pipefail

# Optional ops-user credentials (STELLARINDEX_CLICKHOUSE_OPS_USER/_PASSWORD,
# e.g. from /etc/default/stellarindex-ops). Handed to clickhouse-client via its
# CLICKHOUSE_USER/CLICKHOUSE_PASSWORD env — never argv, which ps and the journal
# would show. Unset ⇒ nothing exported; the default user exactly as before.
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
# the DROP TABLE of ledger_entries_current_old / _v2 is refused above
# max_table_size_to_drop (50 GiB, ansible 21-clickhouse-drop-guard.yml)
# unless /var/lib/clickhouse/flags/force_drop_table exists. Armed for
# exactly one statement and removed right after (the server only consumes
# it when the drop was oversize; an unconsumed flag would silently permit
# the NEXT big drop). Only the phases that already demand an explicit
# D3_FORCE_DROP_* acknowledgement call this — never silently.
CH_FLAGS_DIR="${CH_FLAGS_DIR:-/var/lib/clickhouse/flags}"
guarded_ddl() {
  # belt-and-braces: a SIGTERM between touch and rm must not leave the flag armed
  trap 'rm -f "$CH_FLAGS_DIR/force_drop_table"' EXIT
  touch "$CH_FLAGS_DIR/force_drop_table"; chown clickhouse:clickhouse "$CH_FLAGS_DIR/force_drop_table" 2>/dev/null || true
  log "force_drop_table armed for: $1"
  local rc=0; q "$1" || rc=$?
  rm -f "$CH_FLAGS_DIR/force_drop_table"
  return $rc
}

# num <value> <what> — fail closed on anything that is not a plain
# non-negative integer. clickhouse-client reports an error as TEXT and a
# failed query yields an EMPTY capture, so an unchecked $(…) fed to
# `[ -gt ]` would abort a phase with a raw shell error (or, worse, be
# interpolated into the next statement) instead of a stated refusal.
num() {
  case "$1" in
    ''|*[!0-9]*) log "refusing: $2 is not a non-negative integer ('$1')"; exit 1 ;;
  esac
}

# coverage_of <table> — one TSV row "count min max" over ledger_seq. Raw
# rows, not FINAL: the cutover gate only needs the aggregates the `verify`
# phase already prints, and a FINAL scan of both tables is the expensive
# part of `verify`, not of a few ms of DDL.
coverage_of() {
  q "SELECT count(), min(ledger_seq), max(ledger_seq) FROM stellar.$1 FORMAT TSV"
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
  # ── Coverage gate (runs BEFORE anything is dropped or renamed) ──────
  # The RENAME below is the moment rollback-precutover stops applying: an
  # incomplete v2 swapped over a complete v1 is a data loss no later phase
  # can undo. The floor rule is the migration artifact's own — Step 2 of
  # deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql: "the
  # min(ledger_seq) currently in stellar.ledger_entries_current preserves
  # today's coverage floor; going lower additionally closes that floor".
  # The launch plan's `reproject 38000000 <tip>` therefore RAISES the floor
  # whenever v1 already reaches below 38,000,000, and nothing here would
  # have noticed. Refusal is the default; D3_FORCE_CUTOVER=yes is the same
  # explicit acknowledgement finalize and rollback-precutover demand.
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
    # Both are ReplacingMergeTrees over the same ORDER BY, so at full merge
    # the two counts converge; a freshly built v2 normally holds MORE rows
    # (unmerged parts), never fewer. A small deficit CAN be nothing but
    # merge state — that case is what the override exists for.
    viol "v2 holds fewer rows than v1 ($V2C < $V1C)"
  fi
  if [ "$V2MIN" -gt "$V1MIN" ]; then
    viol "coverage floor would REGRESS: v2 min(ledger_seq)=$V2MIN is above v1's $V1MIN — reproject down to $V1MIN first"
  fi
  if [ "$V2MAX" -lt "$V1MAX" ]; then
    viol "v2 lags the tip: v2 max(ledger_seq)=$V2MAX is below v1's $V1MAX — the v2 MV is not capturing live ingest"
  fi
  if [ "$NVIOL" -gt 0 ]; then
    if [ "${D3_FORCE_CUTOVER:-}" != "yes" ]; then
      log "refusing cutover: $NVIOL coverage check(s) failed. Nothing was dropped or renamed and v1 is still serving. Close the gap (reproject the missing window), or re-run with D3_FORCE_CUTOVER=yes once \`verify\` explains the difference."
      exit 1
    fi
    log "D3_FORCE_CUTOVER=yes — proceeding over $NVIOL coverage violation(s)"
  fi
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
