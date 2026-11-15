#!/usr/bin/env bash
# Drives configs/ansible/roles/archival-node/files/pg-chunk-scan.sh with a fake
# psql: an unreadable chunk is published and never marked verified, a verified
# chunk is skipped next run, a dropped chunk leaves the verified set, a chunk
# that ends its backend twice is named, an undecided chunk is neither verified
# nor counted, a budget-limited run keeps its verdicts, a compressed chunk with
# oversized indexes is listed, and a psql failure keeps the last verdicts.
set -euo pipefail
here="$(cd "$(dirname "$0")/../.." && pwd)"
probe="$here/configs/ansible/roles/archival-node/files/pg-chunk-scan.sh"
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
fail=0
check() { if eval "$2"; then echo "ok: $1"; else echo "FAIL: $1"; fail=1; fi; }
# The fake answers the listing from $FAKE_DIR/list, a decode from
# $FAKE_DIR/v/<relation> (default ok) and the liveness probe from down.
mkdir "$t/v"
cat >"$t/psql" <<'FAKE'
#!/usr/bin/env bash
# Every session must search pg_catalog only: the scan runs as the superuser in an app-owned database.
case "${PGOPTIONS:-}" in *"search_path=pg_catalog"*) ;; *) echo "psql: PGOPTIONS lacks search_path=pg_catalog" >&2; exit 2 ;; esac
cc='' probe=0
case "${PGOPTIONS:-}" in *pgchunkscan.cc=*) cc="${PGOPTIONS##*pgchunkscan.cc=}" ;; esac
for a in "$@"; do case "$a" in
  -c) probe=1 ;;
esac; done
if [ "$probe" = 1 ]; then [ ! -e "$FAKE_DIR/down" ]; exit; fi
cat >/dev/null
if [ -z "$cc" ]; then
  [ -e "$FAKE_DIR/fail" ] && { echo "psql: error: connection to server failed: Permission denied" >&2; exit 2; }
  cat "$FAKE_DIR/list"; exit 0
fi
echo "$cc" >>"$FAKE_DIR/decoded"
case "$(cat "$FAKE_DIR/v/$cc" 2>/dev/null || echo ok)" in
  ok) echo 'psql:<stdin>:20: NOTICE:  CHUNK|ok' >&2 ;;
  bad) echo 'psql:<stdin>:20: NOTICE:  CHUNK|bad|XX001 invalid compressed data' >&2 ;;
  skip) echo 'psql:<stdin>:20: NOTICE:  CHUNK|skip|42P01 relation does not exist' >&2 ;;
  lost) echo 'psql:<stdin>:20: server closed the connection unexpectedly' >&2; exit 2 ;;
esac
FAKE
chmod +x "$t/psql"
export FAKE_DIR="$t" TEXTFILE_DIR="$t/tf" PG_CHUNK_SCAN_STATE="$t/state" PG_CHUNK_SCAN_PSQL="$t/psql" PG_CHUNK_SCAN_RECOVER_WAIT=0
prom="$t/tf/pg_chunk_scan.prom"
run() { : >"$t/decoded"; "$probe" >/dev/null 2>"$t/err" </dev/null; }

printf '%s\n' \
  'C|101:5001|trades|_hyper_1_1_chunk|i.c101|7' \
  'C|102:5002|claimable_observations|_hyper_7_9_chunk|i.c102|7' \
  'B|trades|_hyper_1_24874_chunk|25769803776' >"$t/list"
echo bad >"$t/v/i.c102"
run
check "first run decodes everything" "[ \"\$(wc -l <'$t/decoded' | tr -d ' ')\" = 2 ]"
check "unreadable count published"   "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 1' '$prom'"
check "bad chunk labelled"           "grep -q 'chunk_unreadable{hypertable=\"claimable_observations\",chunk=\"_hyper_7_9_chunk\"} 1' '$prom'"
check "readable chunk verified"      "grep -qx '101:5001@7' '$t/state'"
check "bad chunk not verified"       "! grep -q '102:5002' '$t/state'"
check "success 1"                    "grep -qx 'stellarindex_pg_chunk_scan_success 1' '$prom'"
check "index bloat counted"          "grep -qx 'stellarindex_pg_compressed_chunks_index_bloated 1' '$prom'"
check "index bloat labelled"         "grep -q 'chunk_index_bytes{hypertable=\"trades\",chunk=\"_hyper_1_24874_chunk\"} 25769803776' '$prom'"

printf '%s\n' \
  'C|101:5001|trades|_hyper_1_1_chunk|i.c101|7' \
  'C|102:5002|claimable_observations|_hyper_7_9_chunk|i.c102|7' \
  'C|103:5004|trades|_hyper_1_2_chunk|i.c103|7' >"$t/list"
run
check "second run skips verified"    "! grep -qx 'i.c101' '$t/decoded'"
check "bad chunk decoded first"      "[ \"\$(head -n 1 '$t/decoded')\" = i.c102 ]"
check "bad chunk still counted"      "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 1' '$prom'"

PG_CHUNK_SCAN_BUDGET=0 PG_CHUNK_SCAN_FULL=1 run
check "partial run flagged"          "grep -qx 'stellarindex_pg_chunk_scan_partial 1' '$prom'"
check "partial run decodes nothing"  "[ ! -s '$t/decoded' ]"
check "partial full run keeps state" "grep -qx '101:5001@7' '$t/state' && grep -qx '103:5004@7' '$t/state'"
check "unreached bad chunk counted"  "grep -q 'chunk_unreadable{hypertable=\"claimable_observations\",chunk=\"_hyper_7_9_chunk\"} 1' '$prom'"

