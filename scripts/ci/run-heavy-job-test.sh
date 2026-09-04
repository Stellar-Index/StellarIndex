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
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/flock"   # macOS has no flock
chmod +x "$TMP/bin/flock"
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
  env "$@" "$WRAP" test-job "$PAYLOAD" >"$TMP/out" 2>"$TMP/err"
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

}
mkdir -p "$TMP/userbin"; printf '#!/usr/bin/env bash\necho 1000\n' > "$TMP/userbin/id"; chmod +x "$TMP/userbin/id"
PATH="$TMP/userbin:$PATH" run_cases "non-root"
mkdir -p "$TMP/rootbin"; printf '#!/usr/bin/env bash\necho 0\n' > "$TMP/rootbin/id"; chmod +x "$TMP/rootbin/id"
PATH="$TMP/rootbin:$PATH" run_cases "root (id stubbed)"

echo "run-heavy-job-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
