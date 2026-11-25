#!/bin/bash
# Re-compress stellar.ledger_entry_changes onto the codecs set by
# deploy/clickhouse/ledger_entry_changes_codecs.sql, one partition at a time, with
# REWRITE PARTS: a mutation that rewrites each part one-for-one and keeps duplicates.
# Never OPTIMIZE FINAL here: on a ReplacingMergeTree it merges and deduplicates,
# changing the stored rows. Invariant: count, per-part rows and an all-column hash
# are equal before and after, or the partition's freeze (shadow/lec_codec_<n>) is kept.
# No system table records a part's codec and the codec ALTER leaves no mutation to
# date against, so finished partitions are recorded in $RECOMPRESS_STATE, written
# only after that check passes; delete the file if the codecs change again.
# Run as root, NOT under run-heavy-job.sh: it takes the heavy-job lock per partition
# and waits out the timers' quiet window. The tip partition still takes inserts and
# is skipped. Stopping this script does not stop a running mutation; see
# ops-job-stalled.md "A ClickHouse rewrite outlives its client".
set -uo pipefail
LOG="${RECOMPRESS_LOG:-/var/log/recompress-lec.log}"
STATE="${RECOMPRESS_STATE:-/var/lib/stellarindex/recompress-lec.done}"
FLOOR_GIB="${RECOMPRESS_FLOOR_GIB:-600}"   # free space kept on top of the partition's copy
ABORT_GIB="${RECOMPRESS_ABORT_GIB:-500}"   # kill the mutation below this
QUIET="${RECOMPRESS_QUIET-01:00-08:30}"    # UTC HH:MM-HH:MM; empty disables
ATTEMPTS="${RECOMPRESS_LOCK_ATTEMPTS:-12}"
HEAVY="${HEAVY_JOB:-/usr/local/sbin/run-heavy-job.sh}"
TBL=stellar.ledger_entry_changes
T="database='stellar' AND table='ledger_entry_changes'"
# ClickHouse reports an exception as HTTP 500 with the message in the body;
# without --fail-with-body curl exits 0 and the error reads as a result.
CH(){ curl -sS --fail-with-body 'http://localhost:8123/' --data-binary "$1"; }
log(){ echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
abort(){ log "ABORT $*"; exit 1; }
# num <label> <sql> — print the query's single numeric result, or log why not and fail.
num(){
  local out
  if ! out=$(CH "$2" 2>&1); then log "ABORT $1 query failed: $out"; return 1; fi
  case "$out" in ''|*[!0-9]*) log "ABORT $1='$out' is not a number"; return 1 ;; esac
  printf '%s' "$out"
}
avail_kb(){
  local a
  a=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')
  case "$a" in ''|*[!0-9]*) log "ABORT df avail='$a' is not a number"; return 1 ;; esac
  printf '%s' "$a"
}
check_codecs(){
  local n
  n=$(num codecs "SELECT count() FROM system.columns WHERE $T AND ((name IN ('account_id','tx_hash','asset','balance') AND compression_codec LIKE '%ZSTD(3)%') OR (name='intra_ledger_seq' AND compression_codec LIKE '%Delta%ZSTD(3)%'))") || exit 1
  [ "$n" = 5 ] || abort "$n of 5 columns carry the new codecs; apply deploy/clickhouse/ledger_entry_changes_codecs.sql first"
}
# sig <p> — rows, all-column hash, part count and sorted per-part row counts of partition p.
sig(){
  local r h n pr
  r=$(num "p$1 rows" "SELECT count() FROM $TBL WHERE _partition_id='$1'") || return 1
  h=$(num "p$1 hash" "SELECT sum(cityHash64(*)) FROM $TBL WHERE _partition_id='$1'") || return 1
  n=$(num "p$1 parts" "SELECT count() FROM system.parts WHERE $T AND active AND partition_id='$1'") || return 1
  pr=$(num "p$1 part rows" "SELECT cityHash64(arraySort(groupArray(rows))) FROM system.parts WHERE $T AND active AND partition_id='$1'") || return 1
  printf 'rows=%s hash=%s parts=%s part_rows=%s' "$r" "$h" "$n" "$pr"
}
bytes(){ num "p$1 bytes" "SELECT sum(bytes_on_disk) FROM system.parts WHERE $T AND active AND partition_id='$1'"; }
mins(){ echo $(( 10#${1%%:*} * 60 + 10#${1##*:} )); }
quiet_wait(){
  [ -n "$QUIET" ] || return 0
  case "$QUIET" in [0-2][0-9]:[0-5][0-9]-[0-2][0-9]:[0-5][0-9]) ;; *) abort "RECOMPRESS_QUIET='$QUIET' is not HH:MM-HH:MM" ;; esac
  local s e now in
  s=$(mins "${QUIET%-*}"); e=$(mins "${QUIET#*-}")
  while :; do
    now=$(mins "$(date -u +%H:%M)")
    if [ "$s" -le "$e" ]; then in=$(( now >= s && now < e )); else in=$(( now >= s || now < e )); fi
    [ "$in" = 1 ] || return 0
    log "quiet window $QUIET UTC: waiting $(( (e - now + 1440) % 1440 ))min"
    sleep $(( (e - now + 1440) % 1440 * 60 ))
  done
}
kill_mutation(){
  CH "KILL MUTATION WHERE $T AND mutation_id='$1'" >> "$LOG" 2>&1 || log "KILL MUTATION $1 failed (logged above); kill it by hand"
}

one(){
  local p=$1 f=lec_codec_$1 size avail need open mid st before after
  check_codecs
  size=$(bytes "$p") || exit 1
  avail=$(avail_kb) || exit 1
  need=$(( (FLOOR_GIB << 20) + size / 1024 ))
  [ "$avail" -ge "$need" ] || abort "p$p needs ${need}KiB free (${FLOOR_GIB}GiB floor + partition), have ${avail}KiB"
  open=$(num "open mutations" "SELECT count() FROM system.mutations WHERE $T AND NOT is_done") || exit 1
  [ "$open" = 0 ] || abort "p$p: $open mutation(s) already open on the table"
  before=$(sig "$p") || exit 1
  log "p$p START bytes=$size avail=${avail}KiB $before"
  CH "ALTER TABLE $TBL FREEZE PARTITION $p WITH NAME '$f'" >> "$LOG" 2>&1 || abort "p$p FREEZE failed (logged above)"
  CH "ALTER TABLE $TBL REWRITE PARTS IN PARTITION $p SETTINGS mutations_sync = 0" >> "$LOG" 2>&1 ||
    abort "p$p REWRITE failed (logged above); freeze $f kept"
  mid=$(CH "SELECT mutation_id FROM system.mutations WHERE $T AND command LIKE '%REWRITE PARTS%' ORDER BY create_time DESC, toUInt64OrZero(extract(mutation_id, '[0-9]+')) DESC LIMIT 1" 2>&1)
  case "$mid" in mutation_[0-9]*.txt) ;; *) abort "p$p mutation id '$mid' unreadable; check system.mutations; freeze $f kept" ;; esac
  while :; do
    st=$(CH "SELECT toString(is_done) || '|' || latest_fail_reason FROM system.mutations WHERE $T AND mutation_id='$mid'" 2>&1) ||
      abort "p$p polling $mid failed: $st; it may still run (system.mutations); freeze $f kept"
    if [ -n "${st#*|}" ] && [ "$st" != "${st#*|}" ]; then
      kill_mutation "$mid"; abort "p$p mutation $mid failed: ${st#*|}; killed; freeze $f kept"
    fi
    case "$st" in
      '1|') break ;;
      '0|') ;;
      '') abort "p$p mutation $mid vanished (killed?); freeze $f kept" ;;
      *) abort "p$p mutation $mid status '$st' unreadable; freeze $f kept" ;;
    esac
    if ! avail=$(avail_kb) || [ "$avail" -lt $(( ABORT_GIB << 20 )) ]; then
      kill_mutation "$mid"; abort "p$p free space ${avail}KiB below ${ABORT_GIB}GiB; $mid killed; freeze $f kept"
    fi
    log "p$p rewriting $mid avail=${avail}KiB"
    sleep 60
  done
  after=$(sig "$p") || abort "p$p freeze $f kept"
  if [ "$after" != "$before" ]; then
    log "p$p MISMATCH before[$before] after[$after]; freeze $f KEPT for restore (ops-job-stalled.md)"; exit 3
  fi
  CH "ALTER TABLE $TBL UNFREEZE PARTITION $p WITH NAME '$f'" >> "$LOG" 2>&1 || abort "p$p verified but UNFREEZE $f failed (logged above)"
  echo "$p" >> "$STATE" || abort "p$p verified but $STATE is not writable"
  after=$(bytes "$p") || exit 1
  log "p$p DONE before=${size}B after=${after}B saved=$(( (size - after) / 1073741824 ))GiB $before"
}

