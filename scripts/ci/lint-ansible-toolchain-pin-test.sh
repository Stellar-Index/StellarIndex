#!/usr/bin/env bash
# lint-ansible-toolchain-pin-test.sh — fixture tests for
# scripts/ci/lint-ansible-toolchain-pin.sh (GH-896).
#
# Run: bash scripts/ci/lint-ansible-toolchain-pin-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/lint-ansible-toolchain-pin.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

check() {
  local desc="$1" want_rc="$2" dir="$3"
  local out rc
  out="$(bash "$CHECK" "$dir" 2>&1)"
  rc=$?
  if [ "$rc" = "$want_rc" ]; then
    pass=$((pass + 1))
    echo "ok: $desc"
  else
    fail=$((fail + 1))
    echo "FAIL: $desc — want rc=$want_rc got rc=$rc" >&2
    echo "$out" >&2
  fi
}

clean_dir="$TMP/clean"
mkdir -p "$clean_dir"
cat > "$clean_dir/ci.yml" <<'EOF'
jobs:
  ansible-check:
    steps:
      - name: Install ansible
        run: ./scripts/ci/install-ansible.sh bundle
EOF
check "no inline pin → OK" 0 "$clean_dir"

dirty_dir="$TMP/dirty"
mkdir -p "$dirty_dir"
cat > "$dirty_dir/ci.yml" <<'EOF'
jobs:
  ansible-check:
    steps:
      - name: Install ansible
        run: pipx install --include-deps 'ansible==14.2.0'
EOF
check "inline pipx pin → FAIL" 1 "$dirty_dir"

dirty_core_dir="$TMP/dirty-core"
mkdir -p "$dirty_core_dir"
cat > "$dirty_core_dir/deploy.yml" <<'EOF'
jobs:
  deploy:
    steps:
      - name: Install Ansible
        run: python3 -m pip install 'ansible-core==2.18.4'
EOF
check "inline pip core pin → FAIL" 1 "$dirty_core_dir"

check "missing directory → FAIL" 1 "$TMP/does-not-exist"

echo "lint-ansible-toolchain-pin-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
