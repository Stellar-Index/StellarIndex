#!/usr/bin/env bash
# ops-verdict-test.sh — fixture tests for scripts/ops/ops-verdict.sh.
#
# `systemctl` is STUBBED on PATH with a file-backed fake unit, so this
# runs on any box (macOS included), starts nothing and restarts nothing.
# The fixtures are the property values MEASURED on r1 on 2026-09-07:
#
#   creators-rollup.service   Type=oneshot RemainAfterExit=no
#                             ran 11:32:26 → 11:48:29 CEST
#                             ActiveEnterTimestampMonotonic=0  ← never active
#                             InactiveEnterTimestampMonotonic=9403883529041
#   sponsors-rollup.service   same shape; token 9405834232409
#   apport-autoreport.service never ran since boot, and still reports
#                             Result=success ExecMainStatus=0
#   apparmor.service          Type=oneshot RemainAfterExit=yes, ends
#                             `active` with a populated
#                             ActiveEnterTimestampMonotonic
#
# The properties pinned here are the ones whose absence turns a report
# into a confident, wrong answer:
#
#   1. the NAIVE `while systemctl is-active --quiet U` loop makes ZERO
#      iterations against a running oneshot and then quotes the previous
#      run's Result — the defect, reproduced;
#   2. wait_for_oneshot waits on ActiveState instead, and REFUSES to
#      report unless the unit's completion timestamp advanced past a
#      baseline taken before the run;
#   3. a unit that has never run at all — Result=success by default —
#      is refused, not reported as a success;
#   4. a RemainAfterExit=yes oneshot ends `active`, and is waited on by
#      its ActiveEnterTimestamp instead of hanging until the timeout;
#   5. gate_passed reads the sentinel, never an exit code, and treats an
#      absent, empty, missing or AMBIGUOUS log as a failure;
#   6. require_output / require_affected fail a command that matched
#      nothing and exited 0.
#
# Run: bash scripts/ops/ops-verdict-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LIB="$PWD/scripts/ops/ops-verdict.sh"
[[ -r "$LIB" ]] || { echo "ops-verdict-test: missing $LIB" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
# res <rc> <what> [<detail shown on failure>]
res() { if [[ "$1" -eq 0 ]]; then ok "$2"; else bad "$2 — ${3:-}"; fi; }
# t <test(1) args...> — a plain `test` behind a function so `res $?`
# reads a command status (SC2319 objects to `$?` straight after `[[ ]]`).
t() { test "$@"; }

# ─── the fake systemd ───────────────────────────────────────────────
#
# One directory per unit under $FAKE:
#
#   props   static KEY=VALUE lines (LoadState, Type, RemainAfterExit,
#           ConditionResult) — what does not change across a run
#   steps   one line per systemctl invocation, TAB-separated:
#             <ActiveState> <SubState> <token> <Result> <ExecMainStatus>
#           The last line is sticky, so a unit that reaches a terminal
#           state stays there however long a caller polls.
#   cursor  index of the next step
#
# The stub answers `show` (KEY=VALUE for every property asked for) and
# `is-active` with systemd's own contract: exit 0 only when ActiveState
# is `active` or `reloading`, exit 3 otherwise. That contract is what
# makes the naive loop exit on iteration zero, and r1 shows why it is
# not a detail: creators-rollup.service completed a 16-minute run on
# 2026-09-07 with ActiveEnterTimestampMonotonic still 0, so `is-active`
# could not have returned 0 at any instant of it.

FAKE="$TMP/units"
mkdir -p "$FAKE" "$TMP/bin"
export PATH="$TMP/bin:$PATH"

cat > "$TMP/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
root=${FAKE_UNITS:?}
# Flags may appear anywhere on a real systemctl command line
# (`is-active --quiet U` and `is-active U --quiet` are both valid), so
# parse positionally rather than assuming an order: first bare word is
# the verb, second is the unit. Getting this wrong would make the
# naive-loop reproduction below pass for the wrong reason.
cmd=""; unit=""; quiet=0
for a in "$@"; do
  case $a in
    --quiet | -q) quiet=1 ;;
    -*) ;;
    *) if [[ -z "$cmd" ]]; then cmd=$a; elif [[ -z "$unit" ]]; then unit=$a; fi ;;
  esac
done
d="$root/$unit"
if [[ ! -d "$d" ]]; then
  # An unknown unit: systemd still answers `show` with LoadState=not-found.
  case $cmd in
    show) echo "LoadState=not-found"; echo "ActiveState=inactive"; exit 0 ;;
    is-active) echo "inactive"; exit 3 ;;
    *) exit 1 ;;
  esac
