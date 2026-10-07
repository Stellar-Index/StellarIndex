#!/bin/bash
# rederive-from.sh — re-derive all history from ledger L in one command:
#   1. lake: ch-backfill every 1M-ledger window of [L,TO] from galexie-archive,
#      -workers windows at a time, each through phaseD-range.sh (retry cap,
#      disk guard, done-state);
#   2. seed stellar.supply_flows over [L,TO] (ch-supply-flows-seed.sh);
#   3. projected-rebuild -from L for each -projected source, behind the live
#      projector's tail (its one-writer guard stays on);
#   4. seed-entry-counts, so the counters match the rebuilt tables.
# Workers pull windows newest-first from one queue rather than owning fixed
# contiguous shards: recent windows hold far more rows than early ones, so
# fixed shards leave the run waiting on whichever worker owns the tip.
#
# Resume: state lives in -state-dir (default keyed by L). A window is recorded
# only after ch-backfill exits 0 and each later stage leaves a done marker, so
# re-running the same command skips finished work at any -workers. TO is
# pinned on the first run: a moved end would leave the last window short.
# Every step is idempotent (RMT lake, ON CONFLICT DO NOTHING served tier).
#
# Memory scales with -workers x -parallel ch-backfill goroutines; see
# ordinal-rederive-chunks.sh for the measured per-goroutine cost. Run under
# run-heavy-job.sh on r1:
#   run-heavy-job.sh rederive /usr/local/sbin/rederive-from.sh -from L \
#     -workers 6 -parallel 3 -projected soroswap,aquarius,phoenix
set -uo pipefail

usage() {
  echo "usage: rederive-from.sh -from L [-to T] [-workers N] [-parallel P] [-projected CSV] [-state-dir DIR]" >&2
  exit 2
}

FROM="" TO="" WORKERS=4 PAR=4 PROJECTED="" SDIR=""
while [ $# -gt 0 ]; do
  [ $# -ge 2 ] || usage
  case "$1" in
    -from) FROM=$2 ;;
    -to) TO=$2 ;;
    -workers) WORKERS=$2 ;;
    -parallel) PAR=$2 ;;
    -projected) PROJECTED=$2 ;;
    -state-dir) SDIR=$2 ;;
    *) usage ;;
  esac
  shift 2
done
posint() { case "$1" in ''|*[!0-9]*|0*) return 1 ;; esac; }
posint "$FROM" || { echo "rederive-from: -from '$FROM' must be a positive ledger" >&2; usage; }
[ -z "$TO" ] || posint "$TO" || { echo "rederive-from: -to '$TO' must be a positive ledger" >&2; usage; }
posint "$WORKERS" || { echo "rederive-from: -workers '$WORKERS' must be a positive integer" >&2; usage; }
posint "$PAR" || { echo "rederive-from: -parallel '$PAR' must be a positive integer" >&2; usage; }
if [ -n "$PROJECTED" ]; then
  case ",$PROJECTED," in *[!a-z0-9_,]*|*,,*) echo "rederive-from: -projected '$PROJECTED' must be a comma-separated list of source names" >&2; usage ;; esac
fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG="${REDERIVE_LOG:-/var/log/rederive-from.log}"
SDIR="${SDIR:-/var/lib/ch-backfill/rederive-from-$FROM}"
CONFIG=/etc/stellarindex.toml
WINDOW=1000000   # phaseD-range.sh's window; window starts are FROM + k*WINDOW

# Read systemd EnvironmentFiles verbatim, never `.`/source: their values are
# unquoted, so the shell would mangle a secret. Kept in lockstep by envfile-loader-test.sh.
load_env_file() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      [A-Za-z_]*=*)
        if [ "${2:-}" = export ]; then
          export "${line?}"
        else
          printf -v "${line%%=*}" '%s' "${line#*=}"
        fi
        ;;
    esac
  done < "$1"
}
for f in /etc/default/stellarindex-ops /etc/default/stellarindex; do
  [ -r "$f" ] && load_env_file "$f" export
done
OPS="${REDERIVE_OPS:-/usr/local/bin/stellarindex-ops}"

log() { echo "$(date -u +%FT%TZ) [rederive] $*" >> "$LOG"; }
die() { log "$1"; echo "rederive-from: $1" >&2; exit "${2:-1}"; }

mkdir -p "$SDIR" || die "cannot create state dir $SDIR"
# One run per state dir: a second would double every worker's memory. A lock
# whose holder is gone (kill -9) is taken over so a resume never needs a hand.
LOCK="$SDIR/lock"
if ! mkdir "$LOCK" 2>/dev/null; then
  holder="$(cat "$LOCK/pid" 2>/dev/null)"
  if [ -n "$holder" ] && kill -0 "$holder" 2>/dev/null; then
    die "another run (pid $holder) holds $SDIR"
  fi
  rm -rf "$LOCK"; mkdir "$LOCK" || die "cannot take lock $LOCK"
fi
echo "$$" > "$LOCK/pid"
trap 'rm -rf "$LOCK"' EXIT

if [ -s "$SDIR/to" ]; then
  pinned="$(cat "$SDIR/to")"
  [ -z "$TO" ] || [ "$TO" = "$pinned" ] || die "-to $TO differs from the pinned end $pinned in $SDIR/to; use a new -state-dir to change it" 2
  TO=$pinned
