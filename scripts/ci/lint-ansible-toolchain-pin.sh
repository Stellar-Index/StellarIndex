#!/usr/bin/env bash
# lint-ansible-toolchain-pin.sh — no workflow may hardcode an Ansible
# version inline; every install must route through
# scripts/ci/install-ansible.sh, which reads configs/ansible/toolchain.txt.
#
# Workflows (deploy.yml, ansible-drift.yml, ci.yml) must not each spell out their
# own `ansible==...` / `ansible-core==...` pin, or a bump can land in one
# workflow and not the others. This gate fails if
# `ansible==`/`ansible-core==` (the pip/pipx
# install syntax) appears anywhere under .github/workflows.
set -euo pipefail

cd "$(dirname "$0")/../.."

WORKFLOWS_DIR="${1:-.github/workflows}"

if [ ! -d "$WORKFLOWS_DIR" ]; then
  echo "lint-ansible-toolchain-pin: FAIL — directory not found: $WORKFLOWS_DIR" >&2
  exit 1
fi

hits="$(grep -rnE "['\"]ansible(-core)?==" "$WORKFLOWS_DIR" || true)"

if [ -n "$hits" ]; then
  echo "lint-ansible-toolchain-pin: FAIL — inline Ansible version pin(s) found; route through scripts/ci/install-ansible.sh + configs/ansible/toolchain.txt instead:" >&2
  echo "$hits" >&2
  exit 1
fi

echo "lint-ansible-toolchain-pin: OK — no workflow hardcodes an Ansible version"