fi
i=$(cat "$d/cursor")
n=$(wc -l < "$d/steps")
if [[ "$i" -gt "$n" ]]; then i=$n; fi
line=$(sed -n "${i}p" "$d/steps")
if [[ "$i" -lt "$n" ]]; then echo $((i + 1)) > "$d/cursor"; fi
IFS=$'\t' read -r active sub token result status <<<"$line"
case $cmd in
  show)
    remain=$(sed -n 's/^RemainAfterExit=//p' "$d/props")
    {
      cat "$d/props"
      echo "ActiveState=$active"
      echo "SubState=$sub"
      echo "Result=$result"
      echo "ExecMainStatus=$status"
      if [[ "$remain" == yes ]]; then
        echo "ActiveEnterTimestampMonotonic=$token"
        echo "InactiveEnterTimestampMonotonic=0"
      else
        echo "InactiveEnterTimestampMonotonic=$token"
        echo "ActiveEnterTimestampMonotonic=0"
      fi
    }
    exit 0
    ;;
  is-active)
    if [[ "$quiet" -eq 0 ]]; then echo "$active"; fi
    case $active in
      active | reloading) exit 0 ;;
      *) exit 3 ;;
    esac
    ;;
  *) exit 1 ;;
esac
STUB
chmod +x "$TMP/bin/systemctl"
export FAKE_UNITS="$FAKE"

# mkunit <unit> <RemainAfterExit> <step>... — each step is
# "[<repeat>x]<ActiveState>|<SubState>|<token>|<Result>|<ExecMainStatus>".
# A leading "3x" serves that state for three systemctl invocations; the
# LAST step is sticky, so a fixture ending in `activating` models a unit
# that is still running however often it is polled. A fixture with too
# few steps therefore fails loudly rather than passing for a reason it
# did not intend.
mkunit() {
  local unit=$1 remain=$2; shift 2
  local d="$FAKE/$unit"
  rm -rf "$d"; mkdir -p "$d"
  {
    echo "Id=$unit"
    echo "LoadState=loaded"
    echo "Type=oneshot"
    echo "RemainAfterExit=$remain"
    echo "ConditionResult=yes"
  } > "$d/props"
  : > "$d/steps"
  local s n i
  for s in "$@"; do
    n=1
    case $s in
      [0-9]x* | [0-9][0-9]x*) n=${s%%x*}; s=${s#*x} ;;
    esac
    for ((i = 0; i < n; i++)); do printf '%s\n' "${s//|/$'\t'}" >> "$d/steps"; done
  done
  echo 1 > "$d/cursor"
}
rewind() { echo 1 > "$FAKE/$1/cursor"; }

# `source=` resolves the library under `shellcheck -x` (what the
# Makefile runs); SC1091 is the same fact reported as an info note by a
# plain `shellcheck`, which the repo already silences this way in
# node-healthcheck.sh.
# shellcheck source=scripts/ops/ops-verdict.sh disable=SC1091
. "$LIB"
export OPS_VERDICT_POLL=1

# ─── 1. the defect, reproduced ──────────────────────────────────────
echo "ops-verdict-test: the naive is-active loop against a running oneshot"

# r1's creators-rollup mid-run: idle at the PREVIOUS run's token, then
# two polls of `activating`, then this run's completion at a NEW token.
PREV=9403883529041
NEW=9405834232409
mkunit creators-rollup.service no \
  "inactive|dead|$PREV|success|0" \
  "activating|start|$PREV|success|0"

# Take the baseline the way an operator does, then let the unit start.
base=$(oneshot_baseline creators-rollup.service)
res "$(t "$base" = "$PREV"; echo $?)" \
  "oneshot_baseline reads the previous run's completion stamp" "got '$base'"

iterations=0
while systemctl is-active --quiet creators-rollup.service; do
  iterations=$((iterations + 1))
  [[ "$iterations" -gt 5 ]] && break
done
naive_result=$(sed -n 's/^Result=//p' <(systemctl show creators-rollup.service -p Result))
naive_token=$(sed -n 's/^InactiveEnterTimestampMonotonic=//p' \
  <(systemctl show creators-rollup.service -p InactiveEnterTimestampMonotonic))

res "$(t "$iterations" -eq 0; echo $?)" \
  "the naive loop makes ZERO iterations while the unit is activating" \
  "made $iterations"
res "$(t "$naive_result" = success; echo $?)" \
  "…and then reports Result=success" "got '$naive_result'"
res "$(t "$naive_token" = "$PREV"; echo $?)" \
  "…quoting a run that finished BEFORE the baseline was taken (the defect)" \
  "token $naive_token, baseline $PREV"

# ─── 2. wait_for_oneshot on the same fixture ────────────────────────
echo "ops-verdict-test: wait_for_oneshot waits, then proves the run is this one"

# The same unit, now allowed to finish: three polls of `activating` and
# then this run's completion at a new token.
mkunit creators-rollup.service no \
  "inactive|dead|$PREV|success|0" \
  "3x activating|start|$PREV|success|0" \
  "inactive|dead|$NEW|success|0"
