#!/usr/bin/env bash
# run-heavy-job-test.sh — fixture tests for the heavy-job wrapper's
# ClickHouse ops-batch identity import (2026-08-28 r1).
#
# The wrapper lives INSIDE ansible
# (configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml,
# the /usr/local/sbin/run-heavy-job.sh copy task); this extracts that
# exact content and runs it, so the property under test is the shipped
# script, not a hand-copied twin.
#
# What must hold — each was the silent-no-op the profile shipped with
# the first time round:
#
#   1. a job launched from a shell that did NOT source
#      /etc/default/stellarindex-ops (every runbook sources
#      /etc/default/stellarindex instead) still receives
#      STELLARINDEX_CLICKHOUSE_OPS_USER/_PASSWORD, read VERBATIM from
#      the -ops file (passwords contain `=`, `$`, quotes, spaces);
#   2. ONLY that pair is imported — the caller's AWS_*/DSN choice must
#      not be overridden by the -ops file's;
#   3. a value the caller already exported wins;
#   4. with no pair available the wrapper WARNS on stderr that the job
#      runs as CH `default` at serving priority — never quietly;
#   5. the root branch's scope carries TimeoutStopSec — 5min by default,
#      HEAVY_JOB_STOP_TIMEOUT when the caller sets one — so a
#      `systemctl stop` does not SIGKILL a job mid-cleanup at systemd's
#      90 s default, and the four resource properties are still there.
#      The non-root branch creates no scope at all.
#   6. HEAVY_JOB_STOP_TIMEOUT is VALIDATED before the scope is created.
#      A bare integer is SECONDS under systemd.time, so `2` from an
#      operator meaning two hours is a two-second grace — accepted by
#      systemd, and harder on the job than setting nothing. The wrapper
#      takes a bare integer, Ns, Nmin, Nh and `infinity`, and refuses
#      every other spelling and anything under the 90 s floor (the
#      systemd default this bound replaces), exiting 2 without running
#      the payload.
#   7. a held lock is a REFUSAL (exit 75, payload not run) for every
#      caller, never 0 — a manual caller cannot tell an exit-0 skip
#      from a finished run, and neither can a unit's overlapping fire
#      ($INVOCATION_ID set).
#   8. every systemd unit that ExecStarts the wrapper declares
#      SuccessExitStatus=75, so that skip is not a unit failure.
#   9. the disk watchdog does not hold the lock fd: an orphaned
#      watchdog `sleep` held it past the job's exit, so a prompt
#      relaunch of a failed job found it "held" and was skipped.
#   10. no operator-facing text tells operators to use a UNIQUE job name
#      per attempt: that is what defeated the per-name lock.
#   12. "one heavy job at a time" holds ACROSS job names. An operator
#      launch (HEAVY_JOB_CLASS unset) is refused with exit 75, payload
#      not run, while any other heavy job holds the host-wide lock; a
#      scheduled launch is never refused (it warns beside an operator
#      job); a released lock is free at once; a lock file the caller
#      cannot write still locks (it is opened read-only, never O_CREAT
#      on an existing file: fs.protected_regular refuses that across
#      users in /run/lock); a lock file that cannot be opened at all
#      refuses an operator job and only warns a scheduled one; and every
#      timer/cron launcher declares HEAVY_JOB_CLASS=scheduled. These
#      cases use a real flock(2) shim, not the always-succeeds stub.
#   12. a TERM to the root-branch wrapper stops its sibling scope, so no
#      payload outlives a stopped/timed-out unit holding the lock.
#   13. HEAVY_JOB_MEMORY_MAX reaches the scope (validated), and no root
#      wrapper unit sets a MemoryMax= that could only cap the wrapper.
#
# Runs the wrapper's non-root exec path (no systemd-run / flock needed:
# flock is stubbed on PATH so this runs on macOS too).
#
# Run: bash scripts/ci/run-heavy-job-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="$PWD/configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml"
[[ -r "$TASKS" ]] || { echo "run-heavy-job-test: missing $TASKS" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── extract the shipped wrapper from the ansible task ──────────────
python3 - "$TASKS" "$TMP/run-heavy-job.sh" <<'PY' || { echo "run-heavy-job-test: could not extract wrapper" >&2; exit 2; }
import sys, yaml
tasks = yaml.safe_load(open(sys.argv[1]))
for t in tasks:
    c = t.get("ansible.builtin.copy") or {}
    if c.get("dest") == "/usr/local/sbin/run-heavy-job.sh":
        open(sys.argv[2], "w").write(c["content"])
        sys.exit(0)
sys.exit(1)
PY
chmod +x "$TMP/run-heavy-job.sh"
WRAP="$TMP/run-heavy-job.sh"

mkdir -p "$TMP/bin" "$TMP/lock"
# macOS has no flock; FLOCK_HELD answers "held" (case 7); FLOCK_REC
# records the arguments, i.e. skip (-n) or queue (-w N) (case 7b).
cat > "$TMP/bin/flock" <<'FL'
#!/usr/bin/env bash
[ -n "${FLOCK_REC:-}" ] && printf '%s\n' "$*" >> "$FLOCK_REC"
[ -n "${FLOCK_HELD:-}" ] && exit 1
exit 0
FL
chmod +x "$TMP/bin/flock"
# The watchdog's `sleep 30` is the one sleep the wrapper runs: record
# whether it inherited fd 9 (the lock) into $FD9_REC, then fail so the
# watchdog loop ends at once (case 8).
cat > "$TMP/bin/sleep" <<'SL'
#!/usr/bin/env bash
if [ -n "${FD9_REC:-}" ]; then
  for fd in 9 8; do
    if [ -e "/dev/fd/$fd" ]; then echo "fd$fd=open"; else echo "fd$fd=closed"; fi
  done > "$FD9_REC"
fi
exit 1
SL
chmod +x "$TMP/bin/sleep"
# The wrapper branches on `id -u`: non-root execs the payload directly,
# root wraps it in `systemd-run --scope --unit U -p K=V … CMD`. CI runners
# and dev machines differ in uid, so a test keyed on the real uid exercised only one
# branch — and the container verifier, which runs as root, found the second
# calling a binary the image does not have. Stub systemd-run faithfully
# (drop its own options, exec the command, env inherited as a scope does)
# and record every -p/--property it was handed, one K=V per line, into
# $SYSTEMD_RUN_PROPS when the caller names a file — the scope's
# properties are otherwise unobservable on a machine with no systemd,
# which is how the missing TimeoutStopSec went unnoticed. Then run the
# whole suite twice, with `id` stubbed to answer a non-zero uid and then
# 0, so both branches are proven on every machine. Neither pass may
# depend on the runner's real uid: a dev machine is non-root and the
# container verifier is root, so a pass keyed on the real uid silently
# runs one branch twice and leaves the other untested.
cat > "$TMP/bin/systemd-run" <<'SR'
#!/usr/bin/env bash
PROPS="${SYSTEMD_RUN_PROPS:-/dev/null}"
: > "$PROPS"
while [ $# -gt 0 ]; do
  case "$1" in
    --scope) shift ;;
    -p|--property) printf '%s\n' "$2" >> "$PROPS"; shift 2 ;;
    -p?*) printf '%s\n' "${1#-p}" >> "$PROPS"; shift ;;
    --property=*) printf '%s\n' "${1#--property=}" >> "$PROPS"; shift ;;
    --unit) shift 2 ;;
    --unit=*) shift ;;
    *) break ;;
  esac
