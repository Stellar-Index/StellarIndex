#!/usr/bin/env bash
# catchup-lanes — publish the operator catch-up lanes' state to catchup_lanes.prom.
#
# WHY. A lane that hits a failing step writes <lane>.failed and sleeps until an
# operator removes it. On r1 (2026-10-21) two lanes sat failed for 4-7 h and
# nothing fired. A lane with work queued and no log line for a day is just as
# stuck.
#
# Reads only. Seams for scripts/ci/catchup-lanes-test.sh: CATCHUP_DIR, TEXTFILE_DIR.
set -euo pipefail
D="${CATCHUP_DIR:-/var/lib/stellarindex/catchup}"
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$DIR/catchup_lanes.prom"
mkdir -p "$DIR"
TMP=$(mktemp "$DIR/.catchup_lanes.prom.XXXXXX")
trap 'rm -f "$TMP"' EXIT

{
  echo "# HELP stellarindex_catchup_lane_failed 1 while the lane is stopped on a failed step."
  echo "# TYPE stellarindex_catchup_lane_failed gauge"
  echo "# HELP stellarindex_catchup_lane_queued Queue steps not yet done (held lines excluded)."
  echo "# TYPE stellarindex_catchup_lane_queued gauge"
  echo "# HELP stellarindex_catchup_lane_last_event_timestamp_seconds Unix time of the lane log's last line."
  echo "# TYPE stellarindex_catchup_lane_last_event_timestamp_seconds gauge"
  stop=0; [ -e "$D/STOP" ] && stop=1
  for q in "$D"/*.queue; do
    [ -e "$q" ] || continue
    lane=$(basename "$q" .queue)
    case "$lane" in *[!a-z0-9_-]*) continue ;; esac
    failed=0; [ -e "$D/$lane.failed" ] && failed=1
    done_f="$D/$lane.done"; [ -e "$done_f" ] || done_f=/dev/null
    queued=$(awk -F'\t' 'FILENAME==ARGV[1]{d[$1]=1;next} NF>=2 && $1!~/^#/ && !($1 in d){n++} END{print n+0}' "$done_f" "$q")
    last=0
    if [ -s "$D/$lane.log" ]; then
      ts=$(tail -n 1 "$D/$lane.log" | cut -d' ' -f1)
      # GNU date on hosts; BSD form so the self-test runs on macOS too.
      last=$(date -u -d "$ts" +%s 2>/dev/null || date -u -j -f %Y-%m-%dT%H:%M:%SZ "$ts" +%s 2>/dev/null || echo 0)
    fi
    echo "stellarindex_catchup_lane_failed{lane=\"$lane\"} $failed"
    echo "stellarindex_catchup_lane_queued{lane=\"$lane\"} $queued"
    echo "stellarindex_catchup_lane_last_event_timestamp_seconds{lane=\"$lane\"} $last"
  done
  echo "# HELP stellarindex_catchup_lanes_stopped 1 while the STOP file halts every lane."
  echo "# TYPE stellarindex_catchup_lanes_stopped gauge"
  echo "stellarindex_catchup_lanes_stopped $stop"
} >"$TMP"
chmod 0644 "$TMP"
mv "$TMP" "$OUT"
