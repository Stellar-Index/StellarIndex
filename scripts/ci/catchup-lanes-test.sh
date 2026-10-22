#!/usr/bin/env bash
# Drives configs/ansible/roles/archival-node/files/catchup-lanes.sh over a
# fixture lane directory: failed, queued (held and done lines excluded) and the
# last log time are published per lane.
set -euo pipefail
here="$(cd "$(dirname "$0")/../.." && pwd)"
probe="$here/configs/ansible/roles/archival-node/files/catchup-lanes.sh"
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
fail=0
check() { if grep -qx "$2" "$t/tf/catchup_lanes.prom"; then echo "ok: $1"; else echo "FAIL: $1 (want: $2)"; fail=1; fi; }
mkdir -p "$t/c"
printf 'one\tcmd1\ntwo\tcmd2\n#held\tcmd3\nthree\tcmd4\n' >"$t/c/a.queue"
printf 'one\n' >"$t/c/a.done"
printf '2026-10-21T22:42:04Z START two\n' >"$t/c/a.log"
echo "two rc=1" >"$t/c/a.failed"
printf 'x\tcmd\n' >"$t/c/b.queue"
CATCHUP_DIR="$t/c" TEXTFILE_DIR="$t/tf" "$probe" </dev/null
want_ts=1792622524 # 2026-10-21T22:42:04Z
check "a failed"            'stellarindex_catchup_lane_failed{lane="a"} 1'
check "a queued excludes done and held" 'stellarindex_catchup_lane_queued{lane="a"} 2'
check "a last event"        "stellarindex_catchup_lane_last_event_timestamp_seconds{lane=\"a\"} $want_ts"
check "b not failed"        'stellarindex_catchup_lane_failed{lane="b"} 0'
check "b no done file"      'stellarindex_catchup_lane_queued{lane="b"} 1'
check "b no log"            'stellarindex_catchup_lane_last_event_timestamp_seconds{lane="b"} 0'
check "not stopped"         'stellarindex_catchup_lanes_stopped 0'
exit "$fail"