done
exec "$@"
SR
chmod +x "$TMP/bin/systemd-run"
export PATH="$TMP/bin:$PATH"
export HEAVY_JOB_LOCK_DIR="$TMP/lock"

# The payload prints exactly what the wrapped job would see.
PAYLOAD="$TMP/payload.sh"
cat > "$PAYLOAD" <<'SH'
#!/usr/bin/env bash
printf 'USER=%s\n' "${STELLARINDEX_CLICKHOUSE_OPS_USER-<unset>}"
printf 'PASS=%s\n' "${STELLARINDEX_CLICKHOUSE_OPS_PASSWORD-<unset>}"
printf 'AWS=%s\n'  "${AWS_ACCESS_KEY_ID-<unset>}"
# Case 8: stay alive until the watchdog has run its first sleep.
if [ -n "${FD9_REC:-}" ]; then
  for _ in $(seq 50); do [ -s "$FD9_REC" ] && break; /bin/sleep 0.1; done
fi
SH
chmod +x "$PAYLOAD"

# A realistic /etc/default/stellarindex-ops: comments, other secrets,
# and a password with every character class a shell would mangle.
PASSWORD="p=a\$s\"s w0rd'#x="
OPS_ENV="$TMP/stellarindex-ops"
cat > "$OPS_ENV" <<EOT
# Rendered by Ansible. Do not edit by hand.
AWS_ACCESS_KEY_ID=ops-reader-key
AWS_SECRET_ACCESS_KEY=ops-reader-secret
STELLARINDEX_POSTGRES_DSN=postgres://x:y@127.0.0.1:5432/z
STELLARINDEX_CLICKHOUSE_OPS_USER=ops_batch
STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=$PASSWORD
EOT

# Where the systemd-run stub records the scope's -p properties (case 5).
PROPS="$TMP/props"

rc=0
run() { # run <extra env assignments...> — runs the wrapper, captures out/err/rc
  env -u INVOCATION_ID "$@" "$WRAP" test-job "$PAYLOAD" >"$TMP/out" 2>"$TMP/err"
  rc=$?
}
out_has() { grep -qxF -- "$1" "$TMP/out"; }
err_has() { grep -qF -- "$1" "$TMP/err"; }

