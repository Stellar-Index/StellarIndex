#!/usr/bin/env bash
# ch-maintenance-fail-closed-test.sh — fixture tests for the ClickHouse
# maintenance and Phase-D backfill drivers' failure paths:
#
#   recompress-lec.sh / recompress-others.sh — a ClickHouse exception
#     (HTTP 500, message in the body) must stop the run non-zero and must
#     never be followed by a DONE or *_COMPLETE line; recompress-others.sh
#     must put every table's merge ceiling back to the value it READ before
#     raising it, on failure, on SIGTERM and on success.
#   phaseD-backfill.sh / phaseD-range.sh — a window that keeps failing stops
#     the run after PHASED_MAX_ATTEMPTS tries instead of retrying forever; a
#     transient failure is still retried.
#
# Hermetic: curl, df, sleep and the ops binary are PATH/env shims. The curl
# shim models real curl — an HTTP 500 exits 0 with the exception as the
# body unless --fail/--fail-with-body is passed — and the sleep shim kills
# its caller after SLEEP_LIMIT calls, so an unbounded retry loop fails the
# test instead of hanging it.
#
# Run: bash scripts/ops/ch-maintenance-fail-closed-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
OPS_DIR="${OPS_DIR:-$PWD/scripts/ops}"
for s in recompress-lec.sh recompress-others.sh phaseD-backfill.sh phaseD-range.sh; do
  [ -r "$OPS_DIR/$s" ] || { echo "ch-maintenance-fail-closed-test: missing $OPS_DIR/$s" >&2; exit 2; }
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin"

cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
sql=""; failmode=""
while [ $# -gt 0 ]; do
  case "$1" in
    --data-binary) sql="${2-}"; shift 2 ;;
    --fail|-f) failmode=fail; shift ;;
    --fail-with-body) failmode=body; shift ;;
    --max-time) shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' "$sql" >> "$CH_LOG"
http500() {
  [ "$failmode" = fail ] || printf '%s\n' "$1"
  if [ -n "$failmode" ]; then
    echo "curl: (22) The requested URL returned error: 500" >&2
    exit 22
  fi
  exit 0
}
case "$sql" in
  *"OPTIMIZE TABLE"*)
    if [ -n "${OPT_KILL:-}" ]; then kill -TERM "$PPID"; exit 0; fi
    [ -n "${OPT_FAIL:-}" ] && http500 "Code: 241. DB::Exception: Memory limit (total) exceeded. (MEMORY_LIMIT_EXCEEDED)"
    ;;
  *"FROM system.tables"*) printf 'v=%s\n' "${ORIG_CEILING:-}" ;;
  *"GROUP BY partition"*)
    [ -n "${PARTS_FAIL:-}" ] && http500 "Code: 159. DB::Exception: Timeout exceeded. (TIMEOUT_EXCEEDED)"
    printf '%b' "${PARTS:-40\n41\n}"
    ;;
  *"FROM system.merges"*) echo 0 ;;
  *"sum(bytes_on_disk)"*) echo 2147483648 ;;
  *"max(ledger_seq)"*) echo 63000000 ;;
esac
exit 0
STUB

cat > "$TMP/bin/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "${DF_AVAIL_KB:-2000000000}"
STUB

