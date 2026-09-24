#!/usr/bin/env bash
# deploy-checkout-ref-test.sh — the deploy job's checkout must pin `ref:`
# to the tag being deployed (GH-885).
#
# Without a `ref:`, actions/checkout resolves the dispatching ref (main
# in practice), so every artefact scp'd from the working tree —
# configs/prometheus/rules.r1/, apply-rules.sh — carries main's bytes
# while scripts/ci/config-apply-gate.sh diffs the deploying TAG against
# the previous release tag (`git diff "$PREV" "$VERSION" -- rules.r1/`).
# The gate then credits a step that applied a DIFFERENT commit's rules
# as having applied the tag's. Structural: parses the real workflow, no
# network, no gh.
#
# Run: bash scripts/ci/deploy-checkout-ref-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
WORKFLOW="${WORKFLOW:-.github/workflows/deploy.yml}"
[[ -r "$WORKFLOW" ]] || { echo "deploy-checkout-ref-test: missing $WORKFLOW" >&2; exit 2; }

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

ref="$(python3 - "$WORKFLOW" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
job = wf["jobs"]["deploy"]
for step in job.get("steps", []):
    uses = step.get("uses", "")
    if uses.startswith("actions/checkout@"):
        print(step.get("with", {}).get("ref", ""))
        sys.exit(0)
sys.exit(1)
PY
)" || { echo "deploy-checkout-ref-test: could not find the deploy job's checkout step" >&2; exit 2; }

# shellcheck disable=SC2016  # literal ${{ inputs.version }} must NOT expand — it's the workflow-expression string we compare against
if [ "$ref" = '${{ inputs.version }}' ]; then
  ok "deploy job's checkout pins ref to \${{ inputs.version }}"
else
  bad "deploy job's checkout ref is '${ref:-<unset>}', want \${{ inputs.version }} — the working tree can diverge from the tag the config-apply gate verifies"
fi

echo "deploy-checkout-ref-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