if [ "${1-}" = --one ]; then
  case "${2-}" in ''|*[!0-9]*) abort "--one needs a numeric partition id" ;; esac
  one "$2"; exit 0
fi

check_codecs
PARTS=$(CH "SELECT partition_id FROM system.parts WHERE $T AND active GROUP BY partition_id ORDER BY sum(bytes_on_disk)" 2>&1) ||
  abort "partition-list query failed: $PARTS"
tip=$(num tip "SELECT max(toUInt32(partition_id)) FROM system.parts WHERE $T AND active") || exit 1
todo=""
for p in $PARTS; do
  case "$p" in *[!0-9]*) abort "partition id '$p' is not numeric" ;; esac
  [ "$p" = "$tip" ] && continue
  if [ -n "${RECOMPRESS_PARTITIONS:-}" ] && ! [[ " $RECOMPRESS_PARTITIONS " == *" $p "* ]]; then continue; fi
  todo="$todo $p"
done
for r in ${RECOMPRESS_PARTITIONS:-}; do
  [[ "$todo " == *" $r "* ]] || abort "RECOMPRESS_PARTITIONS: $r is not an active partition below the tip ($tip)"
done
if ! mkdir -p "$(dirname "$STATE")" || ! touch "$STATE"; then abort "cannot write $STATE"; fi
log "RECOMPRESS_START tip=$tip order:$todo"

for p in $todo; do
  if grep -qx "$p" "$STATE"; then log "p$p SKIP already done (in $STATE)"; continue; fi
  attempt=0
  while :; do
    quiet_wait
    attempt=$((attempt + 1))
    "$HEAVY" "recompress-lec-p$p" "$0" --one "$p"; rc=$?
    [ "$rc" = 75 ] || break
    [ "$attempt" -lt "$ATTEMPTS" ] || abort "p$p heavy-job lock still held after $attempt attempts"
    log "p$p heavy-job lock held (attempt $attempt/$ATTEMPTS); retrying in 10min"
    sleep 600
  done
  [ "$rc" = 0 ] || { log "ABORT p$p exited $rc"; exit "$rc"; }
  grep -qx "$p" "$STATE" || abort "p$p exited 0 but is not recorded in $STATE"
done
log "RECOMPRESS_COMPLETE"
