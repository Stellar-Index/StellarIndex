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
# shrink was already clean (the driver skips partitions with zero
# measured duplicate rows up front, so this should be rare).
#
# Progress also lands as node_exporter textfile-collector gauges
# (stellarindex_lake_dedup_*, in <TEXTFILE_DIR>/lake_dedup_<table>.prom),
# written tmp-then-mv like ch-schema-drift.sh's emit_intent_metrics, so
# a multi-day run is visible to monitoring. Every exit, including an
# ABORT or SIGTERM/SIGINT/SIGHUP, writes running=0 and last_exit_ok.
# TEXTFILE_DIR=/dev/null opts out, same convention as that emitter.
set -uo pipefail

T="${1:?usage: lake-dedup-driver.sh <table> [max_partitions]}"
# $T is spliced into SQL and into the log path below: accept only the lake tables named above.
case "$T" in
  transactions|operations|operation_results|contract_events|ledgers|ledger_entry_changes) ;;
  *) echo "lake-dedup-driver: refusing table '$T' — not one of the lake tables this driver dedups" >&2; exit 2 ;;
esac
MAXP="${2:-9999}"
DRY_RUN="${DRY_RUN:-0}"
CH="${CH:-clickhouse-client --port 9300}"
OUT="${OUT:-/var/log/lake-dedup-${T}.log}"
STOP="${STOP:-/tmp/lake-dedup.stop}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
METRICS_OUT="$TEXTFILE_DIR/lake_dedup_${T}.prom"
PROGRESS_FILE="$METRICS_OUT.progress"

log() { echo "$(date -Iseconds) $*" | tee -a "$OUT"; }

# write_progress <processed> — records live partition-progress where the
# background heartbeat ticker (a forked subshell — see below) can read
# it. A forked subshell only ever sees $n as of the instant it forked,
# never a later increment in the parent, so progress has to cross that
# boundary through a file rather than a variable.
write_progress() {
  [ "$TEXTFILE_DIR" = "/dev/null" ] && return 0
  echo "$1" > "$PROGRESS_FILE" 2>/dev/null || true
}

# emit_metrics <running> <exit_ok-or-empty> — atomic textfile-collector
# write. exit_ok is only meaningful (and only emitted) once the run has
# ended; mid-run calls pass "".
emit_metrics() {
  [ "$TEXTFILE_DIR" = "/dev/null" ] && return 0
  local running="$1" exit_ok="$2" processed tmp
  processed=$(cat "$PROGRESS_FILE" 2>/dev/null || echo 0)
  mkdir -p "$TEXTFILE_DIR" 2>/dev/null
  tmp="${METRICS_OUT}.tmp.$$"
  {
    echo "# HELP stellarindex_lake_dedup_running 1 while lake-dedup-driver is executing this table, 0 once it has exited."
    echo "# TYPE stellarindex_lake_dedup_running gauge"
    echo "stellarindex_lake_dedup_running{table=\"$T\"} $running"
    echo "# HELP stellarindex_lake_dedup_heartbeat_unix Unix time of the most recent liveness write, rewritten every 60s independent of partition progress so a stalled OPTIMIZE is distinguishable from a dead process."
    echo "# TYPE stellarindex_lake_dedup_heartbeat_unix gauge"
    echo "stellarindex_lake_dedup_heartbeat_unix{table=\"$T\"} $(date +%s)"
    echo "# HELP stellarindex_lake_dedup_partitions_total Dup-candidate partitions found for the current run."
    echo "# TYPE stellarindex_lake_dedup_partitions_total gauge"
    echo "stellarindex_lake_dedup_partitions_total{table=\"$T\"} ${total:-0}"
    echo "# HELP stellarindex_lake_dedup_partitions_processed Partitions this run has finished OPTIMIZE-ing (or planned, under DRY_RUN) so far."
    echo "# TYPE stellarindex_lake_dedup_partitions_processed gauge"
    echo "stellarindex_lake_dedup_partitions_processed{table=\"$T\"} $processed"
    if [ -n "$exit_ok" ]; then
      echo "# HELP stellarindex_lake_dedup_last_exit_ok 1 when the most recent run for this table exited cleanly, 0 when it aborted."
      echo "# TYPE stellarindex_lake_dedup_last_exit_ok gauge"
      echo "stellarindex_lake_dedup_last_exit_ok{table=\"$T\"} $exit_ok"
    fi
  } > "$tmp" 2>/dev/null
  chmod 644 "$tmp" 2>/dev/null
  mv "$tmp" "$METRICS_OUT" 2>/dev/null
}