echo "run-heavy-job-test:"
run_cases() {
local branch="$1"; printf '  [%s branch]\n' "$branch"

# ── 1. runbook-shaped launch: caller sourced the SERVICE env, not -ops ─
run HEAVY_JOB_OPS_ENV="$OPS_ENV" AWS_ACCESS_KEY_ID=service-key
if out_has "USER=ops_batch"; then ok "pair imported: user"; else bad "pair imported: user ($(cat "$TMP/out"))"; fi
if out_has "PASS=$PASSWORD"; then ok "pair imported: password verbatim (=, \$, quotes, spaces, #)"; else bad "password verbatim ($(grep PASS= "$TMP/out"))"; fi
# ── 2. only the pair — the caller's own AWS identity survives ─────────
if out_has "AWS=service-key"; then ok "only the pair is imported (caller's AWS_* kept)"; else bad "caller's AWS_* overridden ($(grep AWS= "$TMP/out"))"; fi
if err_has "ClickHouse identity: ops_batch"; then ok "stderr names the identity"; else bad "stderr identity line missing ($(cat "$TMP/err"))"; fi
if ! err_has "CH 'default' user"; then ok "no identity WARNING when the pair is present"; else bad "spurious identity WARNING"; fi

# ── 3. caller's explicit value wins ──────────────────────────────────
run HEAVY_JOB_OPS_ENV="$OPS_ENV" STELLARINDEX_CLICKHOUSE_OPS_USER=custom STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=custom-pw
if out_has "USER=custom" && out_has "PASS=custom-pw"; then ok "caller-set pair wins over the file"; else bad "caller-set pair overridden ($(cat "$TMP/out"))"; fi

# ── 4. no pair anywhere → loud, not silent ───────────────────────────
run HEAVY_JOB_OPS_ENV="$TMP/does-not-exist"
if out_has "USER=<unset>"; then ok "missing file: job runs with no identity (pre-fix behaviour)"; else bad "missing file: unexpected identity ($(cat "$TMP/out"))"; fi
if err_has "WARNING" && err_has "CH 'default' user at SERVING priority"; then ok "missing file: WARNING on stderr"; else bad "missing file: no WARNING ($(cat "$TMP/err"))"; fi

printf 'AWS_ACCESS_KEY_ID=ops-reader-key\n' > "$TMP/stellarindex-ops-nopair"
run HEAVY_JOB_OPS_ENV="$TMP/stellarindex-ops-nopair"
if out_has "USER=<unset>" && err_has "CH 'default' user"; then ok "file without the pair (profile not applied): WARNING on stderr"; else bad "file without pair: no WARNING ($(cat "$TMP/err"))"; fi

# ── 5. the scope's stop bound (root branch only) ─────────────────────
# Without TimeoutStopSec a scope takes systemd's 90 s default, and
# `systemctl stop` — the wrapper's own disk watchdog, or an operator —
# SIGKILLs a job mid-cleanup: usd-volume-restamp -chunks re-compresses a
# 160 GB chunk on SIGTERM and cannot finish inside 90 s. The DEFAULT is
# short on purpose (the other payloads die on SIGTERM at once); the
# restamp exports the long value on its own launch line.
: > "$PROPS"
run HEAVY_JOB_OPS_ENV="$OPS_ENV" SYSTEMD_RUN_PROPS="$PROPS"
if [ "$branch" = "non-root" ]; then
  if [ ! -s "$PROPS" ]; then ok "non-root: execs directly, so no scope and no properties"; else bad "non-root branch created a scope ($(tr '\n' ' ' < "$PROPS"))"; fi
  # The bound is inert without a scope, so the non-root branch neither
  # applies nor validates it — a job must not be refused over a setting
  # that could not have taken effect.
  run HEAVY_JOB_OPS_ENV="$OPS_ENV" HEAVY_JOB_STOP_TIMEOUT=2
  if [ "$rc" -eq 0 ] && out_has "USER=ops_batch"; then ok "non-root: an out-of-range bound is inert (no scope to carry it), the job still runs"; else bad "non-root refused over an inert bound (rc=$rc)"; fi
else
  if grep -qx 'TimeoutStopSec=5min' "$PROPS"; then ok "scope carries TimeoutStopSec=5min by default"; else bad "scope has no TimeoutStopSec=5min — a stop SIGKILLs at systemd's 90 s default (props: $(tr '\n' ' ' < "$PROPS"))"; fi
  for p in MemoryMax=20G MemorySwapMax=0 CPUWeight=50 IOWeight=50; do
    if grep -qx "$p" "$PROPS"; then ok "scope still carries $p"; else bad "scope lost $p (props: $(tr '\n' ' ' < "$PROPS"))"; fi
  done
  run HEAVY_JOB_OPS_ENV="$OPS_ENV" SYSTEMD_RUN_PROPS="$PROPS" HEAVY_JOB_STOP_TIMEOUT=2h
  if grep -qx 'TimeoutStopSec=2h' "$PROPS" && ! grep -qx 'TimeoutStopSec=5min' "$PROPS"; then ok "HEAVY_JOB_STOP_TIMEOUT=2h (the restamp's launch line) overrides the default"; else bad "HEAVY_JOB_STOP_TIMEOUT not honoured (props: $(tr '\n' ' ' < "$PROPS"))"; fi

  # ── 6. the bound is validated, not passed through ──────────────────
  # `2` is the one that motivated this: a bare integer is SECONDS under
  # systemd.time, so an operator meaning two hours would have got a
  # two-second grace — accepted by systemd, and a harder kill than
  # setting nothing at all.
  for v in 2 0 -5 abc 90m "2 h" 1min 89 89s; do
    : > "$PROPS"
    run HEAVY_JOB_OPS_ENV="$OPS_ENV" SYSTEMD_RUN_PROPS="$PROPS" "HEAVY_JOB_STOP_TIMEOUT=$v"
    if [ "$rc" -eq 2 ] && [ ! -s "$TMP/out" ] && [ ! -s "$PROPS" ] && err_has "refusing to start test-job"; then
      ok "HEAVY_JOB_STOP_TIMEOUT='$v' refused before the payload runs (exit 2, no scope)"
    else
      bad "HEAVY_JOB_STOP_TIMEOUT='$v' was not refused (rc=$rc, out='$(tr '\n' ' ' < "$TMP/out")', props='$(tr '\n' ' ' < "$PROPS")')"
    fi
  done
  if err_has "below the 90 s floor" && err_has "'2' is two seconds, not two hours"; then ok "the refusal names the 90 s floor and the seconds rule"; else bad "refusal message does not name the floor/unit rule ($(cat "$TMP/err"))"; fi
  run HEAVY_JOB_OPS_ENV="$OPS_ENV" HEAVY_JOB_STOP_TIMEOUT=abc
  if err_has "is not a form this wrapper accepts"; then ok "a non-time value is refused as a FORM, not as a floor breach"; else bad "'abc' refused with the wrong reason ($(cat "$TMP/err"))"; fi

  for v in 90 120s 30min 2h infinity; do
    : > "$PROPS"
    run HEAVY_JOB_OPS_ENV="$OPS_ENV" SYSTEMD_RUN_PROPS="$PROPS" "HEAVY_JOB_STOP_TIMEOUT=$v"
    if [ "$rc" -eq 0 ] && grep -qx "TimeoutStopSec=$v" "$PROPS"; then
      ok "HEAVY_JOB_STOP_TIMEOUT='$v' accepted and passed through verbatim"
    else
      bad "HEAVY_JOB_STOP_TIMEOUT='$v' rejected or mangled (rc=$rc, props='$(tr '\n' ' ' < "$PROPS")')"
    fi
  done
  if err_has "TimeoutStopSec=infinity — a systemctl stop will NEVER escalate to SIGKILL"; then ok "infinity says on stderr that no stop will ever escalate"; else bad "infinity accepted silently ($(cat "$TMP/err"))"; fi
fi

# ── 7. a held lock: exit 75 for every caller, payload not run ────────
run HEAVY_JOB_OPS_ENV="$OPS_ENV" FLOCK_HELD=1
if [ "$rc" -eq 75 ] && [ ! -s "$TMP/out" ] && err_has "refusing to start test-job" && err_has "still alive"; then
  ok "held lock, manual run: refused with exit 75, payload not run"
else
  bad "held lock, manual run not refused (rc=$rc, out='$(tr '\n' ' ' < "$TMP/out")', err='$(tr '\n' ' ' < "$TMP/err")')"
fi
run HEAVY_JOB_OPS_ENV="$OPS_ENV" FLOCK_HELD=1 INVOCATION_ID=0123456789abcdef
if [ "$rc" -eq 75 ] && [ ! -s "$TMP/out" ] && err_has "skipping this fire"; then
  ok "held lock, systemd unit fire: skipped with exit 75, payload not run"
else
  bad "held lock, systemd unit fire not a clean exit-75 skip (rc=$rc, err='$(tr '\n' ' ' < "$TMP/err")')"
fi

# ── 7b. HEAVY_JOB_LOCK_WAIT queues instead of skipping (GH-1229) ─────
run HEAVY_JOB_OPS_ENV="$OPS_ENV" FLOCK_REC="$TMP/flockrec" INVOCATION_ID=0123456789abcdef
if grep -qx -- '-n 9' "$TMP/flockrec"; then ok "no HEAVY_JOB_LOCK_WAIT: the lock is tried once (-n), as before"; else bad "default lock mode changed ($(cat "$TMP/flockrec"))"; fi
run HEAVY_JOB_OPS_ENV="$OPS_ENV" FLOCK_REC="$TMP/flockrec" HEAVY_JOB_LOCK_WAIT=5 INVOCATION_ID=0123456789abcdef
if [ "$rc" -eq 0 ] && grep -qx -- '-w 5 9' "$TMP/flockrec" && out_has "USER=ops_batch"; then
  ok "HEAVY_JOB_LOCK_WAIT=5: the fire waits on the lock (-w 5) and then runs"
else
  bad "HEAVY_JOB_LOCK_WAIT=5 did not queue on the lock (rc=$rc, flock '$(cat "$TMP/flockrec")')"
fi
run HEAVY_JOB_OPS_ENV="$OPS_ENV" FLOCK_HELD=1 HEAVY_JOB_LOCK_WAIT=5 INVOCATION_ID=0123456789abcdef
if [ "$rc" -ne 0 ] && [ "$rc" -ne 75 ] && [ ! -s "$TMP/out" ] && err_has "still held after waiting 5s"; then
  ok "a lock wait that runs out FAILS (rc=$rc), never a success or a skip"
else
  bad "a lock wait that ran out was not a loud failure (rc=$rc, err='$(tr '\n' ' ' < "$TMP/err")')"
fi
for v in abc 0 5s -1; do
  run HEAVY_JOB_OPS_ENV="$OPS_ENV" "HEAVY_JOB_LOCK_WAIT=$v"
  if [ "$rc" -eq 2 ] && [ ! -s "$TMP/out" ]; then ok "HEAVY_JOB_LOCK_WAIT='$v' refused before the payload runs"; else bad "HEAVY_JOB_LOCK_WAIT='$v' not refused (rc=$rc)"; fi
done

# ── 8. every wrapper unit tolerates the skip code ─────────────────────
echo "  [systemd units]"
units=0
while IFS= read -r unit; do
  units=$((units + 1))
  if grep -qx 'SuccessExitStatus=75' "$unit"; then ok "$unit declares SuccessExitStatus=75"; else bad "$unit ExecStarts run-heavy-job.sh without SuccessExitStatus=75 — a lock skip would fail the unit"; fi
  if grep -qx 'Environment=HEAVY_JOB_CLASS=scheduled' "$unit"; then ok "$unit declares HEAVY_JOB_CLASS=scheduled"; else bad "$unit ExecStarts run-heavy-job.sh without Environment=HEAVY_JOB_CLASS=scheduled — the timer would be refused whenever another heavy job runs"; fi
done < <(grep -rlE '^ExecStart=[^ ]*run-heavy-job\.sh ' configs/ansible/roles/archival-node/templates/systemd deploy/systemd)
if [ "$units" -gt 0 ]; then ok "$units wrapper unit(s) checked"; else bad "no unit ExecStarts run-heavy-job.sh — the unit check ran over nothing"; fi

# ── 9. the watchdog does not hold the lock (root branch only) ────────
if [ "$branch" != "non-root" ]; then
  rm -f "$TMP/fd9"
  run HEAVY_JOB_OPS_ENV="$OPS_ENV" FD9_REC="$TMP/fd9"
  if [ "$rc" -eq 0 ] && grep -qx 'fd9=closed' "$TMP/fd9" 2>/dev/null && grep -qx 'fd8=closed' "$TMP/fd9"; then
    ok "the disk watchdog inherits neither lock fd"
  else
    bad "the disk watchdog holds a lock fd, so it outlives the job (rc=$rc, $(tr '\n' ' ' < "$TMP/fd9" 2>/dev/null || echo 'no record'))"
  fi
fi

}

