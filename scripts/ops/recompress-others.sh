#!/bin/bash
# Phase A margin: recompress operations/operation_results/contract_events to ZSTD
# (codecs already set). One table at a time, one partition at a time, biggest-first,
# disk-guarded. Pool has ~2.8 TiB free so this is low-risk; still bounded + reversible.
# The raised merge ceiling is put back to the value each table had before the run
# on every exit a shell can trap (success, failure, TERM/INT/HUP). A SIGKILL cannot
# be trapped: the pre-run values are logged at OTHERS_START so they can be restored
# by hand. OTHERS_COMPLETE and exit 0 mean every partition was rewritten.
set -uo pipefail
LOG="${RECOMPRESS_LOG:-/var/log/recompress-others.log}"
# ClickHouse reports an exception as HTTP 500 with the message in the body;
# without --fail-with-body curl exits 0 and the error reads as a result.
CHL(){ curl -sS --fail-with-body --max-time 14400 'http://localhost:8123/' --data-binary "$1"; }
CH(){  curl -sS --fail-with-body --max-time 60    'http://localhost:8123/' --data-binary "$1"; }
FLOOR_KB=524288000   # 500 GiB — pause if below
TABLES="operations operation_results contract_events"
SETTING=max_bytes_to_merge_at_max_space_in_pool
RAISED_CEILING=536870912000   # 500 GiB, for the duration of the rewrite
log(){ echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
# num <label> <sql> — print the query's single numeric result, or log why not and fail.
num(){
  local out
  if ! out=$(CH "$2" 2>&1); then log "ABORT $1 query failed: $out"; return 1; fi
  case "$out" in ''|*[!0-9]*) log "ABORT $1='$out' is not a number"; return 1 ;; esac
  printf '%s' "$out"
}

# The table's OWN setting, read from its engine clause: "v=" alone means unset
# (inherited from the server default), no row at all means no such table.
for t in $TABLES; do
  if ! v=$(CH "SELECT concat('v=', extract(engine_full, '$SETTING = ([0-9]+)')) FROM system.tables WHERE database='stellar' AND name='$t'" 2>&1); then
    log "ABORT reading $t's $SETTING failed: $v"; exit 1
  fi
  case "$v" in
    v=*[!0-9]*) log "ABORT $t's $SETTING read back as '$v'"; exit 1 ;;
    v=*) ;;
    *) log "ABORT $t's $SETTING read back as '$v' (no such table?)"; exit 1 ;;
  esac
  printf -v "orig_$t" '%s' "${v#v=}"
done

raised=""
# restore_ceilings — put every raised table back to its pre-run value.
restore_ceilings(){
  local t name orig sql out rc=0
  for t in $raised; do
    name="orig_$t"; orig="${!name}"
    if [ -n "$orig" ]; then
      sql="ALTER TABLE stellar.$t MODIFY SETTING $SETTING = $orig"
    else
      sql="ALTER TABLE stellar.$t RESET SETTING $SETTING"
    fi
    if out=$(CH "$sql" 2>&1); then
      log "$t $SETTING restored to ${orig:-the server default}"
    else
      log "RESTORE_FAILED $t: $out — run by hand: $sql"; rc=1
    fi
  done
  raised=""
  return "$rc"
}
on_exit(){
  local rc=$?
  trap - EXIT
  if [ -n "$raised" ] && ! restore_ceilings && [ "$rc" -eq 0 ]; then rc=1; fi
  exit "$rc"
}
trap on_exit EXIT
trap 'log "TERM received — stopping"; exit 143' TERM
trap 'log "INT received — stopping"; exit 130' INT
trap 'log "HUP received — stopping"; exit 129' HUP

log "OTHERS_START pre-run $SETTING: operations=${orig_operations:-default} operation_results=${orig_operation_results:-default} contract_events=${orig_contract_events:-default}"
for t in $TABLES; do
  # Listed BEFORE the ALTER: a timed-out ALTER may still have applied server-side.
  raised="$raised $t"
  if ! out=$(CH "ALTER TABLE stellar.$t MODIFY SETTING $SETTING = $RAISED_CEILING" 2>&1); then
    log "ABORT raising $t's $SETTING failed: $out"; exit 1
  fi
done

skipped=0
for t in $TABLES; do
  log "TABLE $t START"
  if ! parts=$(CH "SELECT partition FROM system.parts WHERE database='stellar' AND table='$t' AND active GROUP BY partition ORDER BY sum(bytes_on_disk) DESC" 2>&1); then
    log "ABORT $t partition-list query failed: $parts"; exit 1
  fi
  for p in $parts; do
    case "$p" in *[!0-9]*) log "ABORT $t partition id '$p' is not numeric"; exit 1 ;; esac
    ok=0
    for w in 1 2 3 4 5 6; do
      avail=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')
      case "$avail" in ''|*[!0-9]*) sleep 60; continue ;; esac
      if [ "$avail" -ge "$FLOOR_KB" ]; then ok=1; break; fi
      log "$t/$p wait $w/6: free ${avail}KiB <500G"; sleep 300
    done
    if [ "$ok" = 0 ]; then log "$t/$p SKIP: free still <500G"; skipped=$((skipped + 1)); continue; fi
    before=$(num "$t/$p before" "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='$t' AND active AND partition='$p'") || exit 1
    if ! CHL "OPTIMIZE TABLE stellar.$t PARTITION ID '$p' FINAL" >> "$LOG" 2>&1; then
      log "$t/$p FAILED: OPTIMIZE returned an error (logged above) — aborting; re-run to resume"; exit 1
    fi
    while :; do
      merging=$(num "$t/$p merges" "SELECT count() FROM system.merges WHERE database='stellar' AND table='$t' AND partition_id='$p'") || exit 1
      [ "$merging" = 0 ] && break
      sleep 20
    done
    after=$(num "$t/$p after" "SELECT sum(bytes_on_disk) FROM system.parts WHERE database='stellar' AND table='$t' AND active AND partition='$p'") || exit 1
    log "$t/$p done saved=$(( (before - after) / 1073741824 ))GiB free_before=${avail}KiB"
  done
  log "TABLE $t COMPLETE"
done

restore_ceilings || exit 1
if [ "$skipped" -gt 0 ]; then
  log "OTHERS_INCOMPLETE skipped=$skipped partition(s), ceilings restored — re-run to cover them"; exit 1
fi
log "OTHERS_COMPLETE (ceilings restored to their pre-run values)"
