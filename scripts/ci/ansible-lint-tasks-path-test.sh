#!/usr/bin/env bash
# ansible-lint-tasks-path-test.sh — pins that ci.yml's ansible-lint step
# actually scans configs/ansible/tasks/, not just roles/ and playbooks/.
#
# Q256 (audit-2026-09-18): the ansible-lint invocation in ci.yml was
# `ansible-lint --profile moderate roles/ playbooks/`. configs/ansible/tasks/
# (deploy-one-binary.yml, sync-migrations.yml — imported into
# playbooks/deploy-binary.yml and roles/archival-node/tasks/
# 14-stellarindex-services.yml via import_tasks) sat outside both explicit
# paths, so a moderate/production-tier violation introduced in either file
# could not fail this REQUIRED check.
#
# Run: bash scripts/ci/ansible-lint-tasks-path-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

CI_YML=".github/workflows/ci.yml"

pass=0
fail=0

extract_step() { # extract_step <file> <step-name-substring>
  local file="$1" name="$2"
  awk -v name="$name" '
    /^      - name:/ {
      if (in_step) { exit }
      if (index($0, name) > 0) { in_step = 1 } else { next }
    }
    in_step { print }
  ' "$file"
}

step_block="$(extract_step "$CI_YML" "ansible-lint (moderate profile")"
run_line="$(grep -E -- '^[[:space:]]*run:[[:space:]]*ansible-lint\b' <<<"$step_block")"

if [ -z "$step_block" ]; then
  echo "  FAIL ansible-lint step not found in $CI_YML" >&2
  fail=$((fail + 1))
elif [[ "$run_line" != *' roles/ playbooks/ tasks/'* ]]; then
  echo "  FAIL ansible-lint step in $CI_YML does not lint configs/ansible/tasks/:" >&2
  printf '%s\n' "$step_block" | sed 's/^/       | /' >&2
  fail=$((fail + 1))
else
  echo "  ok   ansible-lint step lints roles/, playbooks/ AND tasks/"
  pass=$((pass + 1))
fi

echo "ansible-lint-tasks-path-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
