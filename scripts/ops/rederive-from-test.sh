#!/usr/bin/env bash
# rederive-from-test.sh — fixture tests for rederive-from.sh, the parallel
# "re-derive from ledger L" driver: every window ch-backfilled exactly once
# by concurrent workers, the seed / projected-rebuild / entry-count chain run
# only after a complete lake pass and in that order, and a run that fails or
# is killed resumes without redoing finished work or leaving a ch-backfill
# behind.
#
# Hermetic: df, sleep, clickhouse-client and the ops binary are PATH/env
# shims; the real phaseD-range.sh and ch-supply-flows-seed.sh run under them.
#
# Run: bash scripts/ops/rederive-from-test.sh

# check() evals its single-quoted condition so a failure can print the log.
# shellcheck disable=SC2016,SC2034
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
OPS_DIR="${OPS_DIR:-$PWD/scripts/ops}"
SCRIPT="$OPS_DIR/rederive-from.sh"

TMP="$(mktemp -d)"
# A regressed kill path leaves a worker retrying a hung window; reap it here.
trap 'pkill -f -- "$TMP/" 2>/dev/null; rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
check() { if eval "$1"; then ok "$2"; else bad "$2"; sed 's/^/       /' "$LOG" "$OUT" 2>/dev/null | tail -20; fi; }

mkdir -p "$TMP/bin"
cat > "$TMP/bin/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo 2000000000
STUB
# A retry or disk-pause loop must not hang the test: kill the caller after
# SLEEP_LIMIT calls so an unbounded loop fails instead.
cat > "$TMP/bin/sleep" <<'STUB'
#!/usr/bin/env bash
n=$(( $(cat "$SLEEP_COUNT" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$SLEEP_COUNT"
if [ "$n" -gt 60 ]; then kill -TERM "$PPID"; fi
exit 0
STUB
cat > "$TMP/bin/clickhouse-client" <<'STUB'
#!/usr/bin/env bash
echo "${LAKE_TIP:-2500000}"
STUB
# Records "<subcommand> <from> <to|-> <source|->". ch-backfill can fail one
# window (FAIL_FROM), hang on one (HANG_FROM, pid to HANG_PID) or report how
# many ch-backfills are in flight at once (RUNNING_DIR). Like the real
# binary, seed-entry-counts refuses a run that states no mode.
cat > "$TMP/bin/fake-ops" <<'STUB'
#!/usr/bin/env bash
cmd=$1; shift; from=; to=; src=; mode=
while [ $# -gt 0 ]; do
  case "$1" in -from) from=$2; shift 2 ;; -to) to=$2; shift 2 ;; -source) src=$2; shift 2 ;; -write|-dry-run) mode=$1; shift ;; *) shift ;; esac
done
echo "$cmd ${from:--} ${to:--} ${src:--}" >> "$OPS_LOG"
[ "$cmd" = seed-entry-counts ] && [ -z "$mode" ] && exit 2
if [ "$cmd" = ch-backfill ]; then
  if [ "$from" = "${HANG_FROM:-none}" ]; then echo "$$" > "$HANG_PID"; while :; do /bin/sleep 0.1; done; fi
  [ "$from" = "${FAIL_FROM:-none}" ] && exit 7
  if [ -n "${RUNNING_DIR:-}" ]; then
    touch "$RUNNING_DIR/$from"
    ls "$RUNNING_DIR" | wc -l | tr -d ' ' >> "$RUNNING_DIR.seen"
    /bin/sleep 0.5
    rm -f "$RUNNING_DIR/$from"
  fi
fi
[ "$cmd" = projected-rebuild ] && [ "$src" = "${FAIL_SOURCE:-none}" ] && exit 5
exit 0
STUB
chmod +x "$TMP/bin/"*

# fresh <name>: a new state dir and log for a scenario.
fresh() { SD="$TMP/$1.state"; LOG="$TMP/$1.log"; : > "$LOG"; }
# run [ENV=VAL …] -- [args…]: the driver against $SD, its ops calls in $OPSL,
# its stdout+stderr in $OUT and its exit status in $RC.
run() {
  local -a envs=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift
  OPSL="$TMP/ops.$RANDOM"; : > "$OPSL"; OUT="$OPSL.out"
  env PATH="$TMP/bin:$PATH" REDERIVE_LOG="$LOG" REDERIVE_OPS="$TMP/bin/fake-ops" \
      OPS_LOG="$OPSL" SLEEP_COUNT="$OPSL.sleeps" ${envs[@]+"${envs[@]}"} \
      bash "$SCRIPT" -state-dir "$SD" "$@" > "$OUT" 2>&1
  RC=$?
}
calls() { grep -c "^$1" "$OPSL"; }
# 5 windows: 1000, 1001000, 2001000, 3001000 and the short [4001000,4500999].
RANGE=(-from 1000 -to 4500999)
WIN="1000 1001000 2001000 3001000 4001000"
all_windows_once() {
  [ "$(sort "$SD/windows.txt" | tr '\n' ' ')" = "$WIN " ]
}

