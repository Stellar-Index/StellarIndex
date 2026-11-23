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
gate = next((s for s in steps[:play] if 'BASE_SHA="$base_sha" bash' in (s.get("run") or "")
             and "scripts/ci/lint-migrations.sh" in (s.get("run") or "")), None)
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
  # ls-tree answers only the live release's migrations listing; any other tree (the deploying
  # tag's own, whose head would floor out every new migration) is logged and refused.
  # shellcheck disable=SC2016 # stub bodies expand when the stubs run
  printf '%s\n' '#!/bin/sh' 'case "$1" in' \
    'rev-parse) echo 0123456789abcdef0123456789abcdef01234567 ;;' \
    'describe) echo v0.0.1 ;;' \
    'ls-tree) if [ "$#" -eq 3 ] && [ "$2" = --name-only ] && [ "$3" = 0123456789abcdef0123456789abcdef01234567:migrations ]; then' \
    '  printf "0001_a.up.sql\\n0007_b.up.sql\\n0003_c.down.sql\\n"; else echo "$*" >>"$FAKE_GIT_LOG"; exit 1; fi ;;' \
    'show) echo "echo LINT-REACHED BASE_SHA=\${BASE_SHA:-} FLOOR=\${MIGRATIONS_CONTENT_FLOOR:-}" ;;' \
    '*) exit 1 ;;' 'esac' > "$tmp/bin/git"
  chmod +x "$tmp/bin/git"
  : > "$tmp/git.log"
  gate_run() {
    (cd "$tmp/ws" && PATH="$tmp/bin:$PATH" GITHUB_WORKSPACE="$tmp/ws" RUNNER_TEMP="$tmp/rt" GITHUB_SHA=feedface \
      FAKE_GIT_LOG="$tmp/git.log" VERSION=v0.60.0 BASELINE="$1" bash -e "$tmp/numbering-run.sh" </dev/null 2>&1)
  }
  got="$(gate_run v0.59.0)"
  if [ -s "$tmp/git.log" ]; then
    bad "numbering gate listed a tree other than the live release's migrations: $(tr '\n' ' ' <"$tmp/git.log")"
  else
    ok "numbering gate lists only the live release's migrations"
  fi
  if [[ "$got" == *"LINT-REACHED BASE_SHA=0123456789abcdef0123456789abcdef01234567"* ]]; then
    ok "numbering gate reaches the lint with the base's sha when a base is available"
  else
    bad "numbering gate with BASELINE set returned before running lint-migrations.sh with BASE_SHA — output: ${got}"
  fi
  if [[ "$got" == *"FLOOR=0007"* ]]; then
    ok "numbering gate floors the content rules at the live release's highest migration"
  else
    bad "numbering gate does not pass the live release's highest migration as MIGRATIONS_CONTENT_FLOOR — a rollback tag's applied migrations are judged by newer content rules: ${got}"
  fi
  # No host version (fresh host, wiped sidecars): the ancestry tag still drives numbering, but
  # nothing proves its migrations are applied, so no floor.
  got="$(gate_run "")"
  if grep -qx 'LINT-REACHED BASE_SHA=0123456789abcdef0123456789abcdef01234567 FLOOR=' <<<"$got"; then
    ok "with no host baseline the gate keeps BASE_SHA and sets no content floor"
  else
    bad "with no host baseline the gate must pass BASE_SHA and no MIGRATIONS_CONTENT_FLOOR — a fresh host would apply the previous tag's migrations unjudged: ${got}"
  fi
fi

# lint-migrations.sh's floor, on a copy of the real tree whose newest down migration deletes rows
# silently. Applied (<= floor) is exempt, new (> floor) fails, and a bad floor fails closed.
fx="$tmp/fx"
mkdir -p "$fx/x/ci" "$fx/deploy" "$fx/scripts/ci"
cp scripts/ci/lint-migrations.sh "$fx/x/ci/lint-migrations.sh"
cp scripts/ci/lint-migrations-ch-float.baseline "$fx/scripts/ci/"
cp -R migrations "$fx/migrations"
cp -R deploy/clickhouse "$fx/deploy/clickhouse"
newest_down="$(find "$fx/migrations" -name '*.down.sql' | sort | tail -1)"
nn="$(basename "$newest_down" | sed -E 's/^([0-9]+)_.*/\1/')"
printf '\nDELETE FROM trades;\n' >> "$newest_down"
floor_run() { (cd "$fx" && MIGRATIONS_CONTENT_FLOOR="$1" bash x/ci/lint-migrations.sh >"$tmp/fl.out" 2>&1); }
if floor_run "$nn"; then ok "a down-delete in an already-applied migration (floor ${nn}) passes"; else bad "floor ${nn} still fails on the applied migration: $(grep -m3 '❌' "$tmp/fl.out")"; fi
if floor_run "$((10#$nn - 1))"; then
  bad "a down-delete in a migration above the floor passed"
elif grep -q 'down deletes rows silently' "$tmp/fl.out"; then
  ok "a down-delete above the floor fails"
else
  bad "failed for the wrong reason: $(grep -m3 '❌' "$tmp/fl.out")"
fi
if floor_run ""; then bad "no floor judged nothing"; else ok "no floor judges every migration"; fi
if floor_run "abc"; then
  bad "a non-numeric floor passed"
elif grep -q 'not a number' "$tmp/fl.out"; then
  ok "a non-numeric floor fails closed"
else
  bad "bad floor failed for the wrong reason"
fi

# The checkout is the deploying tag: its copy of a gate is the gate as it stood at that tag, so
# an old tag would be judged by an old gate. Each run must read the dispatching commit's copy.
from_tag="$(python3 - "$WORKFLOW" <<'PY'
import re, sys, yaml
steps = yaml.safe_load(open(sys.argv[1]))["jobs"]["deploy"].get("steps", [])
for name in ("lint-migrations.sh", "lint-migration-immutability.sh"):
    tree = re.compile(r'(^|[\s;&|])(\./)?scripts/ci/' + re.escape(name) + r'($|[\s;&|])')
    for s in steps:
        run = s.get("run") or ""
        if "scripts/ci/" + name not in run:
            continue
        if any(tree.search(l) for l in run.splitlines()) or \
                'git show "${GITHUB_SHA}:scripts/ci/' + name + '"' not in run:
            print(name + " in step '" + s.get("name", "?") + "'")
PY
)" || { echo "deploy-migration-gate-coverage-test: could not read the deploy job's gate steps" >&2; exit 2; }
if [ -z "$from_tag" ]; then
  ok "lint-migrations.sh and lint-migration-immutability.sh run from the dispatching commit"
else
  while IFS= read -r l; do
    bad "deploy.yml runs the tag's own copy of ${l}; read it with git show \"\${GITHUB_SHA}:…\""
  done <<<"$from_tag"
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