cat > "$TMP/bin/sleep" <<'STUB'
#!/usr/bin/env bash
n=$(( $(cat "$SLEEP_COUNT" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$SLEEP_COUNT"
if [ "$n" -gt "${SLEEP_LIMIT:-40}" ]; then kill -TERM "$PPID"; fi
exit 0
STUB

cat > "$TMP/bin/fake-ops" <<'STUB'
#!/usr/bin/env bash
from=""
while [ $# -gt 0 ]; do
  case "$1" in -from) from="$2"; shift 2 ;; *) shift ;; esac
done
echo "$from" >> "$OPS_LOG"
if [ "$from" = "${OPS_FAIL_FROM:-none}" ] && [ "$(grep -cx "$from" "$OPS_LOG")" -le "${OPS_FAIL_TIMES:-999999}" ]; then
  exit 7
fi
exit 0
STUB
chmod +x "$TMP/bin/"*

# run <case> <script> [ENV=VAL …] -- [args…]
# Leaves the script's log in $LOG, its SQL in $STMT, its ops calls in $OPSL,
# its sleep count in $SLEEPS and its exit status in $RC.
run() {
  local name="$1" script="$2"; shift 2
  local -a envs=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift
  LOG="$TMP/$name.log"; : > "$LOG"
  STMT="$TMP/$name.sql"; : > "$STMT"
  OPSL="$TMP/$name.ops"; : > "$OPSL"
  STATEF="$TMP/$name.state"; rm -f "$STATEF"
  SLEEPC="$TMP/$name.sleeps"; rm -f "$SLEEPC"
  env PATH="$TMP/bin:$PATH" CH_LOG="$STMT" OPS_LOG="$OPSL" SLEEP_COUNT="$SLEEPC" \
      RECOMPRESS_LOG="$LOG" PHASED_LOG="$LOG" PHASED_STATE="$STATEF" \
      PHASED_OPS="$TMP/bin/fake-ops" \
      ${envs[@]+"${envs[@]}"} bash "$OPS_DIR/$script" "$@" >> "$LOG" 2>&1
  RC=$?
  SLEEPS="$(cat "$SLEEPC" 2>/dev/null || echo 0)"
}

expect_rc_nonzero() { if [ "$RC" -ne 0 ]; then ok "$1 ⇒ non-zero exit"; else bad "$1 ⇒ exited 0"; fi; }
expect_rc_zero() { if [ "$RC" -eq 0 ]; then ok "$1 ⇒ exit 0"; else bad "$1 ⇒ exited $RC"; sed 's/^/       /' "$LOG"; fi; }
expect_absent() { # expect_absent <file> <fixed-string> <label>
  if grep -qF -- "$2" "$1"; then bad "$3"; sed 's/^/       /' "$1"; else ok "$3"; fi
}
expect_count() { # expect_count <file> <fixed-string> <n> <label>
  local got
  got="$(grep -cF -- "$2" "$1")"
  if [ "$got" -eq "$3" ]; then ok "$4"; else bad "$4 (got $got, want $3)"; fi
}

echo "ch-maintenance-fail-closed-test: recompress-lec.sh"

run lec_opt500 recompress-lec.sh OPT_FAIL=1 --
expect_rc_nonzero "OPTIMIZE answered HTTP 500"
expect_absent "$LOG" "DONE" "OPTIMIZE 500: no partition logged DONE"
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "OPTIMIZE 500: RECOMPRESS_COMPLETE never logged"
expect_count "$STMT" "OPTIMIZE TABLE" 1 "OPTIMIZE 500: stopped at the first failing partition"

run lec_parts500 recompress-lec.sh PARTS_FAIL=1 --
expect_rc_nonzero "partition-list query answered HTTP 500"
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "partition-list 500: RECOMPRESS_COMPLETE never logged"
expect_count "$STMT" "OPTIMIZE TABLE" 0 "partition-list 500: no OPTIMIZE issued"

run lec_dfglitch recompress-lec.sh DF_AVAIL_KB=- --
expect_rc_nonzero "every partition skipped on a df glitch"
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "df glitch: RECOMPRESS_COMPLETE never logged"

run lec_happy recompress-lec.sh --
expect_rc_zero "two healthy partitions"
expect_count "$LOG" "DONE" 2 "healthy run: both partitions DONE"
expect_count "$LOG" "RECOMPRESS_COMPLETE" 1 "healthy run: RECOMPRESS_COMPLETE logged"

echo "ch-maintenance-fail-closed-test: recompress-others.sh"
RESET="RESET SETTING max_bytes_to_merge_at_max_space_in_pool"

run oth_opt500 recompress-others.sh OPT_FAIL=1 --
expect_rc_nonzero "OPTIMIZE answered HTTP 500"
expect_absent "$LOG" "OTHERS_COMPLETE" "OPTIMIZE 500: OTHERS_COMPLETE never logged"
expect_absent "$LOG" "/40 done" "OPTIMIZE 500: the failed partition not logged done"
expect_count "$STMT" "$RESET" 3 "OPTIMIZE 500: all three unset ceilings RESET on the way out"

run oth_sigterm recompress-others.sh OPT_KILL=1 --
expect_rc_nonzero "SIGTERM mid-OPTIMIZE"
expect_absent "$LOG" "OTHERS_COMPLETE" "SIGTERM: OTHERS_COMPLETE never logged"
expect_count "$STMT" "$RESET" 3 "SIGTERM: all three unset ceilings RESET by the trap"

run oth_explicit recompress-others.sh ORIG_CEILING=107374182400 --
expect_rc_zero "healthy run over tables with an explicit ceiling"
expect_count "$STMT" "max_bytes_to_merge_at_max_space_in_pool = 107374182400" 3 "explicit ceiling: each table restored to the 100 GiB it had"
expect_absent "$STMT" "161061273600" "explicit ceiling: no hardcoded 150 GiB written"
expect_count "$LOG" "OTHERS_COMPLETE" 1 "explicit ceiling: OTHERS_COMPLETE logged"

run oth_happy recompress-others.sh --
expect_rc_zero "healthy run over tables inheriting the server default"
expect_count "$STMT" "$RESET" 3 "unset ceiling: each table RESET, not pinned to a constant"
expect_absent "$STMT" "161061273600" "unset ceiling: no hardcoded 150 GiB written"

echo "ch-maintenance-fail-closed-test: phaseD-range.sh"

run rng_stuck phaseD-range.sh OPS_FAIL_FROM=1000000 -- 0 2999999 "$TMP/rng_stuck.state"
expect_rc_nonzero "a window that always fails"
expect_count "$OPSL" "1000000" 3 "always-failing window: exactly PHASED_MAX_ATTEMPTS=3 tries"
expect_absent "$LOG" "RANGE [0,2999999] COMPLETE" "always-failing window: RANGE COMPLETE never logged"
if [ "$SLEEPS" -le 2 ]; then ok "always-failing window: 2 retry sleeps, not an endless loop"; else bad "always-failing window: $SLEEPS sleeps — retried without bound"; fi

run rng_transient phaseD-range.sh OPS_FAIL_FROM=1000000 OPS_FAIL_TIMES=1 -- 0 2999999 "$TMP/rng_transient.state"
expect_rc_zero "a window that fails once then succeeds"
n="$(wc -l < "$STATEF" | tr -d ' ')"
if [ "$n" -eq 3 ]; then ok "transient failure: all three windows recorded done"; else bad "transient failure: $n windows recorded, want 3"; fi
expect_count "$LOG" "RANGE [0,2999999] COMPLETE" 1 "transient failure: RANGE COMPLETE logged"

echo "ch-maintenance-fail-closed-test: phaseD-backfill.sh"

run bf_stuck phaseD-backfill.sh OPS_FAIL_FROM=55000000 --
expect_rc_nonzero "a window that always fails"
expect_count "$OPSL" "55000000" 3 "always-failing window: exactly PHASED_MAX_ATTEMPTS=3 tries"
expect_absent "$LOG" "PHASED_BACKFILL_COMPLETE" "always-failing window: PHASED_BACKFILL_COMPLETE never logged"
if [ "$SLEEPS" -le 2 ]; then ok "always-failing window: 2 retry sleeps, not an endless loop"; else bad "always-failing window: $SLEEPS sleeps — retried without bound"; fi

run bf_happy phaseD-backfill.sh --
expect_rc_zero "both ranges healthy"
n="$(wc -l < "$STATEF" | tr -d ' ')"
if [ "$n" -eq 48 ]; then ok "healthy run: 10 + 38 windows recorded"; else bad "healthy run: $n windows recorded, want 48"; fi
expect_count "$LOG" "PHASED_BACKFILL_COMPLETE" 1 "healthy run: PHASED_BACKFILL_COMPLETE logged"

echo "----"
echo "ch-maintenance-fail-closed-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