# ── 12. the host-wide lock ───────────────────────────────────────────
# flock(1) over flock(2), so a held lock is really held: the lock lives
# on the wrapper's open file description, which outlives this shim just
# as it outlives util-linux flock. macOS has no flock(1).
mkdir -p "$TMP/lockbin"
cat > "$TMP/lockbin/flock" <<'FL'
#!/usr/bin/env python3
import fcntl, sys
op, nb = fcntl.LOCK_EX, 0
for a in sys.argv[1:-1]:
    if a == "-n": nb = fcntl.LOCK_NB
    elif a == "-s": op = fcntl.LOCK_SH
    elif a == "-x": op = fcntl.LOCK_EX
    else: sys.exit(64)
try:
    fcntl.flock(int(sys.argv[-1]), op | nb)
except BlockingIOError:
    sys.exit(1)
FL
chmod +x "$TMP/lockbin/flock"
HOLD="$TMP/hold.sh"
cat > "$HOLD" <<'SH'
#!/usr/bin/env bash
: > "$1.started"
while [ ! -e "$1.release" ]; do /bin/sleep 0.1; done
SH
chmod +x "$HOLD"
GLOBAL_LOCK_FILE="$TMP/lock/stellarindex-heavy.lock"
REAL_UID="$(id -u)"