touch "$t/fail"
rc=0; run || rc=$?
check "psql failure exits non-zero"  "[ $rc -ne 0 ]"
check "failure prints psql output"   "grep -q 'connection to server failed' '$t/err'"
check "failure prom world-readable"  "[ -n \"\$(find '$prom' -perm -0004)\" ]"
check "failure publishes success 0"  "grep -qx 'stellarindex_pg_chunk_scan_success 0' '$prom'"
check "failure keeps last verdict"   "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 1' '$prom'"
check "failure keeps state"          "grep -qx '101:5001@7' '$t/state'"
check "failure keeps index bloat"    "grep -qx 'stellarindex_pg_compressed_chunks_index_bloated 0' '$prom'"
rm "$t/fail"

# 101 is dropped, 102 is rewritten readable, 104 ends its backend, 105 is
# dropped between the listing and its decode.
printf '%s\n' \
  'C|102:5003|claimable_observations|_hyper_7_9_chunk|i.c102|7' \
  'C|103:5004|trades|_hyper_1_2_chunk|i.c103|7' \
  'C|104:5005|trades|_hyper_1_3_chunk|i.c104|7' \
  'C|105:5006|trades|_hyper_1_4_chunk|i.c105|7' >"$t/list"
echo ok >"$t/v/i.c102"; echo lost >"$t/v/i.c104"; echo skip >"$t/v/i.c105"
run
check "dropped chunk leaves state"   "! grep -q '101:5001' '$t/state'"
check "repaired chunk clears"        "! grep -q '_hyper_7_9_chunk' '$prom' && grep -qx '102:5003@7' '$t/state'"
check "crashing chunk retried once"  "[ \"\$(grep -cx 'i.c104' '$t/decoded')\" = 2 ]"
check "crashing chunk named"         "grep -q 'chunk_unreadable{hypertable=\"trades\",chunk=\"_hyper_1_3_chunk\"} 1' '$prom'"
check "run survives the crash"       "grep -qx 'stellarindex_pg_chunk_scan_success 1' '$prom'"
check "undecided chunk not verified" "! grep -q '105:5006' '$t/state'"
check "undecided chunk not counted"  "grep -qx 'stellarindex_pg_compressed_chunks_unreadable 1' '$prom'"
check "complete run not partial"     "grep -qx 'stellarindex_pg_chunk_scan_partial 0' '$prom'"

# The next night, and a forced full scan, must not decode the backend-killer again: each decode restarts the cluster.
run
check "crasher not decoded again"    "! grep -qx 'i.c104' '$t/decoded'"
check "crasher still counted"        "grep -q 'chunk_unreadable{hypertable=\"trades\",chunk=\"_hyper_1_3_chunk\"} 1' '$prom'"
PG_CHUNK_SCAN_FULL=1 run
check "full scan spares the crasher" "! grep -qx 'i.c104' '$t/decoded'"

# A statistics reset (crash recovery does one) moves every write count. The
# verified chunks are decoded again; the backend-killer is not.
sed -i.bak 's/|7$/|0/' "$t/list" && rm "$t/list.bak"
run
check "stats reset re-decodes"       "grep -qx 'i.c103' '$t/decoded'"
check "stats reset spares crasher"   "! grep -qx 'i.c104' '$t/decoded'"
sed -i.bak 's/|0$/|7/' "$t/list" && rm "$t/list.bak"
run

# A lost connection with the server still down fails the run; it does not
# blame the chunk.
echo lost >"$t/v/i.c105"; touch "$t/down"
rc=0; run || rc=$?
check "server down fails the run"    "[ $rc -ne 0 ] && grep -qx 'stellarindex_pg_chunk_scan_success 0' '$prom'"
check "server down blames no chunk"  "! grep -q '_hyper_1_4_chunk' '$prom'"
rm "$t/down"

printf '%s\n' 'C|103:5004|trades|_hyper_1_2_chunk|i.c103|7' >"$t/list"
run
check "verified chunk not decoded"   "[ ! -s '$t/decoded' ]"

# Batches written in place: same relation file, more rows written. The chunk
# is decoded again and its key moves on.
printf '%s\n' 'C|103:5004|trades|_hyper_1_2_chunk|i.c103|9' >"$t/list"
run
check "in-place rewrite decoded"     "grep -qx 'i.c103' '$t/decoded'"
check "rewrite key replaces old"     "grep -qx '103:5004@9' '$t/state' && ! grep -q '103:5004@7' '$t/state'"

# A key recorded before the value read (no +v) does not count as verified.
echo '103:5004@9' >"$t/state"
printf '%s\n' 'C|103:5004|trades|_hyper_1_2_chunk|i.c103|9+v' >"$t/list"
run
check "header-only key decoded again" "grep -qx 'i.c103' '$t/decoded' && grep -qx '103:5004@9+v' '$t/state' && ! grep -qx '103:5004@9' '$t/state'"
# The fake psql cannot see the SQL: pin the two statements the claim rests on.
check "scan reads values and marks it" "grep -qF 'OFFSET 0) s WHERE s.t IS NOT NULL' '$probe' && grep -qF \"|| '+v' AS f5\" '$probe'"

check "no stray series when clean"   "! grep -q '^stellarindex_pg_compressed_chunk_unreadable{' '$prom' && grep -qx 'stellarindex_pg_compressed_chunks_unreadable 0' '$prom'"
check "no bloat series when clean"   "! grep -q '^stellarindex_pg_compressed_chunk_index_bytes{' '$prom' && grep -qx 'stellarindex_pg_compressed_chunks_index_bloated 0' '$prom'"
exit "$fail"
