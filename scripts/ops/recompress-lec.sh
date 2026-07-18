#!/bin/bash
# Phase A — recompress ledger_entry_changes full-fidelity partitions [38..53] to
# ZSTD (codec already set via MODIFY COLUMN). Per-partition, biggest-first,
# disk-guarded. Skips degraded partitions (Phase D overwrites them) and the live
# tip (new inserts already land ZSTD). Idempotent: re-running just re-OPTIMIZEs
# (cheap no-op on already-ZSTD partitions). Resumable: safe to restart.
set -uo pipefail
LOG=/var/log/recompress-lec.log
FLOOR_KB=524288000   # 500 GiB abort floor
CH(){ curl -sS 'http://localhost:8123/' --data-binary "$1"; }

PARTS=$(CH "SELECT partition FROM system.parts WHERE database='stellar' AND table='ledger_entry_changes' AND active AND toUInt32(partition) BETWEEN 38 AND 53 GROUP BY partition ORDER BY sum(bytes_on_disk) DESC")
echo "$(date -u +%FT%TZ) RECOMPRESS_START order:$(echo $PARTS | tr '\n' ' ')" >> "$LOG"

for p in $PARTS; do
  avail=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')
  case "$avail" in ''|*[!0-9]*) echo "$(date -u +%FT%TZ) p$p SKIP df-glitch" >> "$LOG"; sleep 30; continue ;; esac
  if [ "$avail" -lt "$FLOOR_KB" ]; then
    echo "$(date -u +%FT%TZ) ABORT <500GiB free (${avail}KiB) before p$p" >> "$LOG"; exit 1
  fi
  before=$(CH "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='ledger_entry_changes' AND active AND partition='$p'")
  echo "$(date -u +%FT%TZ) p$p START before=${before}B avail=${avail}KiB" >> "$LOG"
  CH "OPTIMIZE TABLE stellar.ledger_entry_changes PARTITION ID '$p' FINAL" >> "$LOG" 2>&1
  # belt-and-suspenders: wait out any lingering merge on this partition
  while [ "$(CH "SELECT count() FROM system.merges WHERE database='stellar' AND table='ledger_entry_changes' AND partition_id='$p'")" != "0" ]; do sleep 20; done
  after=$(CH "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='ledger_entry_changes' AND active AND partition='$p'")
  tip=$(CH "SELECT max(ledger_seq) FROM stellar.ledgers")
  echo "$(date -u +%FT%TZ) p$p DONE after=${after}B saved=$(( (before - after) / 1073741824 ))GiB live_tip=${tip}" >> "$LOG"
done
echo "$(date -u +%FT%TZ) RECOMPRESS_COMPLETE" >> "$LOG"
