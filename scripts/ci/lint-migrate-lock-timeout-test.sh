#!/usr/bin/env bash
# lint-migrate-lock-timeout-test.sh — regression test for GH-1166: both
# ansible call sites of `stellarindex-migrate ... up` must set
# PGOPTIONS with lock_timeout + statement_timeout (the DDL-lock-convoy
# mitigation the 2026-07-18 deploy plan recorded but never wired in),
# and must bound the whole invocation with a process-level `timeout`
# (ansible.builtin.shell/command have no task-level timeout keyword of
# their own).
#
# Run: bash scripts/ci/lint-migrate-lock-timeout-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
pass=0
fail=0

check() { # check <desc> <ok>
  local desc="$1" ok="$2"
  if [ "$ok" -eq 0 ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc"
    fail=$((fail + 1))
  fi
}

# ── deploy-binary.yml: ansible.builtin.shell task ──
out="$(python3 - <<'PY'
import yaml, sys
with open("configs/ansible/playbooks/deploy-binary.yml") as f:
    plays = yaml.safe_load(f)
found = False
for play in plays:
    for task in play.get("pre_tasks", []) + play.get("tasks", []):
        if task.get("name") == "Apply outstanding migrations":
            found = True
            env = task.get("environment", {})
            cmd = task.get("ansible.builtin.shell", {}).get("cmd", "")
            opts = env.get("PGOPTIONS", "")
            print("PGOPTIONS=" + opts)
            print("HAS_TIMEOUT_WRAP=" + str("timeout 900" in cmd))
if not found:
    print("TASK_NOT_FOUND")
PY
)"
case "$out" in
  *TASK_NOT_FOUND*)
    check "deploy-binary.yml: 'Apply outstanding migrations' task exists" 1
    ;;
  *)
    case "$out" in
      *"PGOPTIONS="*"lock_timeout"*"statement_timeout"*) check "deploy-binary.yml sets PGOPTIONS with lock_timeout + statement_timeout" 0 ;;
      *) check "deploy-binary.yml sets PGOPTIONS with lock_timeout + statement_timeout" 1 ;;
    esac
    case "$out" in
      *"HAS_TIMEOUT_WRAP=True"*) check "deploy-binary.yml wraps the invocation in a process timeout" 0 ;;
      *) check "deploy-binary.yml wraps the invocation in a process timeout" 1 ;;
    esac
    ;;
esac

# ── 14-stellarindex-services.yml: ansible.builtin.command task ──
out="$(python3 - <<'PY'
import yaml, sys
with open("configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml") as f:
    tasks = yaml.safe_load(f)
found = False
for task in tasks:
    if task.get("name") == "Run pending migrations against the stellarindex database":
        found = True
        env = task.get("environment", {})
        argv = task.get("ansible.builtin.command", {}).get("argv", [])
        opts = env.get("PGOPTIONS", "")
        print("PGOPTIONS=" + opts)
        print("HAS_TIMEOUT_WRAP=" + str(argv[:2] == ["timeout", "900"]))
if not found:
    print("TASK_NOT_FOUND")
PY
)"
case "$out" in
  *TASK_NOT_FOUND*)
    check "14-stellarindex-services.yml: migration task exists" 1
    ;;
  *)
    case "$out" in
      *"PGOPTIONS="*"lock_timeout"*"statement_timeout"*) check "14-stellarindex-services.yml sets PGOPTIONS with lock_timeout + statement_timeout" 0 ;;
      *) check "14-stellarindex-services.yml sets PGOPTIONS with lock_timeout + statement_timeout" 1 ;;
    esac
    case "$out" in
      *"HAS_TIMEOUT_WRAP=True"*) check "14-stellarindex-services.yml wraps the invocation in a process timeout" 0 ;;
      *) check "14-stellarindex-services.yml wraps the invocation in a process timeout" 1 ;;
    esac
    ;;
esac

echo "lint-migrate-lock-timeout-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