# finish_metrics <rc> — stop the heartbeat ticker and any in-flight
# OPTIMIZE client, then write the terminal state. Runs as the EXIT trap
# on every exit, including the enumeration ABORT before `total` is known.
finish_metrics() {
  local rc="$1"
  if [ -n "${opt_pid:-}" ]; then
    kill "$opt_pid" 2>/dev/null
    wait "$opt_pid" 2>/dev/null
  fi
  if [ -n "${hb_pid:-}" ]; then
    kill "$hb_pid" 2>/dev/null
    wait "$hb_pid" 2>/dev/null
  fi
  if [ "$rc" -eq 0 ]; then emit_metrics 0 1; else emit_metrics 0 0; fi
  rm -f "$PROGRESS_FILE" 2>/dev/null
}
# On a signal the EXIT trap alone sees the last command's status (often
# 0), so each signal exits 128+signo explicitly and reads as a failure.
on_signal() { log "ABORT: received SIG$1 — stopping"; exit "$2"; }
trap 'finish_metrics "$?"' EXIT
trap 'on_signal HUP 129' HUP
trap 'on_signal INT 130' INT
trap 'on_signal TERM 143' TERM
# Reset before anything can abort, so no exit path reports a stale
# progress file left by a killed earlier run.
write_progress 0

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

# The table's own ORDER BY key is the real duplicate identity: two rows
# with the same key are the same ledger row seen twice (once per ingest
# campaign), whatever their ingested_at happens to be. A months>1 proxy
# both false-negatives (both campaigns' rows landing in the same
# calendar month, e.g. a backfill re-run near a month boundary) and
# false-positives (a partition that legitimately spans two months with
# no duplicates at all, paying for an OPTIMIZE that finds nothing).
SORTKEY=$($CH -q "SELECT sorting_key FROM system.tables WHERE database='stellar' AND name='${T}'" < /dev/null)
sortkey_rc=$?
if [ "$sortkey_rc" -ne 0 ] || [ -z "$SORTKEY" ]; then
  log "ABORT: sorting-key lookup FAILED rc=$sortkey_rc sortkey='$SORTKEY' — cannot compute real dup counts"
  exit 1
fi

# Partitions where the row count exceeds the distinct-key count actually
# hold duplicate rows under the table's own ORDER BY — this is a direct
# measurement, not a proxy. Oldest first: coldest data, and failures
# surface before the big recent partitions are touched.
PARTS=$($CH -q "
  SELECT partition FROM (
    SELECT _partition_id AS partition, count() - uniqExact((${SORTKEY})) AS dup_rows
    FROM stellar.${T} GROUP BY partition
  ) WHERE dup_rows > 0 ORDER BY toUInt32OrZero(partition) ASC" < /dev/null)
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

emit_metrics 1 ""
if [ "$TEXTFILE_DIR" != "/dev/null" ]; then
  # $$ is the driver's pid inside the subshell too: stop ticking if the
  # driver was SIGKILLed, or a dead run would keep reporting running=1.
  ( while sleep 60; do kill -0 "$$" 2>/dev/null || exit 0; emit_metrics 1 ""; done ) &
  hb_pid=$!
fi

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
    write_progress "$n"
    continue
  fi

  t0=$(date +%s)
  # Backgrounded and waited on so a signal is handled now, not after a
  # merge that can run up to --receive_timeout.
  $CH --receive_timeout 7200 -q "OPTIMIZE TABLE stellar.${T} PARTITION '${p}' FINAL" < /dev/null 2>>"$OUT" &
  opt_pid=$!
  wait "$opt_pid"
  rc=$?
  opt_pid=""
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
  write_progress "$n"
done

log "=== done: $n partitions processed ==="
