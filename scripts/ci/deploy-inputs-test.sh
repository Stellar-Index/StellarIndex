#!/usr/bin/env bash
# deploy-inputs-test.sh — deploy.yml's Validate step must reject every
# dispatch input that could smuggle extra-vars into `ansible-playbook -e`
# (deploy-ansible-input-8).
#
# `-e "health_grace_seconds=${HEALTH_GRACE}"` is ansible's k=v form, which
# is split on WHITESPACE into multiple vars. health_grace_seconds is a
# `type: string` input (GitHub's `number` typing is UI/API-side only;
# `gh workflow run -f` bypasses it), so `15 backup_freshness_skip=true`
# used to sail through Validate and silently skip the backup-freshness
# gate. This extracts the shipped Validate step out of the workflow and
# runs it, so the property under test is the real script, not a twin.
# No network, no gh.
#
# Run: bash scripts/ci/deploy-inputs-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
WORKFLOW="${WORKFLOW:-.github/workflows/deploy.yml}"
[[ -r "$WORKFLOW" ]] || { echo "deploy-inputs-test: missing $WORKFLOW" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

python3 - "$WORKFLOW" "$TMP/validate.sh" <<'PY' || { echo "deploy-inputs-test: could not extract the Validate step" >&2; exit 2; }
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
for job in wf["jobs"].values():
    for step in job.get("steps", []):
        if step.get("name") == "Validate inputs":
            open(sys.argv[2], "w").write(step["run"])
            sys.exit(0)
sys.exit(1)
PY

# run_validate VERSION BINARIES HEALTH_GRACE → exit code; stdout+stderr in $out
run_validate() {
  out="$(VERSION="$1" BINARIES="$2" HEALTH_GRACE="$3" bash "$TMP/validate.sh" 2>&1)"
  return $?
}

if run_validate v1.2.3 stellarindex-api 15; then
  ok "sane inputs (v1.2.3, stellarindex-api, 15) pass"
else
  bad "sane inputs rejected: $out"
fi

INJECT='15 backup_freshness_skip=true migrations_skip=true'
if run_validate v1.2.3 stellarindex-api "$INJECT"; then
  bad "health_grace_seconds='$INJECT' PASSED Validate — whitespace in the value becomes extra ansible -e vars (backup_freshness_skip=true skips the backup-freshness gate)"
elif grep -q "health_grace_seconds" <<<"$out"; then
  ok "health_grace_seconds extra-vars injection is rejected, naming the input"
else
  bad "injection rejected but for the wrong reason: $out"
fi

# shellcheck disable=SC2016  # literal $(id) must NOT expand — it is the attack string
for v in "" "abc" "-1" "1.5" "15;" "99999" '$(id)'; do
  if run_validate v1.2.3 stellarindex-api "$v"; then
    bad "health_grace_seconds='$v' accepted (want 1-4 digits only)"
  else
    ok "health_grace_seconds='$v' rejected"
  fi
done

# The inputs that were already validated must stay validated.
if run_validate 'v1.0.0; id' stellarindex-api 15; then bad "non-SemVer version accepted"; else ok "non-SemVer version still rejected"; fi
if run_validate v1.0.0 '../../etc/passwd' 15; then bad "illegal binary name accepted"; else ok "illegal binary name still rejected"; fi

echo "deploy-inputs-test: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
