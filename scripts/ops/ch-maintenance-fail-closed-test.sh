#!/usr/bin/env bash
# ch-maintenance-fail-closed-test.sh — fixture tests for the ClickHouse
# maintenance and Phase-D backfill drivers' failure paths:
#
#   recompress-lec.sh — never OPTIMIZEs; a partition is DONE (freeze released,
#     state written) only when its rows, all-column hash and per-part rows
#     are unchanged by REWRITE PARTS; any failure, a mismatch, a failed
#     mutation, low disk or a held heavy-job lock exits non-zero with the
#     freeze kept; the tip partition and recorded partitions are skipped.
#   recompress-others.sh — a ClickHouse exception
#     (HTTP 500, message in the body) must stop the run non-zero and must
#     never be followed by a DONE or *_COMPLETE line; it
#     must put every table's merge ceiling back to the value it READ before
#     raising it, on failure, on SIGTERM and on success.
#   phaseD-backfill.sh / phaseD-range.sh — a window that keeps failing stops
#     the run after PHASED_MAX_ATTEMPTS tries instead of retrying forever; a
#     transient failure is still retried.
#
# Hermetic: curl, clickhouse-client, df, date, sleep, the heavy-job wrapper and the ops binary are shims. The curl
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
rewritten() { grep -q "REWRITE PARTS" "$CH_LOG"; }
case "$sql" in
  *"FROM system.columns"*intra_ledger_seq*) echo "${LEC_CODECS:-5}" ;;
  *"AND active GROUP BY partition_id"*)
    [ -n "${PARTS_FAIL:-}" ] && http500 "Code: 159. DB::Exception: Timeout exceeded. (TIMEOUT_EXCEEDED)"
    printf '%b' "${LEC_PARTS:-40\n41\n65\n}"
    ;;
  *"max(toUInt32(partition_id))"*) echo "${LEC_TIP:-65}" ;;
  *"REWRITE PARTS IN PARTITION"*)
    [ -n "${REWRITE_FAIL:-}" ] && http500 "Code: 243. DB::Exception: Cannot reserve 300.00 GiB, not enough space. (NOT_ENOUGH_SPACE)"
    ;;
  *"FREEZE PARTITION"*|*"KILL MUTATION"*) ;;
  *"SELECT mutation_id"*) echo "mutation_9.txt" ;;
  *"latest_fail_reason FROM system.mutations"*)
    if [ -n "${MUT_FAIL:-}" ]; then echo "0|$MUT_FAIL"; else echo "1|"; fi
    ;;
  *"count() FROM system.mutations WHERE database='stellar' AND table='ledger_entry_changes'") echo 3 ;;
  *"FROM system.mutations"*"NOT is_done"*) echo "${MUT_OPEN:-0}" ;;
  *"sum(cityHash64(*))"*) if [ -n "${HASH_DRIFT:-}" ] && rewritten; then echo 222; else echo 111; fi ;;
  *"count() FROM stellar.ledger_entry_changes"*) echo 1000 ;;
  *"count() FROM system.parts"*) echo 4 ;;
  *"groupArray(rows)"*) echo 777 ;;
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
  *"FROM system.columns"*) echo "${ZSTD_COLS:-1}" ;;
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

# Models clickhouse-client -q for recompress-lec.sh over the curl shim: a server
# exception exits non-zero; CHC_FAIL_MATCH fails any query containing it, as auth does.
cat > "$TMP/bin/fake-chc" <<'STUB'
#!/usr/bin/env bash
sql=""
while [ $# -gt 0 ]; do
  case "$1" in -q) sql="${2-}"; shift 2 ;; *) shift ;; esac
done
if [ -n "${CHC_FAIL_MATCH:-}" ] && [[ "$sql" == *"$CHC_FAIL_MATCH"* ]]; then
  printf '%s\n' "$sql" >> "$CH_LOG"
  echo "Code: 516. DB::Exception: default: Authentication failed. (AUTHENTICATION_FAILED)" >&2
  exit 4
fi
exec curl --fail-with-body --data-binary "$sql"
STUB

cat > "$TMP/bin/date" <<'STUB'
#!/usr/bin/env bash
if [ "$*" = "-u +%H:%M" ]; then echo "$FAKE_HHMM"; exit 0; fi
exec /bin/date "$@"
STUB

