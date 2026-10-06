#!/usr/bin/env bash
# ordinal-rederive-chunks-test.sh — fixture tests for
# scripts/ops/ordinal-rederive-chunks.sh with `zfs` and stellarindex-ops
# STUBBED (zfs on PATH, ops via OPS), so it runs on any box and never touches
# a pool or ClickHouse. Pins:
#   1. chunk bounds: [START,BAND_END) exclusive, each chunk -to hi-1;
#   2. RUN_START_EPOCH=<now-60> is the first line;
#   3. -write always, -bucket unless BUCKET is empty, EXTRA_FLAGS passed;
#   4. the start floor (exit 3 before chunk 1), the per-chunk floor (exit 3
#      mid-run), and unreadable avail (exit 4, fail closed);
#   5. a failing chunk exits 1 and logs the real rc.
#
# Run: bash scripts/ci/ordinal-rederive-chunks-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/ordinal-rederive-chunks.sh"
[[ -f "$SCRIPT" ]] || { echo "ordinal-rederive-chunks-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
# check <label> <cmd...> — ok when the command succeeds.
check() { local l="$1"; shift; if "$@"; then ok "$l"; else bad "$l"; fi; }

# zfs stub: `zfs list -Hp -o avail <pool>` prints $FAKE/avail; logs argv;
# fails (exit 1) from call number ZFS_FAIL_FROM onward when set.
cat > "$TMP/bin/zfs" <<'STUB'
#!/usr/bin/env bash
n=$(( $(cat "$FAKE/zfs.calls" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$FAKE/zfs.calls"
echo "$*" >> "$FAKE/zfs.args"
if [ -n "${ZFS_FAIL_FROM:-}" ] && [ "$n" -ge "$ZFS_FAIL_FROM" ]; then
  echo "cannot open '${*: -1}': dataset does not exist" >&2
  exit 1
fi
cat "$FAKE/avail"
STUB
# ops stub: one argv line per call; each call consumes OPS_COST bytes of the
# fake pool; exits OPS_RC.
cat > "$TMP/fake-ops" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$FAKE/ops.args"
a=$(cat "$FAKE/avail")
echo $(( a - ${OPS_COST:-0} )) > "$FAKE/avail"
exit "${OPS_RC:-0}"
STUB
chmod +x "$TMP/bin/zfs" "$TMP/fake-ops"

# run <case> <avail> [VAR=value ...] — runs the script in a fresh fake dir;
# sets RC, OUT and FAKE.
run() {
  local name="$1" avail="$2"
  shift 2
  FAKE="$TMP/$name"
  mkdir -p "$FAKE"
  echo "$avail" > "$FAKE/avail"
  : > "$FAKE/ops.args"
  OUT="$(env PATH="$TMP/bin:$PATH" FAKE="$FAKE" OPS="$TMP/fake-ops" \
    CONFIG_PATH="$TMP/none.toml" START=100 BAND_END=350 CHUNK=100 \
    START_MIN_AVAIL_BYTES=1000 MIN_AVAIL_BYTES=500 "$@" bash "$SCRIPT" 2>&1)"
  RC=$?
}
calls() { wc -l < "$FAKE/ops.args" | tr -d ' '; }
has_arg() { grep -q -- "$1" "$FAKE/ops.args"; }

echo "== chunk bounds, argv, RUN_START_EPOCH =="
before=$(date +%s)
run bounds 5000
after=$(date +%s)
check "exit 0 on a clean run (got $RC)" test "$RC" -eq 0
check "three chunks for [100,350) at CHUNK=100" test "$(calls)" = 3
check "chunk -to is hi-1 (BAND_END exclusive)" test "$(sed -E 's/.*-from ([0-9]+) -to ([0-9]+).*/\1-\2/' "$FAKE/ops.args" | tr '\n' ' ')" = "100-199 200-299 300-349 "
check "every call carries -write" test "$(grep -c -- '-write' "$FAKE/ops.args")" = 3
check "default -bucket galexie-archive" has_arg '-bucket galexie-archive '
check "no EXTRA_FLAGS by default" test "$(grep -c -- '-changes-only' "$FAKE/ops.args")" = 0
first="$(head -n1 <<<"$OUT")"
epoch="${first#RUN_START_EPOCH=}"
check "first line is RUN_START_EPOCH (got '$first')" test "$first" != "$epoch"
check "RUN_START_EPOCH is now-60" test "$epoch" -ge $((before - 60)) -a "$epoch" -le $((after - 60))
check "zfs read as 'list -Hp -o avail data'" grep -qx 'list -Hp -o avail data' "$FAKE/zfs.args"
check "logs ALL CHUNKS DONE" grep -q 'ALL CHUNKS DONE' <<<"$OUT"

echo "== BUCKET / EXTRA_FLAGS / POOL =="
run bucket 5000 BUCKET=custom-bucket EXTRA_FLAGS='-changes-only -heartbeat /x'
check "BUCKET override is passed" has_arg '-bucket custom-bucket '
check "EXTRA_FLAGS word-split onto every call" test "$(grep -c -- '-changes-only -heartbeat /x$' "$FAKE/ops.args")" = 3
run nobucket 5000 BUCKET=
check "empty BUCKET omits -bucket" test "$(grep -c -- '-bucket' "$FAKE/ops.args")" = 0
run pool 5000 POOL=tank
check "POOL selects the dataset read" grep -qx 'list -Hp -o avail tank' "$FAKE/zfs.args"

echo "== disk gates =="
run startfloor 999
check "below START_MIN_AVAIL_BYTES exits 3 (got $RC)" test "$RC" -eq 3
check "start floor writes no chunk" test "$(calls)" = 0
run chunkfloor 1000 OPS_COST=300
check "per-chunk floor exits 3 (got $RC)" test "$RC" -eq 3
check "per-chunk floor stops after 2 chunks (1000->700->400<500)" test "$(calls)" = 2
check "per-chunk floor names the floor" grep -q 'below the MIN_AVAIL_BYTES floor 500' <<<"$OUT"
run unreadable 5000 ZFS_FAIL_FROM=1
check "unreadable avail at start exits 4 (got $RC)" test "$RC" -eq 4
check "unreadable avail writes no chunk" test "$(calls)" = 0
run unreadable-mid 5000 ZFS_FAIL_FROM=3
check "unreadable avail mid-run exits 4 (got $RC)" test "$RC" -eq 4
check "mid-run unreadable stops after 1 chunk" test "$(calls)" = 1
run garbage '-'
check "non-numeric avail exits 4 (got $RC)" test "$RC" -eq 4
run badfloor 5000 MIN_AVAIL_BYTES=1.25T
check "non-integer floor exits 2 (got $RC)" test "$RC" -eq 2

echo "== failing chunk =="
run chunkfail 5000 OPS_RC=7
check "failing chunk exits 1 (got $RC)" test "$RC" -eq 1
check "logs the real rc=7" grep -q 'FAILED rc=7' <<<"$OUT"
check "stops after the failing chunk" test "$(calls)" = 1

echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