HOLDER=""
hold_start() { # hold_start <tag> <name> <class> — a job that runs until released
  rm -f "$TMP/$1.started" "$TMP/$1.release"
  env -u INVOCATION_ID HEAVY_JOB_CLASS="$3" HEAVY_JOB_OPS_ENV="$OPS_ENV" "$WRAP" "$2" "$HOLD" "$TMP/$1" >/dev/null 2>"$TMP/$1.err" &
  HOLDER=$!
  local i=0
  while [ ! -e "$TMP/$1.started" ] && [ "$i" -lt 300 ]; do /bin/sleep 0.1; i=$((i + 1)); done
  [ -e "$TMP/$1.started" ] || bad "holder $2 ($3) never started ($(cat "$TMP/$1.err"))"
}
hold_stop() { : > "$TMP/$1.release"; wait "$HOLDER"; }
lrun() { # lrun <name> <class> [env...] — one launch while whatever is held stays held
  local name="$1" class="$2"; shift 2
  env -u INVOCATION_ID HEAVY_JOB_CLASS="$class" HEAVY_JOB_OPS_ENV="$OPS_ENV" "$@" "$WRAP" "$name" "$PAYLOAD" >"$TMP/out" 2>"$TMP/err"
  rc=$?
}
ran() { out_has "USER=ops_batch"; }
errs() { tr '\n' ' ' < "$TMP/err"; }

