#!/bin/bash
# Phase A margin: recompress operations/operation_results/contract_events to ZSTD
# (codecs already set). One table at a time, one partition at a time, biggest-first,
# disk-guarded. Pool has ~2.8 TiB free so this is low-risk; still bounded + reversible.
set -uo pipefail
LOG=/var/log/recompress-others.log
CHL(){ curl -sS --max-time 14400 'http://localhost:8123/' --data-binary "$1"; }
CH(){  curl -sS --max-time 60    'http://localhost:8123/' --data-binary "$1"; }
FLOOR_KB=524288000   # 500 GiB — pause if below

CH "ALTER TABLE stellar.operations         MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 536870912000" >/dev/null
CH "ALTER TABLE stellar.operation_results  MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 536870912000" >/dev/null
CH "ALTER TABLE stellar.contract_events    MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 536870912000" >/dev/null
echo "$(date -u +%FT%TZ) OTHERS_START" >> "$LOG"

for t in operations operation_results contract_events; do
  echo "$(date -u +%FT%TZ) TABLE $t START" >> "$LOG"
  parts=$(CH "SELECT partition FROM system.parts WHERE database='stellar' AND table='$t' AND active GROUP BY partition ORDER BY sum(bytes_on_disk) DESC")
  for p in $parts; do
    ok=0
    for w in 1 2 3 4 5 6; do
      avail=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')
      case "$avail" in ''|*[!0-9]*) sleep 60; continue ;; esac
      if [ "$avail" -ge "$FLOOR_KB" ]; then ok=1; break; fi
      echo "$(date -u +%FT%TZ) $t/$p wait: free ${avail}KiB <500G" >> "$LOG"; sleep 300
    done
    [ "$ok" = 0 ] && { echo "$(date -u +%FT%TZ) $t/$p SKIP: free still <500G" >> "$LOG"; continue; }
    before=$(CH "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='$t' AND active AND partition='$p'")
    CHL "OPTIMIZE TABLE stellar.$t PARTITION ID '$p' FINAL" >> "$LOG" 2>&1
    while [ "$(CH "SELECT count() FROM system.merges WHERE database='stellar' AND table='$t' AND partition_id='$p'")" != "0" ]; do sleep 20; done
    after=$(CH "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='$t' AND active AND partition='$p'")
    echo "$(date -u +%FT%TZ) $t/$p done saved=$(( (before - after) / 1073741824 ))GiB free_after=${avail}KiB" >> "$LOG"
  done
  echo "$(date -u +%FT%TZ) TABLE $t COMPLETE" >> "$LOG"
done

# revert the ceilings
for t in operations operation_results contract_events; do
  CH "ALTER TABLE stellar.$t MODIFY SETTING max_bytes_to_merge_at_max_space_in_pool = 161061273600" >/dev/null
done
echo "$(date -u +%FT%TZ) OTHERS_COMPLETE (ceilings reverted to 150G)" >> "$LOG"
