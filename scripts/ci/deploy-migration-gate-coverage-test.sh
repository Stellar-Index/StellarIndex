#!/usr/bin/env bash
# deploy-migration-gate-coverage-test.sh — deploy.yml's migration gate
# must run all four migration lints, not just compat.
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
# nothing. It also pins the migration follow-up gate: before the playbook,
# on followups_acknowledged. Structural: parses the real workflow, no
# network, no gh.
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

# The follow-up gate must run before the playbook applies migrations, and on
# its own acknowledgement: config_acknowledged asserts a surface is already
# applied, which a replay that has not run yet can never be.
# With no BASE_SHA the numbering check compares the tag with itself and sees nothing. The base
# must be the NEWEST release the host ran (migrate included): the database is migrated that far.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
numbering="$(python3 - "$WORKFLOW" "$tmp/numbering-run.sh" <<'PY'
import sys, yaml
steps = yaml.safe_load(open(sys.argv[1]))["jobs"]["deploy"].get("steps", [])
play = next((i for i, s in enumerate(steps) if s.get("id") == "playbook"), len(steps))
gate = next((s for s in steps[:play] if 'BASE_SHA="$base_sha" bash scripts/ci/lint-migrations.sh' in (s.get("run") or "")), None)
if gate is None:
    print("absent")
    sys.exit(0)
print("present")
env = gate.get("env", {})
print("newest" if str(env.get("BASELINE", "")).replace(" ", "") == "${{steps.baseline.outputs.newest}}" else "not-newest")
open(sys.argv[2], "w").write(gate["run"])
PY
)" || { echo "deploy-migration-gate-coverage-test: could not read the numbering gate step" >&2; exit 2; }
if grep -qx present <<<"$numbering"; then
  ok "numbering gate runs lint-migrations.sh with BASE_SHA before the playbook"
else
  bad "no step runs lint-migrations.sh with BASE_SHA before the playbook — the back-numbering check compares the tag with itself"
fi
if grep -qx newest <<<"$numbering"; then
  ok "numbering gate's base is steps.baseline.outputs.newest"
else
  bad "numbering gate's BASELINE is not steps.baseline.outputs.newest — a lower base lets a migration numbered below the database's version through"
fi
# Execute the step against stubs: with a base available it must reach the lint with that base's sha.
if [ -s "$tmp/numbering-run.sh" ]; then
  mkdir -p "$tmp/bin" "$tmp/ws/scripts/ci"
  # shellcheck disable=SC2016 # stub bodies expand when the stubs run
  printf '%s\n' '#!/bin/sh' 'case "$1" in rev-parse) echo 0123456789abcdef0123456789abcdef01234567 ;; describe) echo v0.0.1 ;; *) exit 1 ;; esac' > "$tmp/bin/git"
  # shellcheck disable=SC2016
  printf '%s\n' 'echo "LINT-REACHED BASE_SHA=${BASE_SHA:-}"' > "$tmp/ws/scripts/ci/lint-migrations.sh"
  chmod +x "$tmp/bin/git"
  got="$(cd "$tmp/ws" && PATH="$tmp/bin:$PATH" VERSION=v0.60.0 BASELINE=v0.59.0 bash -e "$tmp/numbering-run.sh" </dev/null 2>&1)"
  if [[ "$got" == *"LINT-REACHED BASE_SHA=0123456789abcdef0123456789abcdef01234567"* ]]; then
    ok "numbering gate reaches the lint with the base's sha when a base is available"
  else
    bad "numbering gate with BASELINE set returned before running lint-migrations.sh with BASE_SHA — output: ${got}"
  fi
fi

followup="$(python3 - "$WORKFLOW" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
steps = wf["jobs"]["deploy"].get("steps", [])
gate = next((i for i, s in enumerate(steps) if "scripts/ci/migration-followup-gate.sh" in (s.get("run") or "")), None)
play = next((i for i, s in enumerate(steps) if s.get("id") == "playbook"), None)
inputs = wf.get("on", wf.get(True, {}))["workflow_dispatch"]["inputs"]
env = steps[gate].get("env", {}) if gate is not None else {}
print("present" if gate is not None else "absent")
print("before" if gate is not None and play is not None and gate < play else "not-before")
print("own-ack" if "followups_acknowledged" in inputs
      and any("inputs.followups_acknowledged" in str(v) for v in env.values())
      and not any("config_acknowledged" in str(v) for v in env.values()) else "no-own-ack")
# The job checks out the deploying tag, and tags cut before the gate existed
# lack the script: it must come from the dispatching commit, never the tag tree.
run = (steps[gate].get("run") or "") if gate is not None else ""
from_tree = any(l.strip().startswith(("bash scripts/ci/migration-followup-gate.sh", "scripts/ci/migration-followup-gate.sh", "./scripts/ci/migration-followup-gate.sh"))
                for l in run.splitlines())
print("from-dispatch-sha" if 'git show "${GITHUB_SHA}:scripts/ci/migration-followup-gate.sh"' in run
      and not from_tree else "from-tag-tree")
PY
)" || { echo "deploy-migration-gate-coverage-test: could not read the follow-up gate step" >&2; exit 2; }
for want in present before own-ack from-dispatch-sha; do
  if grep -qx "$want" <<<"$followup"; then
    ok "migration follow-up gate: ${want}"
  else
    bad "migration follow-up gate: want ${want} — the gate must run before the deploy playbook, on followups_acknowledged alone, from the dispatching commit's copy of the script"
  fi
done

echo "deploy-migration-gate-coverage-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
