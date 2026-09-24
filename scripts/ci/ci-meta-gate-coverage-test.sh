#!/usr/bin/env bash
# ci-meta-gate-coverage-test.sh — structural pins for two of the
# meta-gates that keep the other gates honest (GH-778).
#
# 1. The Tier D fork-detection cron test
#    (scripts/ci/verify-archive-tier-d-test.sh) asserts on
#    internal/ops/archive/verify_archive.go's flags, but its ci.yml step
#    used to run only `if: needs.preflight.outputs.ansible == 'true'` —
#    a Go-only diff that renames one of those flags set `ansible=false`
#    and skipped the only test that reads it.
# 2. ci-health.yml's main-CI-red tripwire watched `ci.yml` only
#    (check-main-ci-health.sh's CI_WORKFLOW_FILE default), while
#    api-audit.yml and commit-identity.yml also run on push to main
#    with no tripwire of their own.
#
# Structural: parses the real workflow files, no network, no gh.
#
# Run: bash scripts/ci/ci-meta-gate-coverage-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CI_YML="${CI_YML:-.github/workflows/ci.yml}"
CI_HEALTH_YML="${CI_HEALTH_YML:-.github/workflows/ci-health.yml}"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ── 1. Tier D cron test step is gated on ansible OR go ─────────────────
tier_d_if="$(python3 - "$CI_YML" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
for job in wf["jobs"].values():
    for step in job.get("steps", []):
        run = step.get("run", "")
        if isinstance(run, str) and "verify-archive-tier-d-test.sh" in run:
            print(step.get("if", ""))
            sys.exit(0)
sys.exit(1)
PY
)" || { echo "ci-meta-gate-coverage-test: could not find the Tier D cron test step in $CI_YML" >&2; exit 2; }

if [[ "$tier_d_if" == *"outputs.ansible"* && "$tier_d_if" == *"outputs.go"* ]]; then
  ok "Tier D cron test step runs on a Go-only diff too (if: $tier_d_if)"
else
  bad "Tier D cron test step's if-condition is '$tier_d_if' — a Go-only diff to internal/ops/archive/verify_archive.go would skip the only test that reads it"
fi

# ── 2. ci-health watches every push-to-main workflow ────────────────────
watched="$(python3 - "$CI_HEALTH_YML" <<'PY'
import sys, yaml, re
wf = yaml.safe_load(open(sys.argv[1]))
env = wf.get("env", {})
files = env.get("CI_WORKFLOW_FILES", "")
if not files:
    for job in wf["jobs"].values():
        for step in job.get("steps", []):
            run = step.get("run", "")
            if isinstance(run, str):
                files += " " + " ".join(re.findall(r"CI_WORKFLOW_FILE=\"?([\w.-]+)\"?", run))
print(files.strip())
PY
)"

for wf_name in ci.yml api-audit.yml commit-identity.yml; do
  case " $watched " in
    *" $wf_name "*) ok "ci-health watches $wf_name" ;;
    *) bad "ci-health does not watch $wf_name (watched: '${watched:-<none>}') — that push-to-main workflow can go red with no tripwire" ;;
  esac
done

echo "ci-meta-gate-coverage-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
