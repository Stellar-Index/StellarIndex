#!/bin/bash
# Phase D1: archive-walk the degraded ranges with the v0.17.0 ops binary.
# Writes full-fidelity ledger_entry_changes (op-level + intra_ledger_seq) over
# [54M,63.05M] (recent, high value) then [2,38M] (early era). 1M-ledger windows,
# resumable (fresh state file — NOT the old done-windows.txt), disk-guarded.
# Idempotent (RMT overwrite). Census rows coexist harmlessly; cleaned by a bounded
# DELETE after the walk. NOTE: this is ADDITIVE (~2 TiB) — pool has ~4.4 TiB free.
set -uo pipefail
LOG=/var/log/phaseD-backfill.log
STATE=/var/lib/ch-backfill/phaseD-done-windows.txt
FLOOR_KB=524288000   # 500 GiB — pause below
mkdir -p "$(dirname "$STATE")"; touch "$STATE"
set -a; . /etc/default/stellarindex-ops 2>/dev/null; . /etc/default/stellarindex 2>/dev/null; set +a
OPS=/usr/local/bin/stellarindex-ops

echo "$(date -u +%FT%TZ) PHASED_START ranges [54000000,63050000] then [2,38000000]; state=$STATE" >> "$LOG"
for range in "54000000 63050000" "2 38000000"; do
  set -- $range; RFROM=$1; RTO=$2
  echo "$(date -u +%FT%TZ) RANGE [$RFROM,$RTO] START" >> "$LOG"
  w=$RFROM
  while [ "$w" -le "$RTO" ]; do
    wto=$((w + 999999)); [ "$wto" -gt "$RTO" ] && wto=$RTO
    if grep -qx "$w" "$STATE"; then w=$((wto + 1)); continue; fi
    avail=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')
    case "$avail" in ''|*[!0-9]*) sleep 60; continue ;; esac
    if [ "$avail" -lt "$FLOOR_KB" ]; then echo "$(date -u +%FT%TZ) PAUSE <500G (${avail}KiB) before $w" >> "$LOG"; sleep 300; continue; fi
    echo "$(date -u +%FT%TZ) window $w-$wto START avail=${avail}KiB" >> "$LOG"
    if "$OPS" ch-backfill -config /etc/stellarindex.toml -bucket galexie-archive -parallel 3 -flush-every 200 -from "$w" -to "$wto" >> "$LOG" 2>&1; then
      echo "$w" >> "$STATE"
      echo "$(date -u +%FT%TZ) window $w-$wto DONE (avail now $(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')KiB, tip $(curl -sS --max-time 15 localhost:8123/ --data-binary 'SELECT max(ledger_seq) FROM stellar.ledgers'))" >> "$LOG"
    else
      echo "$(date -u +%FT%TZ) window $w-$wto FAILED — retry on resume" >> "$LOG"; sleep 30; continue
    fi
    w=$((wto + 1))
  done
  echo "$(date -u +%FT%TZ) RANGE [$RFROM,$RTO] COMPLETE" >> "$LOG"
done
echo "$(date -u +%FT%TZ) PHASED_BACKFILL_COMPLETE" >> "$LOG"