# Models run-heavy-job.sh: exit 75 without running the payload while the lock is held.
cat > "$TMP/bin/fake-heavy" <<'STUB'
#!/usr/bin/env bash
echo "$1" >> "$HEAVY_LOG"
[ -n "${HEAVY_HELD:-}" ] && exit 75
shift
exec "$@"
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
      RECOMPRESS_STATE="$STATEF" HEAVY_JOB="$TMP/bin/fake-heavy" HEAVY_LOG="$OPSL" FAKE_HHMM=12:00 \
      RECOMPRESS_CH="$TMP/bin/fake-chc --port 9300" \
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

RW="REWRITE PARTS IN PARTITION"

run lec_happy recompress-lec.sh --
expect_rc_zero "two healthy partitions below the tip"
expect_count "$LOG" " DONE " 2 "healthy run: both partitions DONE"
expect_count "$STMT" "FREEZE PARTITION 40 WITH NAME 'lec_codec_40'" 2 "healthy run: p40 frozen, then unfrozen"
expect_count "$STMT" "UNFREEZE PARTITION" 2 "healthy run: both freezes released"
if [ "$(tr '\n' ' ' < "$STATEF")" = "40 41 " ]; then ok "healthy run: both partitions recorded done"; else bad "healthy run: state is '$(cat "$STATEF" 2>/dev/null)'"; fi
expect_count "$LOG" "RECOMPRESS_COMPLETE" 1 "healthy run: RECOMPRESS_COMPLETE logged"
expect_absent "$STMT" "$RW 65" "healthy run: the tip partition is never rewritten"
expect_absent "$STMT" "OPTIMIZE" "healthy run: no OPTIMIZE issued (it deduplicates)"
expect_count "$OPSL" "recompress-lec-p" 2 "healthy run: the heavy-job lock taken once per partition"

run lec_codecs recompress-lec.sh LEC_CODECS=4 --
expect_rc_nonzero "codecs not applied"
expect_absent "$STMT" "ALTER" "codecs missing: no ALTER issued"
expect_count "$LOG" "ledger_entry_changes_codecs.sql" 1 "codecs missing: message names the DDL to apply"

run lec_rw500 recompress-lec.sh REWRITE_FAIL=1 --
expect_rc_nonzero "REWRITE answered HTTP 500"
expect_absent "$LOG" " DONE " "REWRITE 500: no partition logged DONE"
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "REWRITE 500: RECOMPRESS_COMPLETE never logged"
expect_absent "$STMT" "UNFREEZE" "REWRITE 500: freeze not released"
expect_count "$STMT" "$RW" 1 "REWRITE 500: stopped at the first failing partition"

run lec_mutfail recompress-lec.sh "MUT_FAIL=Code: 241. DB::Exception: Memory limit exceeded" --
expect_rc_nonzero "mutation reports a fail reason"
expect_count "$STMT" "KILL MUTATION" 1 "mutation failure: KILL MUTATION issued"
expect_absent "$LOG" " DONE " "mutation failure: no partition logged DONE"
expect_absent "$STMT" "UNFREEZE" "mutation failure: freeze not released"

run lec_mismatch recompress-lec.sh HASH_DRIFT=1 --
expect_rc_nonzero "rows changed by the rewrite"
expect_count "$LOG" "p40 MISMATCH" 1 "mismatch: logged MISMATCH"
expect_count "$LOG" "lec_codec_40" 1 "mismatch: the log names the kept freeze"
expect_absent "$STMT" "UNFREEZE" "mismatch: freeze not released"
if [ -s "$STATEF" ]; then bad "mismatch: state written: $(cat "$STATEF")"; else ok "mismatch: state not written"; fi
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "mismatch: RECOMPRESS_COMPLETE never logged"

# 600 GiB floor = 629145600 KiB; plus the 2 GiB partition, 630000000 KiB is short.
run lec_floor recompress-lec.sh DF_AVAIL_KB=630000000 --
expect_rc_nonzero "free space below floor + partition"
expect_absent "$STMT" "FREEZE" "low disk: aborted before FREEZE"

run lec_dfglitch recompress-lec.sh DF_AVAIL_KB=- --
expect_rc_nonzero "df glitch"
expect_absent "$STMT" "FREEZE" "df glitch: aborted before FREEZE"

run lec_locked recompress-lec.sh HEAVY_HELD=1 RECOMPRESS_LOCK_ATTEMPTS=3 --
expect_rc_nonzero "heavy-job lock held beyond the attempts"
expect_count "$OPSL" "recompress-lec-p40" 3 "lock held: exactly 3 attempts"
expect_absent "$STMT" "FREEZE" "lock held: nothing frozen"
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "lock held: RECOMPRESS_COMPLETE never logged"

