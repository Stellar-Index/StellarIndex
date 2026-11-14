#!/bin/bash
# si-catchup.sh LANE: run LANE.queue steps (name<TAB>command) in order, one at a time.
# Appending to the queue is picked up on the next pass; a failed step stops the lane
# (LANE.failed names it) until the file is deleted after a fix.
set -u
LANE=$1; D="${CATCHUP_DIR:-/var/lib/stellarindex/catchup}"; Q="$D/$LANE.queue"; DONE="$D/$LANE.done"
mkdir -p "$D"
# Queue lines run as this user (root): refuse a directory or queue anyone else owns or can write.
mine_only() { [ ! -L "$1" ] && [ -O "$1" ] && [ -z "$(find "$1" -maxdepth 0 \( -perm -020 -o -perm -002 \))" ]; }
mine_only "$D" || { echo "si-catchup: $D is not writable by this user alone; refusing to run its queue" >&2; exit 1; }
touch "$Q" "$DONE"
log() { echo "$(date -u +%FT%TZ) $*" >>"$D/$LANE.log"; }
while [ ! -e "$D/STOP" ]; do
  if [ -e "$D/$LANE.failed" ]; then sleep 60; continue; fi
  next=$(awk -F'\t' -v dn="$DONE" 'FILENAME==dn{d[$1]=1;next} NF>=2 && $1!~/^#/ && !($1 in d){print; exit}' "$DONE" "$Q")
  if [ -z "$next" ]; then sleep 60; continue; fi
  if ! mine_only "$D" || ! mine_only "$Q"; then log "REFUSE $Q is writable by another user"; exit 1; fi
  name=${next%%	*}; cmd=${next#*	}
  log "START $name"; t0=$SECONDS
  bash -c "$cmd" >>"$D/$name.log" 2>&1; rc=$?
  if [ $rc -eq 0 ]; then echo "$name" >>"$DONE"; log "DONE $name $((SECONDS-t0))s"
  # 75 = run-heavy-job deferred: a scheduled heavy job holds the host lock and operator jobs never wait for it.
  elif [ $rc -eq 75 ]; then log "DEFER $name rc=75"; sleep 300
  else echo "$name rc=$rc" >"$D/$LANE.failed"; log "FAIL $name rc=$rc $((SECONDS-t0))s"; fi
done