elif [ -z "$TO" ]; then
  # Same ops-credential handover as ch-supply-flows-seed.sh: env, never argv.
  if [ -n "${STELLARINDEX_CLICKHOUSE_OPS_USER:-}" ]; then
    export CLICKHOUSE_USER="$STELLARINDEX_CLICKHOUSE_OPS_USER"
    export CLICKHOUSE_PASSWORD="${STELLARINDEX_CLICKHOUSE_OPS_PASSWORD:-}"
  fi
  TO="$(clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.ledgers")" || die "could not read the lake tip to pin -to"
fi
posint "$TO" || die "end ledger '$TO' is not a positive integer" 2
[ "$TO" -ge "$FROM" ] || die "end ledger $TO is below -from $FROM" 2
echo "$TO" > "$SDIR/to"

WSTATE="$SDIR/windows.txt"; touch "$WSTATE"
WINDOWS=()
for (( w=FROM; w<=TO; w+=WINDOW )); do WINDOWS=("$w" ${WINDOWS[@]+"${WINDOWS[@]}"}); done
CLAIMS="$LOCK/claims"; mkdir -p "$CLAIMS"

# worker: claim the next undone window (mkdir is the atomic claim) until the
# queue is empty or another worker has failed.
worker() {
  local w wto rc
  for w in "${WINDOWS[@]}"; do
    [ -e "$LOCK/failed" ] && return 0
    grep -qx "$w" "$WSTATE" && continue
    mkdir "$CLAIMS/$w" 2>/dev/null || continue
    wto=$((w + WINDOW - 1)); [ "$wto" -gt "$TO" ] && wto=$TO
    PHASED_LOG="$LOG" PHASED_OPS="$OPS" bash "$HERE/phaseD-range.sh" "$w" "$wto" "$WSTATE" "$PAR" || {
      rc=$?
      echo "$w $rc" >> "$LOCK/failed"
      return "$rc"
    }
  done
}

# kill_tree PID: TERM a worker and everything under it, so no ch-backfill
# outlives the run and races the resumed one. Each process is stopped before
# its children are listed, so it cannot start a window the listing misses.
kill_tree() {
  local c
  kill -STOP "$1" 2>/dev/null
  for c in $(pgrep -P "$1" 2>/dev/null); do kill_tree "$c"; done
  kill -TERM "$1" 2>/dev/null
  kill -CONT "$1" 2>/dev/null
}
PIDS=()
on_signal() {
  log "SIGNAL $1 — stopping workers; re-run the same command to resume"
  local p
  for p in ${PIDS[@]+"${PIDS[@]}"}; do kill_tree "$p"; done
  wait
  exit 143
}
trap 'on_signal TERM' TERM
trap 'on_signal INT' INT

log "START [$FROM,$TO] windows=${#WINDOWS[@]} workers=$WORKERS parallel=$PAR projected=${PROJECTED:-none} state=$SDIR"
for (( i=0; i<WORKERS && i<${#WINDOWS[@]}; i++ )); do
  worker & PIDS+=("$!")
done
lake_rc=0
for p in "${PIDS[@]}"; do
  wait "$p" || { rc=$?; [ "$lake_rc" -ne 0 ] || lake_rc=$rc; }
done
PIDS=()
if [ -e "$LOCK/failed" ] || [ "$lake_rc" -ne 0 ]; then
  [ "$lake_rc" -ne 0 ] || lake_rc=1
  die "LAKE FAILED (window rc: $(tr '\n' ' ' < "$LOCK/failed" 2>/dev/null)) — re-run to resume" "$lake_rc"
fi
missing=0
for w in "${WINDOWS[@]}"; do grep -qx "$w" "$WSTATE" || missing=$((missing + 1)); done
[ "$missing" -eq 0 ] || die "LAKE INCOMPLETE: $missing window(s) not recorded done — re-run to resume"
log "LAKE DONE [$FROM,$TO]"

# stage NAME CMD…: run CMD once; its done marker makes a resume skip it.
stage() {
  local name=$1; shift
  if [ -e "$SDIR/done.$name" ]; then log "$name already done — skipped"; return 0; fi
  log "$name START"
  local rc
  "$@" >> "$LOG" 2>&1 || { rc=$?; die "$name FAILED (exit $rc) — re-run to resume" "$rc"; }
  touch "$SDIR/done.$name"
  log "$name DONE"
}
stage supply-flows env FROM="$FROM" TO="$TO" OPS="$OPS" CFG="$CONFIG" STATE="$SDIR/supply-flows.txt" \
  bash "$HERE/ch-supply-flows-seed.sh"
IFS=, read -r -a SOURCES <<< "$PROJECTED"
for src in ${SOURCES[@]+"${SOURCES[@]}"}; do
  # No -to: projected-rebuild then stops at the live projector's cursor.
  stage "projected.$src" "$OPS" projected-rebuild -config "$CONFIG" -source "$src" -from "$FROM" -workers "$WORKERS" -write
done
stage entry-counts "$OPS" seed-entry-counts -config "$CONFIG" -write
log "REDERIVE_COMPLETE [$FROM,$TO]"
