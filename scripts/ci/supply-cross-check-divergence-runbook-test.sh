#!/usr/bin/env bash
# supply-cross-check-divergence-runbook-test.sh — proves the
# mandatory re-seed commands in
# docs/operations/runbooks/supply-cross-check-divergence.md actually
# EXECUTE against the shipped run-heavy-job.sh wrapper (F155).
#
# The wrapper's contract (configs/ansible/roles/archival-node/tasks/
# 14-stellarindex-services.yml, the /usr/local/sbin/run-heavy-job.sh
# copy task) is `run-heavy-job.sh <name> <command...>`: NAME is
# consumed as a job label for the singleton lock, and everything
# after it is exec'd as the payload command. A runbook command that
# passes the ops binary name (`stellarindex-ops`) as NAME instead of
# a job label pushes the binary's first subcommand word (`supply`)
# into the exec position — `run-heavy-job: line N: exec: supply: not
# found` (exit 127), exactly as F155 describes, under time pressure
# on a supply-correctness incident.
#
# This test extracts BOTH the real wrapper (from the ansible task,
# same technique as scripts/ci/run-heavy-job-test.sh) and the real
# `sh` code block from the runbook (so the property under test is the
# shipped doc text, not a hand-copied twin), stubs `stellarindex-ops`
# on PATH, and runs the extracted commands verbatim through the
# wrapper's non-root exec path (flock stubbed so this runs on macOS).
#
# Run: bash scripts/ci/supply-cross-check-divergence-runbook-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="$PWD/configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml"
RUNBOOK="$PWD/docs/operations/runbooks/supply-cross-check-divergence.md"
[[ -r "$TASKS" ]] || { echo "supply-cross-check-divergence-runbook-test: missing $TASKS" >&2; exit 2; }
[[ -r "$RUNBOOK" ]] || { echo "supply-cross-check-divergence-runbook-test: missing $RUNBOOK" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── extract the shipped wrapper from the ansible task ──────────────
python3 - "$TASKS" "$TMP/run-heavy-job.sh" <<'PY' || { echo "supply-cross-check-divergence-runbook-test: could not extract wrapper" >&2; exit 2; }
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

# ─── extract the runbook's mandatory re-seed `sh` code block ────────
python3 - "$RUNBOOK" "$TMP/reseed.sh" <<'PY' || { echo "supply-cross-check-divergence-runbook-test: could not extract runbook block" >&2; exit 2; }
import re, sys
text = open(sys.argv[1]).read()
blocks = re.findall(r"```sh\n(.*?)```", text, re.S)
target = [b for b in blocks if "seed-sac-balances" in b and "-full-history" in b]
if len(target) != 1:
    sys.exit(1)
open(sys.argv[2], "w").write(target[0])
PY
[[ -s "$TMP/reseed.sh" ]] || { echo "supply-cross-check-divergence-runbook-test: empty extracted block" >&2; exit 2; }

mkdir -p "$TMP/bin" "$TMP/lock"
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/flock"   # macOS has no flock
chmod +x "$TMP/bin/flock"
printf '#!/usr/bin/env bash\necho 1000\n' > "$TMP/bin/id"   # non-root branch
chmod +x "$TMP/bin/id"

# Stub the ops binary the runbook expects to be exec'd. Records its
# full argv so the test can assert the wrapper actually reached it —
# not "supply" or any other mis-parsed token.
INVOKED="$TMP/invoked.txt"
cat > "$TMP/bin/stellarindex-ops" <<SH
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$INVOKED"
exit 0
SH
chmod +x "$TMP/bin/stellarindex-ops"

# The runbook's code block invokes the bare `run-heavy-job.sh` name,
# exactly as an operator on r1 (where it lives on PATH at
# /usr/local/sbin) would paste it. Put our extracted copy of the real
# wrapper on PATH under that same bare name — nothing in the doc text
# under test is rewritten.
WRAP_DIR="$(dirname "$WRAP")"
export PATH="$WRAP_DIR:$TMP/bin:$PATH"
export HEAVY_JOB_LOCK_DIR="$TMP/lock"

echo "supply-cross-check-divergence-runbook-test:"

# The extracted block is two `run-heavy-job.sh ...` invocations
# (dry-run, then write) joined by line-continuations. Run them exactly
# as an operator would paste them into a shell.
: > "$INVOKED"
bash -c "
set -e
$(cat "$TMP/reseed.sh")
" > "$TMP/out" 2>"$TMP/err"
rc=$?

if [ "$rc" -eq 0 ]; then
  ok "both re-seed commands in the runbook exit 0 through the real wrapper"
else
  bad "re-seed commands failed (rc=$rc); stderr: $(cat "$TMP/err")"
fi

if grep -q "exec: supply: not found" "$TMP/err"; then
  bad "wrapper tried to exec 'supply' as a binary — NAME argument still swallows stellarindex-ops (F155 regression)"
else
  ok "wrapper never tried to exec 'supply' as a binary"
fi

invoked_count="$(wc -l < "$INVOKED" | tr -d ' ')"
if [ "$invoked_count" = "2" ]; then
  ok "stellarindex-ops was actually exec'd twice (dry-run, then write)"
else
  bad "expected 2 stellarindex-ops invocations, got $invoked_count ($(cat "$INVOKED"))"
fi

if grep -q "^supply seed-sac-balances -config /etc/stellarindex.toml -full-history -dry-run$" "$INVOKED" \
   && grep -q "^supply seed-sac-balances -config /etc/stellarindex.toml -full-history -write$" "$INVOKED"; then
  ok "stellarindex-ops received the correct 'supply seed-sac-balances' subcommand and flags"
else
  bad "stellarindex-ops did not receive the expected argv ($(cat "$INVOKED"))"
fi

echo "supply-cross-check-divergence-runbook-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