echo "rederive-from-test: arguments"
fresh args
run -- -to 5
check '[ "$RC" -eq 2 ] && [ ! -s "$OPSL" ]' "no -from ⇒ usage exit 2, nothing run"
run -- -from 1000 -projected 'soroswap;rm'
check '[ "$RC" -eq 2 ] && [ ! -s "$OPSL" ]' "a -projected name outside [a-z0-9_] ⇒ usage exit 2"
run -- -from 5000 -to 4000
check '[ "$RC" -eq 2 ] && [ ! -s "$OPSL" ]' "-to below -from ⇒ exit 2, nothing run"

echo "rederive-from-test: healthy run"
fresh happy
mkdir -p "$TMP/running"
run RUNNING_DIR="$TMP/running" -- "${RANGE[@]}" -workers 3 -projected soroswap,aquarius
check '[ "$RC" -eq 0 ]' "healthy run ⇒ exit 0"
check '[ "$(calls ch-backfill)" -eq 5 ] && all_windows_once' "every window ch-backfilled and recorded exactly once"
check 'grep -qx "ch-backfill 4001000 4500999 -" "$OPSL"' "the last window stops at -to"
check '[ "$(sort -n "$TMP/running.seen" | tail -1)" -ge 2 ]' "workers ran ch-backfills concurrently"
check 'grep -qx "ch-supply 1000 1000999 -" "$OPSL" && [ "$(calls ch-supply)" -eq 5 ]' "supply_flows seeded over [L,TO]"
check 'grep -qx "projected-rebuild 1000 - soroswap" "$OPSL" && grep -qx "projected-rebuild 1000 - aquarius" "$OPSL"' "each -projected source rebuilt from L behind the live tail (no -to)"
check 'awk "/^ch-backfill/ { b = NR } /^ch-supply/ && !s { s = NR } END { exit !(b && s && b < s) }" "$OPSL"' "seeding starts only after the last window"
check '[ "$(tail -1 "$OPSL")" = "seed-entry-counts - - -" ] && [ "$(calls seed-entry-counts)" -eq 1 ]' "entry counts recomputed once, last"
check '[ "$(grep -c REDERIVE_COMPLETE "$LOG")" -eq 1 ] && [ ! -e "$SD/lock" ] && [ "$(cat "$SD/to")" = 4500999 ]' "REDERIVE_COMPLETE logged, lock released, end pinned"

run -- "${RANGE[@]}" -workers 3 -projected soroswap,aquarius
check '[ "$RC" -eq 0 ] && [ ! -s "$OPSL" ] && [ "$(grep -c REDERIVE_COMPLETE "$LOG")" -eq 2 ]' "re-run of a finished command ⇒ exit 0 with nothing redone"
run -- -from 1000 -to 9000000
check '[ "$RC" -eq 2 ] && [ ! -s "$OPSL" ]' "re-run with a different -to ⇒ refused, nothing run"

echo "rederive-from-test: queue order"
fresh order
run -- "${RANGE[@]}" -workers 1
check '[ "$RC" -eq 0 ] && [ "$(grep "^ch-backfill" "$OPSL" | cut -d" " -f2 | tr "\n" " ")" = "4001000 3001000 2001000 1001000 1000 " ]' "windows taken newest-first"

echo "rederive-from-test: end pinned from the lake tip"
fresh tip
run LAKE_TIP=2500000 -- -from 1000 -workers 2
check '[ "$RC" -eq 0 ] && [ "$(cat "$SD/to")" = 2500000 ] && grep -qx "ch-backfill 2001000 2500000 -" "$OPSL"' "no -to ⇒ the lake tip is pinned and is the last window's end"
run LAKE_TIP=3500000 -- -from 1000 -workers 2
check '[ "$RC" -eq 0 ] && [ ! -s "$OPSL" ]' "resume after the tip moved keeps the pinned end"

