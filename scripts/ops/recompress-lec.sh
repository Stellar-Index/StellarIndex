#!/bin/bash
# Phase A — recompress ledger_entry_changes full-fidelity partitions [38..53] to
# ZSTD (codec already set via MODIFY COLUMN). Per-partition, biggest-first,
# disk-guarded. Skips degraded partitions (Phase D overwrites them) and the live
# tip (new inserts already land ZSTD). Idempotent: re-running just re-OPTIMIZEs
# (cheap no-op on already-ZSTD partitions). Resumable: safe to restart.
# RECOMPRESS_COMPLETE is logged, and the exit is 0, only when every partition
# was rewritten; a failed query or a skipped partition exits non-zero.
set -uo pipefail
LOG="${RECOMPRESS_LOG:-/var/log/recompress-lec.log}"
FLOOR_KB=524288000   # 500 GiB abort floor
# ClickHouse reports an exception as HTTP 500 with the message in the body;
# without --fail-with-body curl exits 0 and the error reads as a result.
CH(){ curl -sS --fail-with-body 'http://localhost:8123/' --data-binary "$1"; }
log(){ echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
# num <label> <sql> — print the query's single numeric result, or log why not and fail.
num(){
  local out
  if ! out=$(CH "$2" 2>&1); then log "ABORT $1 query failed: $out"; return 1; fi
  case "$out" in ''|*[!0-9]*) log "ABORT $1='$out' is not a number"; return 1 ;; esac
  printf '%s' "$out"
}

if ! PARTS=$(CH "SELECT partition FROM system.parts WHERE database='stellar' AND table='ledger_entry_changes' AND active AND toUInt32(partition) BETWEEN 38 AND 53 GROUP BY partition ORDER BY sum(bytes_on_disk) DESC" 2>&1); then
  log "ABORT partition-list query failed: $PARTS"; exit 1
fi
log "RECOMPRESS_START order:$(echo "$PARTS" | tr '\n' ' ')"

skipped=0
for p in $PARTS; do
  case "$p" in *[!0-9]*) log "ABORT partition id '$p' is not numeric"; exit 1 ;; esac
  avail=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')
  case "$avail" in ''|*[!0-9]*) log "p$p SKIP df-glitch"; skipped=$((skipped + 1)); sleep 30; continue ;; esac
  if [ "$avail" -lt "$FLOOR_KB" ]; then
    log "ABORT <500GiB free (${avail}KiB) before p$p"; exit 1
  fi
  before=$(num "p$p before" "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='ledger_entry_changes' AND active AND partition='$p'") || exit 1
  log "p$p START before=${before}B avail=${avail}KiB"
  if ! CH "OPTIMIZE TABLE stellar.ledger_entry_changes PARTITION ID '$p' FINAL" >> "$LOG" 2>&1; then
    log "p$p FAILED: OPTIMIZE returned an error (logged above) — aborting; re-run to resume"; exit 1
  fi
  # belt-and-suspenders: wait out any lingering merge on this partition
  while :; do
    merging=$(num "p$p merges" "SELECT count() FROM system.merges WHERE database='stellar' AND table='ledger_entry_changes' AND partition_id='$p'") || exit 1
    [ "$merging" = 0 ] && break
    sleep 20
  done
  after=$(num "p$p after" "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='ledger_entry_changes' AND active AND partition='$p'") || exit 1
  tip=$(num "live tip" "SELECT max(ledger_seq) FROM stellar.ledgers") || exit 1
  log "p$p DONE after=${after}B saved=$(( (before - after) / 1073741824 ))GiB live_tip=${tip}"
done
if [ "$skipped" -gt 0 ]; then
  log "RECOMPRESS_INCOMPLETE skipped=$skipped partition(s) — re-run to cover them"; exit 1
fi
log "RECOMPRESS_COMPLETE"
