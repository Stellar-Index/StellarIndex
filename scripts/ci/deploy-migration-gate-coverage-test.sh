#!/usr/bin/env bash
# deploy-migration-gate-coverage-test.sh — deploy.yml's migration gate
# must run all four migration lints, not just compat (GH-1165).
#
# ci.yml runs four migration lints: lint-migrations.sh (numbering,
# money-column, register), lint-migration-compat.sh (rule-9
# additive/old-binary-safe), lint-migration-immutability.sh (frozen
# shipped migrations) and lint-migration-commands.sh (tested header
# commands). deploy.yml's own comment calls its migration step "the LAST
# gate before DDL runs on production" and notes `main` is unprotected, so
# a migration can reach a tag without ever passing ci.yml. Before the
# fix, deploy.yml ran only lint-migration-compat.sh --staged: an
# in-place edit of an already-shipped migration, a duplicate/back-filed
# number, or an untested header command reached production checked by
# nothing. Structural: parses the real workflow, no network, no gh.
#
# Run: bash scripts/ci/deploy-migration-gate-coverage-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
WORKFLOW="${WORKFLOW:-.github/workflows/deploy.yml}"
[[ -r "$WORKFLOW" ]] || { echo "deploy-migration-gate-coverage-test: missing $WORKFLOW" >&2; exit 2; }

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

run_lines="$(python3 - "$WORKFLOW" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
job = wf["jobs"]["deploy"]
for step in job.get("steps", []):
    run = step.get("run")
    if run:
        print(run)
        print("---STEP---")
PY
)" || { echo "deploy-migration-gate-coverage-test: could not read the deploy job's steps" >&2; exit 2; }

for script in lint-migrations.sh lint-migration-compat.sh lint-migration-immutability.sh lint-migration-commands.sh; do
  if grep -qF "scripts/ci/${script}" <<<"$run_lines"; then
    ok "deploy.yml runs ${script}"
  else
    bad "deploy.yml never runs ${script} — the last gate before DDL runs on production is missing this migration lint"
  fi
done

echo "deploy-migration-gate-coverage-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