printf '40\n' > "$TMP/lec_done.state"
run lec_skip recompress-lec.sh RECOMPRESS_STATE="$TMP/lec_done.state" --
expect_rc_zero "one partition already recorded done"
expect_absent "$STMT" "$RW 40" "already done: p40 not rewritten"
expect_count "$STMT" "$RW 41" 1 "already done: p41 rewritten"
expect_count "$LOG" "p40 SKIP" 1 "already done: skip logged"

run lec_onlytip recompress-lec.sh RECOMPRESS_PARTITIONS=65 --
expect_rc_nonzero "the tip requested explicitly"
expect_absent "$STMT" "ALTER" "tip requested: no ALTER issued"

run lec_subset recompress-lec.sh RECOMPRESS_PARTITIONS=41 --
expect_rc_zero "a requested subset"
expect_absent "$STMT" "$RW 40" "subset: unrequested p40 not rewritten"

run lec_openmut recompress-lec.sh MUT_OPEN=1 --
expect_rc_nonzero "a mutation already open on the table"
expect_absent "$STMT" "FREEZE" "open mutation: nothing frozen"

run lec_quiet recompress-lec.sh FAKE_HHMM=02:00 SLEEP_LIMIT=2 --
expect_rc_nonzero "inside the quiet window"
expect_absent "$OPSL" "recompress-lec-p" "quiet window: heavy job never started"

run lec_noauth recompress-lec.sh CHC_FAIL_MATCH="FROM system.mutations" --
expect_rc_nonzero "client cannot read system.mutations at startup"
expect_count "$LOG" "cannot read system.mutations" 1 "startup probe: names the transport problem"
expect_absent "$STMT" "ALTER" "startup probe: no ALTER issued"

run lec_clientexit recompress-lec.sh CHC_FAIL_MATCH="latest_fail_reason" --
expect_rc_nonzero "client exits non-zero while polling the mutation"
expect_absent "$LOG" " DONE " "client exit: no partition logged DONE"
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "client exit: RECOMPRESS_COMPLETE never logged"
expect_absent "$STMT" "UNFREEZE" "client exit: freeze not released"
expect_count "$LOG" "freeze lec_codec_40 kept" 1 "client exit: the log names the kept freeze"

run lec_parts500 recompress-lec.sh PARTS_FAIL=1 --
expect_rc_nonzero "partition-list query answered HTTP 500"
expect_absent "$LOG" "RECOMPRESS_COMPLETE" "partition-list 500: RECOMPRESS_COMPLETE never logged"
expect_absent "$STMT" "ALTER" "partition-list 500: no ALTER issued"

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

run oth_lz4 recompress-others.sh ZSTD_COLS=0 --
expect_rc_nonzero "an XDR column still on the default LZ4 codec"
expect_absent "$STMT" "OPTIMIZE TABLE" "LZ4 column: no partition rewritten"
expect_absent "$STMT" "MODIFY SETTING" "LZ4 column: no merge ceiling raised"
expect_absent "$LOG" "OTHERS_COMPLETE" "LZ4 column: OTHERS_COMPLETE never logged"

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

# A window is done only up to the end it ran to: a run that stopped short at 1499999 must not make a longer run
# skip 1500000-1999999.
run rng_short phaseD-range.sh -- 0 1499999 "$TMP/rng_grow.state"
expect_rc_zero "a range ending inside a window"
run rng_grown phaseD-range.sh -- 0 2999999 "$TMP/rng_grow.state"
expect_rc_zero "the same state file, a longer range"
if ! grep -qx 0 "$OPSL"; then ok "longer range: the full first window is skipped"; else bad "longer range: the full first window is skipped"; fi
expect_count "$OPSL" "1000000" 1 "longer range: the window that ended short runs again"
expect_count "$OPSL" "2000000" 1 "longer range: the new window runs"

# An older run recorded bare starts. A bare start is trusted only when its successor is recorded.
printf '0\n1000000\n' > "$TMP/rng_bare.state"
run rng_legacy phaseD-range.sh -- 0 2999999 "$TMP/rng_bare.state"
expect_rc_zero "a state file of bare starts"
if ! grep -qx 0 "$OPSL"; then ok "bare starts: a window with a recorded successor is skipped"; else bad "bare starts: a window with a recorded successor is skipped"; fi
expect_count "$OPSL" "1000000" 1 "bare starts: the last recorded window, whose end is unknown, runs again"

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