out=$(wait_for_oneshot creators-rollup.service 30 "$base" 2>&1); rc=$?
res "$(t "$rc" -eq 0; echo $?)" "a NEW successful run returns 0" "rc=$rc out=$out"
case $out in
  *"verdict=success"*) ok "…and says verdict=success" ;;
  *) bad "…and says verdict=success — got '$out'" ;;
esac
case $out in
  *"$NEW"*) ok "…reporting THIS run's completion stamp, not the previous one" ;;
  *) bad "…reporting THIS run's completion stamp — got '$out'" ;;
esac

# ─── 3. the stale result is REFUSED, not reported ───────────────────
echo "ops-verdict-test: a result that predates the baseline is refused"

# The unit never starts: idle at the same token the baseline holds.
mkunit sponsors-rollup.service no "inactive|dead|$PREV|success|0"
out=$(wait_for_oneshot sponsors-rollup.service 1 "$PREV" 2>&1); rc=$?
res "$(t "$rc" -eq 3; echo $?)" \
  "an unadvanced completion stamp returns 3 (refused), not 0" "rc=$rc out=$out"
case $out in
  *"verdict=stale-result"*) ok "…and names the reason: verdict=stale-result" ;;
  *) bad "…and names the reason — got '$out'" ;;
esac
case $out in
  *success*"result="*) bad "…without quoting Result at all — got '$out'" ;;
  *) ok "…without quoting the previous run's Result" ;;
esac

# A unit that has NEVER run reports Result=success by default — measured
# on r1's apport-autoreport.service. Baseline and token are both 0, so
# the stamp cannot advance and the helper refuses.
mkunit apport-autoreport.service no "inactive|dead|0|success|0"
out=$(wait_for_oneshot apport-autoreport.service 1 0 2>&1); rc=$?
res "$(t "$rc" -eq 3; echo $?)" \
  "a unit that never ran (Result=success by default) is refused" "rc=$rc out=$out"

# ─── 4. failure, timeout, load state ────────────────────────────────
echo "ops-verdict-test: the other verdicts each have their own code"

mkunit failing-rollup.service no \
  "activating|start|$PREV|success|0" \
  "failed|failed|$NEW|exit-code|2"
out=$(wait_for_oneshot failing-rollup.service 30 "$PREV" 2>&1); rc=$?
res "$(t "$rc" -eq 1; echo $?)" "a NEW failed run returns 1" "rc=$rc out=$out"
case $out in
  *"result=exit-code"*"exec_status=2"*) ok "…carrying Result and ExecMainStatus" ;;
  *) bad "…carrying Result and ExecMainStatus — got '$out'" ;;
esac

mkunit stuck-rollup.service no "activating|start|$PREV|success|0"
out=$(wait_for_oneshot stuck-rollup.service 1 "$PREV" 2>&1); rc=$?
res "$(t "$rc" -eq 2; echo $?)" "a unit still activating at the timeout returns 2" "rc=$rc out=$out"

out=$(wait_for_oneshot no-such-unit.service 1 0 2>&1); rc=$?
res "$(t "$rc" -eq 4; echo $?)" "an unloaded unit returns 4, not a verdict" "rc=$rc out=$out"

out=$(wait_for_oneshot 2>&1); rc=$?
res "$(t "$rc" -eq 4; echo $?)" "no unit name returns 4" "rc=$rc"

out=$(wait_for_oneshot creators-rollup.service notanumber "$PREV" 2>&1); rc=$?
res "$(t "$rc" -eq 4; echo $?)" "a non-numeric timeout returns 4 rather than looping" "rc=$rc"

# No baseline argument at all: one is taken on entry, so a unit that had
# ALREADY finished cannot advance past it and is refused (3) instead of
# reporting the run the caller missed.
mkunit finished-rollup.service no "inactive|dead|$NEW|success|0"
out=$(wait_for_oneshot finished-rollup.service 1 2>&1); rc=$?
res "$(t "$rc" -eq 3; echo $?)" \
  "an omitted baseline fails closed on an already-finished run" "rc=$rc out=$out"

# ─── 5. RemainAfterExit=yes ends `active`, not `inactive` ───────────
echo "ops-verdict-test: a RemainAfterExit=yes oneshot is waited on correctly"

# 34 of r1's services are Type=oneshot and many (apparmor, disable-thp,
# blk-availability…) keep RemainAfterExit=yes: they finish `active` with
# a populated ActiveEnterTimestamp. Waiting for `inactive` on one of
# those hangs until the timeout, which is how a correct helper gets
# deleted by the next operator.
mkunit apparmor.service yes \
  "inactive|dead|0|success|0" \
  "activating|start|0|success|0" \
  "active|exited|$NEW|success|0"
