#!/usr/bin/env bash
# Drives configs/ansible/roles/archival-node/files/pg-chunk-scan.sh with a fake
# psql: an unreadable chunk is published and never marked verified, a verified
# chunk is skipped next run, a budget-limited run keeps its progress, and a
# psql failure keeps the last verdicts.
set -euo pipefail
here="$(cd "$(dirname "$0")/../.." && pwd)"
probe="$here/configs/ansible/roles/archival-node/files/pg-chunk-scan.sh"
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
fail=0
check() { if eval "$2"; then echo "ok: $1"; else echo "FAIL: $1"; fail=1; fi; }
# The fake prints the notices psql would, and records the skip list it got.
cat >"$t/psql" <<'FAKE'
#!/usr/bin/env bash
for a in "$@"; do case "$a" in
  skip=*) echo "${a#skip=}" >"$FAKE_DIR/skip.seen" ;;
  first=*) echo "${a#first=}" >"$FAKE_DIR/first.seen" ;;
esac; done
cat >/dev/null
[ -e "$FAKE_DIR/fail" ] && { echo "psql: error: connection to server failed: Permission denied" >&2; exit 2; }
cat "$FAKE_DIR/notices" >&2
FAKE
chmod +x "$t/psql"
export FAKE_DIR="$t" TEXTFILE_DIR="$t/tf" PG_CHUNK_SCAN_STATE="$t/state" PG_CHUNK_SCAN_PSQL="$t/psql"
prom="$t/tf/pg_chunk_scan.prom"

printf '%s\n' \
  'psql:<stdin>:3: NOTICE:  CHUNK|ok|101:5001|trades|_hyper_1_1_chunk' \
  'psql:<stdin>:3: NOTICE:  CHUNK|bad|102:5002|claimable_observations|_hyper_7_9_chunk' >"$t/notices"
"$probe" >/dev/null </dev/null
check "first run is full"            "grep -qx '{}' '$t/skip.seen'"
check "unreadable count published"   "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 1' '$prom'"
check "bad chunk labelled"           "grep -q 'chunk_unreadable{hypertable=\"claimable_observations\",chunk=\"_hyper_7_9_chunk\"} 1' '$prom'"
check "readable chunk verified"      "grep -qx '101:5001' '$t/state'"
check "bad chunk not verified"       "! grep -q '102:5002' '$t/state'"
check "success 1"                    "grep -qx 'stellarindex_pg_chunk_scan_success 1' '$prom'"

printf '%s\n' 'psql:<stdin>:3: NOTICE:  CHUNK|bad|102:5002|claimable_observations|_hyper_7_9_chunk' >"$t/notices"
"$probe" >/dev/null </dev/null
check "second run skips verified"    "grep -qx '{101:5001}' '$t/skip.seen'"
check "bad chunk still counted"      "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 1' '$prom'"
check "bad chunk decoded first"      "grep -qx '{102:5002}' '$t/first.seen'"

printf '%s\n' \
  'psql:<stdin>:3: NOTICE:  CHUNK|bad|102:5002|claimable_observations|_hyper_7_9_chunk' \
  'psql:<stdin>:3: NOTICE:  CHUNK|ok|103:5004|trades|_hyper_1_2_chunk' \
  'psql:<stdin>:3: NOTICE:  SCAN|partial' >"$t/notices"
PG_CHUNK_SCAN_FULL=1 "$probe" >/dev/null </dev/null
check "partial run flagged"          "grep -qx 'stellarindex_pg_chunk_scan_partial 1' '$prom'"
check "partial full run keeps state" "grep -qx '101:5001' '$t/state' && grep -qx '103:5004' '$t/state'"

touch "$t/fail"
rc=0; "$probe" >/dev/null 2>"$t/err" </dev/null || rc=$?
check "psql failure exits non-zero"  "[ $rc -ne 0 ]"
check "failure prints psql output"   "grep -q 'connection to server failed' '$t/err'"
check "failure prom world-readable"  "[ -n \"\$(find '$prom' -perm -0004)\" ]"
check "failure publishes success 0"  "grep -qx 'stellarindex_pg_chunk_scan_success 0' '$prom'"
check "failure keeps last verdict"   "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 1' '$prom'"
check "failure keeps state"          "grep -qx '101:5001' '$t/state'"
rm "$t/fail"

printf '%s\n' 'psql:<stdin>:3: NOTICE:  CHUNK|ok|102:5003|claimable_observations|_hyper_7_9_chunk' >"$t/notices"
"$probe" >/dev/null </dev/null
check "repaired chunk clears"        "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 0' '$prom'"
check "complete run not partial"     "grep -qx 'stellarindex_pg_chunk_scan_partial 0' '$prom'"
check "no stray series when clean"   "! grep -q '^stellarindex_pg_compressed_chunk_unreadable{' '$prom'"
exit "$fail"