run_lock_cases() {
printf '  [%s branch, host-wide lock]\n' "$1"
hold_start h1 op-a exclusive
lrun op-b ""
if [ "$rc" -eq 75 ] && ! ran && err_has "refusing to start op-b: another heavy job holds the host-wide lock"; then ok "operator job refused (exit 75, payload not run) while an operator job of ANOTHER name runs"; else bad "second operator job not refused (rc=$rc, out='$(tr '\n' ' ' < "$TMP/out")', err='$(errs)')"; fi
lrun op-a ""
if [ "$rc" -eq 75 ] && ! ran && err_has "still alive"; then ok "same-name duplicate still hits its per-job lock first"; else bad "same-name duplicate not refused by the per-job lock (rc=$rc, err='$(errs)')"; fi
lrun sched-a scheduled
if [ "$rc" -eq 0 ] && ran && err_has "WARNING sched-a (scheduled) is starting beside an operator heavy job"; then ok "scheduled job runs beside an operator job, with a WARNING"; else bad "scheduled job suppressed or silent beside an operator job (rc=$rc, err='$(errs)')"; fi
hold_stop h1
lrun op-c ""
if [ "$rc" -eq 0 ] && ran; then ok "lock is free the moment the holder exits (the watchdog does not keep it)"; else bad "operator job refused after the holder exited (rc=$rc, err='$(errs)')"; fi

hold_start h2 sched-h scheduled
lrun op-d ""
if [ "$rc" -eq 75 ] && ! ran; then ok "operator job refused while a scheduled job runs"; else bad "operator job started beside a scheduled job (rc=$rc)"; fi
lrun sched-b scheduled
if [ "$rc" -eq 0 ] && ran && ! err_has "WARNING sched-b"; then ok "scheduled jobs share the lock without a warning"; else bad "scheduled job blocked or warned beside another scheduled job (rc=$rc, err='$(errs)')"; fi
hold_stop h2

lrun op-e bogus
if [ "$rc" -eq 2 ] && ! ran && err_has "HEAVY_JOB_CLASS='bogus' is neither"; then ok "unknown HEAVY_JOB_CLASS refused (exit 2, payload not run)"; else bad "unknown HEAVY_JOB_CLASS not refused (rc=$rc)"; fi

# A lock file the caller cannot write (on r1: one root created and the
# stellarindex user opens, or the reverse) must still lock.
chmod 0444 "$GLOBAL_LOCK_FILE"
if [ "$REAL_UID" -eq 0 ]; then
  echo "  note: running as real root, so mode 0444 does not deny the write open the unwritable-lock cases guard against"
fi
lrun op-f ""
if [ "$rc" -eq 0 ] && ran; then ok "a lock file the caller cannot write still admits a job (opened read-only)"; else bad "unwritable lock file broke the launch (rc=$rc, err='$(errs)')"; fi
hold_start h3 op-g exclusive
lrun op-h ""
if [ "$rc" -eq 75 ] && ! ran && err_has "host-wide lock"; then ok "a lock file the caller cannot write still excludes a second operator job"; else bad "unwritable lock file does not exclude (rc=$rc, err='$(errs)')"; fi
hold_stop h3
chmod 0644 "$GLOBAL_LOCK_FILE"

# A host-wide lock that cannot be opened at all (any uid): nothing proves
# the host is free, so an operator job is refused; a timer warns and runs.
lrun op-i "" HEAVY_JOB_LOCK_DIR="$TMP/lock-broken"
if [ "$rc" -eq 2 ] && ! ran && err_has "refusing to start op-i: cannot open the host-wide lock"; then ok "unopenable host-wide lock: operator job refused (exit 2, payload not run)"; else bad "unopenable host-wide lock: operator job not refused (rc=$rc, err='$(errs)')"; fi
lrun sched-i scheduled HEAVY_JOB_LOCK_DIR="$TMP/lock-broken"
if [ "$rc" -eq 0 ] && ran && err_has "WARNING sched-i (scheduled) could not open the host-wide lock"; then ok "unopenable host-wide lock: scheduled job warns and runs"; else bad "unopenable host-wide lock: scheduled job not run with a WARNING (rc=$rc, err='$(errs)')"; fi
}
mkdir -p "$TMP/lock-broken"
ln -s "$TMP/nowhere/stellarindex-heavy.lock" "$TMP/lock-broken/stellarindex-heavy.lock"

mkdir -p "$TMP/heldbin"; printf '#!/usr/bin/env bash\nexit 1\n' > "$TMP/heldbin/flock"; chmod +x "$TMP/heldbin/flock"
mkdir -p "$TMP/userbin"; printf '#!/usr/bin/env bash\necho 1000\n' > "$TMP/userbin/id"; chmod +x "$TMP/userbin/id"
PATH="$TMP/userbin:$PATH" run_cases "non-root"
PATH="$TMP/lockbin:$TMP/userbin:$PATH" run_lock_cases "non-root"
mkdir -p "$TMP/rootbin"; printf '#!/usr/bin/env bash\necho 0\n' > "$TMP/rootbin/id"; chmod +x "$TMP/rootbin/id"
PATH="$TMP/rootbin:$PATH" run_cases "root (id stubbed)"
PATH="$TMP/lockbin:$TMP/rootbin:$PATH" run_lock_cases "root (id stubbed)"

# ── 12. every cron launcher of the wrapper declares itself scheduled ──
echo "  [cron launchers]"
if python3 - "$TASKS" <<'PY'
import re, sys, yaml
found, missing = 0, []
for t in yaml.safe_load(open(sys.argv[1], encoding="utf-8")) or []:
    job = ((t or {}).get("ansible.builtin.cron") or {}).get("job") or ""
    if "run-heavy-job.sh" in job:
        found += 1
        if not re.search(r"HEAVY_JOB_CLASS=scheduled\s+\S*run-heavy-job\.sh", job):
            missing.append(t.get("name"))
