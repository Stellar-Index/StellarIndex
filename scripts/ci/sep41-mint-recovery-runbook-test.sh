#!/usr/bin/env bash
# sep41-mint-recovery-runbook-test.sh — proves every
# `run-heavy-job.sh <name> …` invocation in
# docs/operations/sep41-mint-recovery.md uses the SAME job-label NAME
# (K016).
#
# The wrapper's contract (configs/ansible/roles/archival-node/tasks/
# 14-stellarindex-services.yml, the /usr/local/sbin/run-heavy-job.sh
# copy task) derives its singleton lock straight from NAME:
# `LOCK="${HEAVY_JOB_LOCK_DIR:-/run/lock}/stellarindex-heavy-${NAME}.lock"`.
# This runbook's own Preconditions section requires "One heavy job at a
# time" — but the step-2 re-derive used `sep41-mint-recover` while the
# step-3 "reset line was missing, re-run" fallback used the DIFFERENT
# label `sep41-recover`. Two labels for the same recovery procedure
# means two different lock files: the wrapper never actually serializes
# a step-2 run against the step-3 fallback (or a retried step-2 against
# itself under the other spelling), silently defeating the "one heavy
# job at a time" guarantee an operator relies on mid-incident.
#
# This test extracts the real `sh` code blocks from the shipped runbook
# text (not a hand-copied twin) and asserts every `run-heavy-job.sh`
# invocation's NAME argument is identical.
#
# Run: bash scripts/ci/sep41-mint-recovery-runbook-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
RUNBOOK="$PWD/docs/operations/sep41-mint-recovery.md"
[[ -r "$RUNBOOK" ]] || { echo "sep41-mint-recovery-runbook-test: missing $RUNBOOK" >&2; exit 2; }

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

echo "sep41-mint-recovery-runbook-test:"

NAMES="$(python3 - "$RUNBOOK" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
blocks = re.findall(r"```sh\n(.*?)```", text, re.S)
names = []
for b in blocks:
    names.extend(re.findall(r"run-heavy-job\.sh\s+(\S+)", b))
print("\n".join(names))
PY
)"

count="$(printf '%s\n' "$NAMES" | grep -c . || true)"
if [ "$count" -ge 2 ]; then
  ok "found $count run-heavy-job.sh invocation(s) in the runbook"
else
  bad "expected at least 2 run-heavy-job.sh invocations, found $count"
fi

unique="$(printf '%s\n' "$NAMES" | sort -u | grep -c . || true)"
if [ "$unique" -eq 1 ]; then
  ok "all run-heavy-job.sh invocations share one job-label NAME ($(printf '%s\n' "$NAMES" | sort -u | tr '\n' ' '))"
else
  bad "run-heavy-job.sh invocations use $unique DIFFERENT job-label NAMEs ($(printf '%s\n' "$NAMES" | sort -u | tr '\n' ' ')) — different names hash to different singleton-lock files, defeating the runbook's 'one heavy job at a time' precondition"
fi

echo "sep41-mint-recovery-runbook-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