base=$(oneshot_baseline apparmor.service)
res "$(t "$base" = 0; echo $?)" \
  "oneshot_baseline reads ActiveEnterTimestamp for RemainAfterExit=yes" "got '$base'"
out=$(wait_for_oneshot apparmor.service 30 "$base" 2>&1); rc=$?
res "$(t "$rc" -eq 0; echo $?)" \
  "…and the wait ends on the active state rather than timing out" "rc=$rc out=$out"

# ─── 6. gate_passed ─────────────────────────────────────────────────
echo "ops-verdict-test: gate_passed reads the sentinel, never an exit code"

printf '=== Go build ===\nok\n%s\n' "$GATE_SENTINEL_VERIFY" > "$TMP/verify-ok.log"
printf '=== Go build ===\nok\nVERIFY INCOMPLETE: 1 check(s) deferred\n' > "$TMP/verify-deferred.log"
printf 'prepush: %s profile=native integration=run\n' "$GATE_SENTINEL_PREPUSH" > "$TMP/prepush.log"
printf 'verify-container: %s\n' "$GATE_SENTINEL_CONTAINER" > "$TMP/container.log"
: > "$TMP/empty.log"
{ cat "$TMP/verify-ok.log"; printf 'site-crawl-check: %s\n' "$GATE_SENTINEL_VERIFY"; } > "$TMP/combined.log"

gate_passed "$TMP/verify-ok.log" "$GATE_SENTINEL_VERIFY" >/dev/null 2>&1
res $? "verify.sh's own sentinel passes"

gate_passed "$TMP/verify-deferred.log" "$GATE_SENTINEL_VERIFY" >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 1; echo $?)" \
  "a deferred verify.sh run fails even though it printed plenty of output" "rc=$rc"

gate_passed "$TMP/nope.log" "$GATE_SENTINEL_VERIFY" >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 2; echo $?)" "a missing log is a failure, not a pass" "rc=$rc"

gate_passed "$TMP/empty.log" "$GATE_SENTINEL_VERIFY" >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 2; echo $?)" "an empty log is a failure, not a pass" "rc=$rc"

gate_passed "$TMP/combined.log" "$GATE_SENTINEL_VERIFY" >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 3; echo $?)" \
  "two scripts emitting the same sentinel is surfaced, not accepted" "rc=$rc"

gate_passed "$TMP/prepush.log" "$GATE_SENTINEL_PREPUSH" >/dev/null 2>&1
res $? "prepush's real line (prefixed + suffixed) matches its sentinel"

gate_passed "$TMP/container.log" "$GATE_SENTINEL_CONTAINER" >/dev/null 2>&1
res $? "verify-container's real line matches its sentinel"

gate_passed "$TMP/prepush.log" "$GATE_SENTINEL_VERIFY" >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 1; echo $?)" \
  "a prepush log does NOT satisfy verify.sh's sentinel" "rc=$rc"

# ─── 7. an operation that matched nothing ───────────────────────────
echo "ops-verdict-test: silence is the failure mode"

require_output "disarm" 1 -- true >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 1; echo $?)" "a command that exits 0 with NO output fails" "rc=$rc"

require_output "disarm" 1 -- printf '   \n\t\n' >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 1; echo $?)" "whitespace-only output is not a row" "rc=$rc"

require_output "disarm" 3 -- printf '1041\n1042\n1043\n' >/dev/null 2>&1
res $? "three returned job ids clear a minimum of three"

require_output "disarm" 4 -- printf '1041\n1042\n1043\n' >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 1; echo $?)" "…and fail a minimum of four" "rc=$rc"

out=$(require_output "disarm" 1 -- printf '1041\n' 2>&1)
case $out in
  *"1 row(s) of a required minimum of 1"*) ok "a self-accounting line names N of M" ;;
  *) bad "a self-accounting line names N of M — got '$out'" ;;
esac

require_output "disarm" 1 -- false >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 2; echo $?)" "a failing command still fails (no weakening)" "rc=$rc"

require_affected "alter_job" 1 -- printf 'SELECT 0\n' >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 1; echo $?)" "psql's 'SELECT 0' tag fails a minimum of one" "rc=$rc"

require_affected "alter_job" 1 -- printf 'UPDATE 3\n' >/dev/null 2>&1
res $? "'UPDATE 3' clears a minimum of one"

require_affected "seed" 7 -- printf 'INSERT 0 7\n' >/dev/null 2>&1
res $? "'INSERT 0 7' reads 7, not 0"

require_affected "alter_job" 1 -- printf 'job_id\n1041\n' >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 3; echo $?)" \
  "output with no command tag proves nothing and returns 3" "rc=$rc"

require_affected "alter_job" 1 -- false >/dev/null 2>&1; rc=$?
res "$(t "$rc" -eq 2; echo $?)" "a failing psql still fails" "rc=$rc"

echo "ops-verdict-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