print(f"  cron launchers checked: {found}")
sys.exit(1 if found == 0 or missing else 0)
PY
then ok "every cron launcher of the wrapper declares HEAVY_JOB_CLASS=scheduled"; else bad "a cron launcher of the wrapper is not HEAVY_JOB_CLASS=scheduled (or none was found)"; fi

# ── 14. units that SHARE a job name queue, never skip (GH-1229) ──────
# Two units on one lock name are mutually exclusive on purpose, so a skip
# means one of them silently does not run while the other is long (Tier
# B skipped Tier A's whole bootstrap pass). Each must set
# HEAVY_JOB_LOCK_WAIT, and its TimeoutStartSec (whole seconds) must
# outlast the wait, or systemd kills the queued fire first.
echo "  [units sharing a job name]"
shared=0
# deploy/systemd/X.service mirrors templates/systemd/X.service.j2: the
# same unit, so units are counted by basename, not by file.
names="$(grep -rE '^ExecStart=[^ ]*run-heavy-job\.sh ' configs/ansible/roles/archival-node/templates/systemd deploy/systemd |
  sed -E 's|^([^:]*/)?([^/:]+\.service)(\.j2)?:ExecStart=[^ ]+ ([^ ]+).*|\4 \2|' | sort -u | awk '{print $1}' | uniq -d)"
for name in $names; do
  while IFS= read -r unit; do
    shared=$((shared + 1))
    wait_s="$(sed -n 's/^Environment=HEAVY_JOB_LOCK_WAIT=//p' "$unit")"
    tss="$(sed -n 's/^TimeoutStartSec=//p' "$unit")"
    case "$wait_s" in ''|*[!0-9]*) wait_s=0 ;; esac
    case "$tss" in ''|*[!0-9]*) tss=0 ;; esac
    if [ "$wait_s" -gt 0 ] && [ "$tss" -gt "$wait_s" ]; then
      ok "$unit shares '$name': queues ${wait_s}s, TimeoutStartSec=${tss}s outlasts it"
    else
      bad "$unit shares job name '$name' with another unit but would SKIP on a held lock (HEAVY_JOB_LOCK_WAIT='${wait_s}', TimeoutStartSec='${tss}')"
    fi
  done < <(grep -rlE "^ExecStart=[^ ]*run-heavy-job\.sh ${name}( |\$)" configs/ansible/roles/archival-node/templates/systemd deploy/systemd)
done
if [ "$shared" -gt 0 ]; then ok "$shared unit(s) on a shared job name checked"; else bad "no two units share a job name — the check ran over nothing (verify-archive tiers A and B should)"; fi

# ── 12. a stopped wrapper stops its scope (GH-1228) ──────────────────
# The scope is a SIBLING of the calling unit, so a unit stop or its
# TimeoutStartSec signals only the wrapper. Unforwarded, the payload ran
# on holding the lock and every later fire skipped as "already running".
# The systemd-run stub execs the payload as the wrapper's child, so
# killing the wrapper orphans it exactly as a real scope does; the
# systemctl stub stands in for `systemctl stop <unit>.scope`.
echo "  [stop forwarding, root branch]"
mkdir -p "$TMP/stopbin"
cat > "$TMP/stopbin/systemctl" <<'SC'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SYSTEMCTL_REC"
if [ "$1" = stop ] && [ -s "$PAYLOAD_PIDFILE" ]; then kill -TERM "$(cat "$PAYLOAD_PIDFILE")"; fi
exit 0
SC
chmod +x "$TMP/stopbin/systemctl"
cat > "$TMP/long.sh" <<'SH'
#!/usr/bin/env bash
echo "$$" > "$PAYLOAD_PIDFILE"
exec /bin/sleep 30
SH
chmod +x "$TMP/long.sh"
: > "$TMP/sysctl"; rm -f "$TMP/payload.pid"
PATH="$TMP/stopbin:$TMP/rootbin:$PATH" SYSTEMCTL_REC="$TMP/sysctl" PAYLOAD_PIDFILE="$TMP/payload.pid" \
  HEAVY_JOB_OPS_ENV="$OPS_ENV" env -u INVOCATION_ID "$WRAP" sig-job "$TMP/long.sh" >"$TMP/out" 2>"$TMP/err" &
wpid=$!
for _ in $(seq 50); do [ -s "$TMP/payload.pid" ] && break; /bin/sleep 0.1; done
payload_pid="$(cat "$TMP/payload.pid" 2>/dev/null)"
kill -TERM "$wpid"
wait "$wpid"; rc=$?
if grep -qxF "stop heavy-sig-job-${wpid}.scope" "$TMP/sysctl"; then
  ok "TERM to the wrapper runs systemctl stop on its own scope"
else
  bad "TERM to the wrapper did not stop heavy-sig-job-${wpid}.scope (systemctl calls: '$(tr '\n' ' ' < "$TMP/sysctl")')"
fi
if [ -n "$payload_pid" ] && ! kill -0 "$payload_pid" 2>/dev/null; then
  ok "no payload outlives the stopped wrapper (the lock goes with it)"