echo "rederive-from-test: a window that keeps failing"
fresh winfail
run FAIL_FROM=2001000 -- "${RANGE[@]}" -workers 2 -projected soroswap
check '[ "$RC" -eq 7 ]' "always-failing window ⇒ the window's exit code (7)"
check '[ "$(grep -c "^ch-backfill 2001000" "$OPSL")" -eq 3 ]' "the failing window tried PHASED_MAX_ATTEMPTS=3 times"
check '[ "$(calls ch-supply)" -eq 0 ] && [ "$(calls projected-rebuild)" -eq 0 ] && [ "$(calls seed-entry-counts)" -eq 0 ]' "no seed or rebuild over an incomplete lake"
check '! grep -q "LAKE DONE\|REDERIVE_COMPLETE" "$LOG" && ! grep -qx 2001000 "$SD/windows.txt"' "neither LAKE DONE nor REDERIVE_COMPLETE logged"
done_before="$(wc -l < "$SD/windows.txt" | tr -d ' ')"
run -- "${RANGE[@]}" -workers 2 -projected soroswap
check '[ "$RC" -eq 0 ] && all_windows_once' "re-run completes every window exactly once"
check '[ "$(calls ch-backfill)" -eq $((5 - done_before)) ]' "re-run redoes only the $((5 - done_before)) unfinished window(s)"

echo "rederive-from-test: killed mid-window"
fresh killed
OPSL="$TMP/ops.killed"; : > "$OPSL"; OUT="$OPSL.out"
env PATH="$TMP/bin:$PATH" REDERIVE_LOG="$LOG" REDERIVE_OPS="$TMP/bin/fake-ops" OPS_LOG="$OPSL" \
    SLEEP_COUNT="$OPSL.sleeps" HANG_FROM=3001000 HANG_PID="$TMP/hang.pid" \
    bash "$SCRIPT" -state-dir "$SD" "${RANGE[@]}" -workers 2 > "$OUT" 2>&1 &
driver=$!
for _ in $(seq 1 100); do [ -s "$TMP/hang.pid" ] && break; /bin/sleep 0.1; done
kill -TERM "$driver"
wait "$driver"; RC=$?
hung="$(cat "$TMP/hang.pid" 2>/dev/null)"
for _ in $(seq 1 30); do kill -0 "$hung" 2>/dev/null || break; /bin/sleep 0.1; done
check '[ -n "$hung" ] && [ "$RC" -eq 143 ]' "SIGTERM mid-window ⇒ exit 143"
check '! kill -0 "$hung" 2>/dev/null' "the in-flight ch-backfill is killed with the run"
/bin/sleep 0.5
check '! pgrep -f -- "$SD/" >/dev/null' "no worker of the killed run survives it"
check '! grep -q "LAKE DONE\|REDERIVE_COMPLETE" "$LOG" && [ ! -e "$SD/lock" ] && ! grep -qx 3001000 "$SD/windows.txt"' "killed run: no completion logged, window not recorded, lock released"
kill -9 "$hung" 2>/dev/null
done_before="$(wc -l < "$SD/windows.txt" | tr -d ' ')"
run -- "${RANGE[@]}" -workers 2
check '[ "$RC" -eq 0 ] && all_windows_once && [ "$(calls ch-backfill)" -eq $((5 - done_before)) ]' "resume after the kill finishes only the unfinished windows"

echo "rederive-from-test: a failing chain stage"
fresh stagefail
run FAIL_SOURCE=aquarius -- "${RANGE[@]}" -workers 2 -projected soroswap,aquarius
check '[ "$RC" -eq 5 ] && [ "$(calls seed-entry-counts)" -eq 0 ] && ! grep -q REDERIVE_COMPLETE "$LOG"' "a failed projected-rebuild stops the chain with its exit code"
run -- "${RANGE[@]}" -workers 2 -projected soroswap,aquarius
check '[ "$RC" -eq 0 ] && [ "$(cat "$OPSL" | tr "\n" "|")" = "projected-rebuild 1000 - aquarius|seed-entry-counts - - -|" ]' "resume re-runs only the failed source, then the entry counts"

echo "rederive-from-test: one run per state dir"
fresh locked
mkdir -p "$SD/lock"; echo "$$" > "$SD/lock/pid"
run -- "${RANGE[@]}"
check '[ "$RC" -ne 0 ] && [ ! -s "$OPSL" ] && grep -q "another run" "$OUT"' "a live holder's lock ⇒ refused, nothing run"
sleep_pid() { /bin/sleep 0 & echo $!; }
dead="$(sleep_pid)"; wait "$dead" 2>/dev/null
echo "$dead" > "$SD/lock/pid"
run -- "${RANGE[@]}"
check '[ "$RC" -eq 0 ] && all_windows_once' "a dead holder's lock is taken over"

echo "----"
echo "rederive-from-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
