#!/usr/bin/env bash
# install-ansible.sh — one installer for the two Ansible toolchain shapes
# used across deploy.yml, ansible-drift.yml and ci.yml.
#
# One pin, not an inline version string per workflow: separate copies
# drift and break the collection pins. Both pins live in
# configs/ansible/toolchain.txt; this
# script is the only place that reads them.
#
# Usage: install-ansible.sh core|bundle
#   core   — pip install ansible-core only (deploy.yml's lean install;
#            requirements.yml is the sole collection source of truth).
#   bundle — pipx install the full `ansible` metapackage, including its
#            bundled collections (ansible-drift.yml / ci.yml's
#            syntax-check / lint / dry-run installs).
#
# Does not install Galaxy collections or anything workflow-specific
# (ansible-lint, passlib) — callers do that themselves after this runs.
set -euo pipefail

cd "$(dirname "$0")/../.."

TOOLCHAIN="configs/ansible/toolchain.txt"
if [ ! -f "$TOOLCHAIN" ]; then
  echo "install-ansible: FAIL — $TOOLCHAIN not found" >&2
  exit 1
fi

mode="${1:-}"
case "$mode" in
  core | bundle) ;;
  *)
    echo "install-ansible: usage: $0 core|bundle" >&2
    exit 1
    ;;
esac

core_version="$(sed -n 's/^ANSIBLE_CORE_VERSION=\(.*\)$/\1/p' "$TOOLCHAIN")"
ansible_version="$(sed -n 's/^ANSIBLE_VERSION=\(.*\)$/\1/p' "$TOOLCHAIN")"

if [ -z "$core_version" ]; then
  echo "install-ansible: FAIL — no ANSIBLE_CORE_VERSION in $TOOLCHAIN" >&2
  exit 1
fi
if [ -z "$ansible_version" ]; then
  echo "install-ansible: FAIL — no ANSIBLE_VERSION in $TOOLCHAIN" >&2
  exit 1
fi

if [ "$mode" = "core" ]; then
  python3 -m pip install 'pip==24.3.1'
  python3 -m pip install "ansible-core==${core_version}"
else
  pipx install --include-deps "ansible==${ansible_version}"
fi