else
  bad "payload pid '${payload_pid}' outlived the stopped wrapper, holding the lock"
  [ -n "$payload_pid" ] && kill "$payload_pid" 2>/dev/null
fi
if [ "$rc" -ne 0 ] && [ "$rc" -ne 75 ]; then ok "a stopped run exits non-zero ($rc), never as success or skip"; else bad "a stopped run exited $rc"; fi

cat > "$TMP/rc3.sh" <<'SH'
#!/usr/bin/env bash
read -r line; echo "stdin=$line"; exit 3
SH
chmod +x "$TMP/rc3.sh"
printf 'hello\n' | PATH="$TMP/rootbin:$PATH" HEAVY_JOB_OPS_ENV="$OPS_ENV" env -u INVOCATION_ID "$WRAP" rc-job "$TMP/rc3.sh" >"$TMP/out" 2>"$TMP/err"
rc=$?
if [ "$rc" -eq 3 ]; then ok "the payload's own exit status is the wrapper's"; else bad "payload exit 3 came back as $rc"; fi
if out_has "stdin=hello"; then ok "the backgrounded payload still reads the caller's stdin"; else bad "payload lost stdin ($(cat "$TMP/out"))"; fi

# ── 13. the memory cap reaches the payload's scope (GH-1228) ─────────
echo "  [memory cap, root branch]"
: > "$PROPS"
PATH="$TMP/rootbin:$PATH" run HEAVY_JOB_OPS_ENV="$OPS_ENV" SYSTEMD_RUN_PROPS="$PROPS" HEAVY_JOB_MEMORY_MAX=2G
if [ "$rc" -eq 0 ] && grep -qx 'MemoryMax=2G' "$PROPS" && ! grep -qx 'MemoryMax=20G' "$PROPS"; then
  ok "HEAVY_JOB_MEMORY_MAX=2G becomes the scope's MemoryMax"
else
  bad "HEAVY_JOB_MEMORY_MAX=2G not applied to the scope (rc=$rc, props: $(tr '\n' ' ' < "$PROPS"))"
fi
for v in 2 0G 2GB -1G abc G; do
  : > "$PROPS"
  PATH="$TMP/rootbin:$PATH" run HEAVY_JOB_OPS_ENV="$OPS_ENV" SYSTEMD_RUN_PROPS="$PROPS" "HEAVY_JOB_MEMORY_MAX=$v"
  if [ "$rc" -eq 2 ] && [ ! -s "$TMP/out" ] && [ ! -s "$PROPS" ]; then
    ok "HEAVY_JOB_MEMORY_MAX='$v' refused before the payload runs"
  else
    bad "HEAVY_JOB_MEMORY_MAX='$v' not refused (rc=$rc)"
  fi
done
# A root unit's own MemoryMax= binds the wrapper shell, not the payload in
# the sibling scope — it must be expressed as HEAVY_JOB_MEMORY_MAX.
while IFS= read -r unit; do
  grep -qx 'User=root' "$unit" || continue
  if grep -qE '^MemoryMax=' "$unit"; then
    bad "$unit sets MemoryMax= on a root wrapper unit — it caps the wrapper, not the payload; use Environment=HEAVY_JOB_MEMORY_MAX"
  else
    ok "$unit: no unit-level MemoryMax= pretending to cap the payload"
  fi
done < <(grep -rlE '^ExecStart=[^ ]*run-heavy-job\.sh ' configs/ansible/roles/archival-node/templates/systemd deploy/systemd)

# ── 11. NAME must be a job label, not the payload's own binary ───────
# A dropped label ("run-heavy-job.sh stellarindex-ops supply …" instead
# of "run-heavy-job.sh <label> stellarindex-ops supply …") shifts every
# word left, so the wrapper would otherwise try to exec the binary's
# own subcommand ("supply") as the command (F155).
echo "  [NAME validation]"
for bin in stellarindex-ops stellarindex-api stellarindex-migrate stellarindex-aggregator stellarindex-indexer stellarindex-sla-probe; do
  env -u INVOCATION_ID "$WRAP" "$bin" supply seed-sac-balances >"$TMP/out" 2>"$TMP/err"
  rc=$?
  if [ "$rc" -eq 2 ] && [ ! -s "$TMP/out" ] && err_has "is a binary name, not a job label"; then
    ok "NAME='$bin' refused as a job label before the payload runs"
  else
    bad "NAME='$bin' was not refused (rc=$rc, out='$(tr '\n' ' ' < "$TMP/out")', err='$(tr '\n' ' ' < "$TMP/err")')"
  fi
done

# ── 10. nothing tells an operator to pick a per-attempt job name ─────
echo "  [operator-facing text]"
# git grep: tracked files only, so git-ignored local scratch never trips it.
if hits=$(git grep -nIiE 'unique (job )?name per attempt|with a unique job name|run-heavy-job\.sh [^ ]*-try[0-9<]' \
    -- docs cmd internal deploy configs); then
  bad "per-attempt job names defeat the per-name lock; use ONE name per job: $hits"
else
  ok "no runbook or help text prescribes a unique job name per attempt"
fi

echo "run-heavy-job-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
