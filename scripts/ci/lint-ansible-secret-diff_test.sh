#!/usr/bin/env bash
# lint-ansible-secret-diff_test.sh — T492: the scan glob covered only
# ROLES_DIR/*/tasks/*.yml, so a secret-rendering `ansible.builtin.template`
# task placed in a role's handlers/ (or a playbook, or the shared
# configs/ansible/tasks/) was invisible to this lint. This plants one in a
# fake role's handlers/ file and asserts the lint catches it.
#
# Run: bash scripts/ci/lint-ansible-secret-diff_test.sh
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LINT="$REPO_ROOT/scripts/ci/lint-ansible-secret-diff.py"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

mkdir -p "$TMPDIR/configs/ansible/roles/fakerole/handlers"
mkdir -p "$TMPDIR/configs/ansible/roles/fakerole/templates"

cat > "$TMPDIR/configs/ansible/roles/fakerole/handlers/main.yml" <<'EOF'
---
- name: Render leaked secret config
  ansible.builtin.template:
    src: leak.conf.j2
    dest: /etc/leak.conf
EOF

cat > "$TMPDIR/configs/ansible/roles/fakerole/templates/leak.conf.j2" <<'EOF'
password = {{ fake_service_password }}
EOF

set +e
OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
STATUS=$?
set -e

echo "$OUTPUT"

if [[ $STATUS -eq 0 ]]; then
  echo "FAIL: lint passed (exit 0) on a secret-rendering template task hidden in handlers/main.yml — the handlers/ scan gap (T492) reproduced" >&2
  exit 1
fi

if [[ "$OUTPUT" != *"fakerole/handlers/main.yml::Render leaked secret config"* ]]; then
  echo "FAIL: lint failed for the wrong reason — did not name the handlers/main.yml violation" >&2
  exit 1
fi

echo "PASS: lint-ansible-secret-diff catches a secret-rendering template task in a role's handlers/ file"
