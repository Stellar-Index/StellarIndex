#!/usr/bin/env bash
# lake-dedup-driver.sh — force ReplacingMergeTree deduplication of the
# raw lake, partition by partition. Operator artifact (2026-07-25);
# companion runbook: docs/operations/lake-dedup-2026-07.md.
#
# WHY THIS EXISTS. The lake was ingested twice — a partial backfill in
# June 2026 (transactions ~96% of history, operations ~48%, LEC ~1.4%)
# and a full re-backfill on 2026-07-16/17. Copies are value-identical
# (verified: 0 disagreeing ledgers in sampled buckets; only ingested_at
# differs). ReplacingMergeTree dedups ONLY when parts merge, and old
# partitions (5-9 active parts, no new writes) never attract background
# merges — so ~34% of the lake's rows (~3 TiB of ~14 TiB CH-reported)
# are duplicates that will sit there FOREVER without an explicit
# OPTIMIZE ... FINAL. Measured dup mass by table:
#
#   transactions       ~50% of 2.13 TiB   (~1.05 TiB)
#   operations         ~32% of 2.99 TiB   (~0.97 TiB)
#   operation_results  ~32% of 2.03 TiB   (~0.66 TiB)
#   contract_events    ~28% of 772 GiB    (~0.21 TiB)
#   ledgers            ~49% of 23.6 GiB   (small)
#   ledger_entry_changes: ~1.4% of 6.17 TiB — EXCLUDED by default:
#     rewriting 6.17 TiB to reclaim ~90 GiB is poor ROI; revisit only
#     if the pool is desperate.
#
# SAFETY MODEL. OPTIMIZE ... FINAL is the engine's own merge, forced:
# it deletes nothing except RMT-duplicate rows (same ORDER BY key,
# keeps max ingested_at), is atomic per partition (readers see old or
# new parts, never a mix), and is idempotent. The partitions this
# touches are write-cold (historical); the live sink writes only near
# tip. Each merge needs scratch ≈ the partition's size (~20-40 GB);
# the driver refuses to start a partition without 3x that free.
#
# USAGE (on r1):
#   ./lake-dedup-driver.sh <table> [max_partitions]
#   DRY_RUN=1 ./lake-dedup-driver.sh transactions      # plan only
#   touch /tmp/lake-dedup.stop                          # graceful stop
#
# Order of execution across tables (biggest reclaim first):
#   transactions, operations, operation_results, contract_events, ledgers
#
# Progress + per-partition before/after row counts land in
# /var/log/lake-dedup-<table>.log. Every partition logs rows_before,
# rows_after, and dup_rows_removed — a partition whose counts do not
# shrink was already clean (the driver skips single-ingest-month
# partitions up front, so this should be rare).
set -uo pipefail

T="${1:?usage: lake-dedup-driver.sh <table> [max_partitions]}"
MAXP="${2:-9999}"
DRY_RUN="${DRY_RUN:-0}"
CH="${CH:-clickhouse-client --port 9300}"
OUT="${OUT:-/var/log/lake-dedup-${T}.log}"
STOP="${STOP:-/tmp/lake-dedup.stop}"

log() { echo "$(date -Iseconds) $*" | tee -a "$OUT"; }

# require_number <label> <value> — fail CLOSED unless $2 is a plain
# non-negative integer. A clickhouse-client failure (auth, OOM, server
# down) and unparseable tool output (e.g. a df mount surprise) both land
# here as the same shape — something other than digits — so this is the
# one check standing between either failure mode and an unguarded
# OPTIMIZE. `${p:+ partition=$p}` is set-u-safe even before $p exists.
require_number() {
  case "$2" in
    ''|*[!0-9]*)
      log "ABORT${p:+ partition=$p}: $1='$2' is not a number — refusing to guess"
      exit 1
      ;;
  esac
}

log "=== lake-dedup start table=$T max_partitions=$MAXP dry_run=$DRY_RUN ==="

# Partitions holding rows from MORE THAN ONE ingest month are the dup
# candidates (the two campaigns are a month apart). Single-month
# partitions are already clean and are skipped without a rewrite.
# Oldest first: coldest data, and failures surface before the big
# recent partitions are touched.
PARTS=$($CH -q "
  SELECT partition FROM (
    SELECT _partition_id AS partition, uniqExact(toYYYYMM(ingested_at)) AS months
    FROM stellar.${T} GROUP BY partition
  ) WHERE months > 1 ORDER BY toUInt32OrZero(partition) ASC" < /dev/null)
enum_rc=$?
if [ "$enum_rc" -ne 0 ]; then
  log "ABORT: partition-enumeration query FAILED rc=$enum_rc — NOT reporting success on an unknown partition list"
  exit 1
fi

# A legitimate zero (the query succeeded and found no dup candidates) is
# logged here, distinct from the ABORT line above that a failed query now
# takes instead — the two are never both reachable for the same run.
total=$(echo "$PARTS" | grep -c . || true)
log "dup-candidate partitions: $total"

n=0
for p in $PARTS; do
  [ -f "$STOP" ] && { log "STOP file present — exiting cleanly after $n partitions"; break; }
  n=$((n+1)); [ "$n" -gt "$MAXP" ] && { n=$((n-1)); log "max_partitions reached"; break; }

  stats=$($CH -q "
    SELECT sum(rows), sum(bytes_on_disk) FROM system.parts
    WHERE database='stellar' AND table='${T}' AND active AND partition='${p}'" < /dev/null)
  stats_rc=$?
  if [ "$stats_rc" -ne 0 ]; then
    log "ABORT partition=$p: partition-stats query FAILED rc=$stats_rc"
    exit 1
  fi
  read -r rows_before bytes_before <<<"$stats"
  require_number "rows_before" "$rows_before"
  require_number "bytes_before" "$bytes_before"

  # Scratch guard: require 3x the partition's on-disk size free. Both
  # operands are validated numeric above/below BEFORE this comparison —
  # an unparseable df line used to make `[ … -lt … ]` error (exit 2),
  # which `if` reads as false, skipping the ABORT and falling through to
  # an unguarded OPTIMIZE.
  free_bytes=$(df --output=avail -B1 /var/lib/clickhouse | tail -1 | tr -d ' ')
  require_number "free_bytes" "$free_bytes"
  if [ "$free_bytes" -lt $((bytes_before * 3)) ]; then
    log "ABORT partition=$p: free=$free_bytes < 3x partition=$bytes_before — pool too tight"
    exit 1
  fi

  if [ "$DRY_RUN" = "1" ]; then
    log "DRY partition=$p rows=$rows_before bytes=$bytes_before"
    continue
  fi

  t0=$(date +%s)
  $CH --receive_timeout 7200 -q "OPTIMIZE TABLE stellar.${T} PARTITION '${p}' FINAL" < /dev/null 2>>"$OUT"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    log "ABORT partition=$p: OPTIMIZE FAILED rc=$rc — see $OUT for clickhouse-client's stderr"
    exit 1
  fi
  rows_after=$($CH -q "
    SELECT sum(rows) FROM system.parts
    WHERE database='stellar' AND table='${T}' AND active AND partition='${p}'" < /dev/null)
  rows_after_rc=$?
  if [ "$rows_after_rc" -ne 0 ]; then
    log "ABORT partition=$p: post-OPTIMIZE row-count query FAILED rc=$rows_after_rc"
    exit 1
  fi
  require_number "rows_after" "$rows_after"
  log "partition=$p rc=$rc rows_before=$rows_before rows_after=$rows_after dup_removed=$((rows_before - rows_after)) elapsed=$(( $(date +%s) - t0 ))s"
done

log "=== done: $n partitions processed ==="
