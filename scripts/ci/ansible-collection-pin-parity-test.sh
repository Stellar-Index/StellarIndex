#!/usr/bin/env bash
# ansible-collection-pin-parity-test.sh — pins that ci.yml's ansible
# syntax/lint gate and ansible-drift.yml's drift-check (which can APPLY
# to production r1) install the SAME collection versions deploy.yml
# actually applies.
#
# F146 (audit-2026-09-02): both jobs installed only the batteries-
# included `ansible` pipx bundle, whose community.general/ansible.posix/
# community.postgresql resolve to whatever pipx picked for that ansible
# release (community.general 11.x-class under ansible 14.2.0). Neither
# job ever installed configs/ansible/requirements.yml, the file
# deploy.yml uses to pin community.general to 9.5.0 (ansible.posix
# 1.5.4, community.postgresql 3.10.0) at apply time. A task using a
# module argument present in the bundle but absent/changed in the
# pinned collection passed ci.yml's syntax-check + ansible-lint and
# ansible-drift.yml's `--check --diff` clean, then broke — or silently
# diverged — on the real r1 apply, which ansible-drift.yml's `apply=true`
# input can trigger directly against production.
#
# This asserts, against the WORKING TREE (not a fixture): the ansible
# install step in each of ci.yml's `ansible-check` job and
# ansible-drift.yml's `drift-check` job runs
#   ansible-galaxy collection install -r configs/ansible/requirements.yml
# — i.e. that the two CI-side jobs consume the exact file deploy.yml
# consumes, not just whatever the pipx-resolved bundle happened to ship.
#
# Run: bash scripts/ci/ansible-collection-pin-parity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

CI_YML=".github/workflows/ci.yml"
DRIFT_YML=".github/workflows/ansible-drift.yml"
PIN_LINE="ansible-galaxy collection install -r configs/ansible/requirements.yml"

pass=0
fail=0

# extract_step <file> <step-name-substring>
# Prints the named step's block (header line through, but not
# including, the next `- name:` step header), matched by the literal
# step-name text rather than a line number so this survives edits
# elsewhere in the file.
extract_step() {
  local file="$1" name="$2"
  awk -v name="$name" '
    /^      - name:/ {
      if (in_step) { exit }
      if (index($0, name) > 0) { in_step = 1 } else { next }
    }
    in_step { print }
  ' "$file"
}

check_step_has_pin() { # check_step_has_pin <desc> <file> <step-name>
  local desc="$1" file="$2" step="$3" block
  block="$(extract_step "$file" "$step")"
  if [ -z "$block" ]; then
    echo "  FAIL $desc — step '$step' not found in $file" >&2
    fail=$((fail + 1))
    return
  fi
  if grep -qF -- "$PIN_LINE" <<<"$block"; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc — '$step' in $file does not install the pinned collections:" >&2
    echo "       expected to find: $PIN_LINE" >&2
    printf '%s\n' "$block" | sed 's/^/       | /' >&2
    fail=$((fail + 1))
  fi
}

echo "ansible-collection-pin-parity-test: CI-side jobs install the pinned collections"

check_step_has_pin \
  "ci.yml ansible-check job installs configs/ansible/requirements.yml before syntax/lint" \
  "$CI_YML" "Install ansible + lint"

check_step_has_pin \
  "ansible-drift.yml drift-check job installs configs/ansible/requirements.yml before --check/apply" \
  "$DRIFT_YML" "Install ansible"

# Sanity: the pin line must appear BEFORE any ansible-playbook or
# ansible-lint invocation in the same job, or the override would race
# a step that already ran against the unpinned bundle. Checked by
# confirming the FIRST occurrence of `ansible-playbook`/`ansible-lint`
# in each workflow's `ansible`-class job steps comes after the install
# step that contains the pin line (both scripts run steps top-to-bottom
# in file order, so a simple line-number comparison holds).
check_pin_precedes_use() { # check_pin_precedes_use <desc> <file>
  local desc="$1" file="$2" pin_line use_line
  pin_line="$(grep -nF -m1 -- "$PIN_LINE" "$file" | cut -d: -f1)"
  use_line="$(grep -nE -m1 'ansible-playbook|ansible-lint --profile' "$file" | cut -d: -f1)"
  if [ -z "$pin_line" ] || [ -z "$use_line" ]; then
    echo "  FAIL $desc — could not locate both the pin line and a use line in $file" >&2
    fail=$((fail + 1))
    return
  fi
  if [ "$pin_line" -lt "$use_line" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc — pin line ($pin_line) does not precede first ansible-playbook/-lint use ($use_line) in $file" >&2
    fail=$((fail + 1))
  fi
}

check_pin_precedes_use \
  "ci.yml installs the pin before the first ansible-playbook/ansible-lint invocation" \
  "$CI_YML"

check_pin_precedes_use \
  "ansible-drift.yml installs the pin before the first ansible-playbook invocation" \
  "$DRIFT_YML"

echo "ansible-collection-pin-parity-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
