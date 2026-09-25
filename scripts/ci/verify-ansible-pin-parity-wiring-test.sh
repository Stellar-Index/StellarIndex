#!/usr/bin/env bash
# verify-ansible-pin-parity-wiring-test.sh — regression test for GH-896's
# refutation gap: scripts/ci/ansible-collection-pin-parity-test.sh existed
# and passed but was never invoked by anything automated (not verify.sh, not
# a ci.yml job, not prepush.sh, not lint-changed.sh's default lane), so a
# future PR that dropped the `ansible-galaxy collection install` step from
# ci.yml or ansible-drift.yml would sail through `make prepush` clean.
#
# This asserts scripts/dev/verify.sh actually invokes the pin-parity
# self-test, next to its sibling Ansible self-tests. A grep, not a fixture:
# the defect was absence-of-wiring, not wrong logic in the wired script.
#
# Run: bash scripts/ci/verify-ansible-pin-parity-wiring-test.sh
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1

VERIFY_SH="scripts/dev/verify.sh"

if [ ! -f "$VERIFY_SH" ]; then
  echo "verify-ansible-pin-parity-wiring-test: FAIL — $VERIFY_SH not found" >&2
  exit 1
fi

if ! grep -qE '(^|[[:space:]])\./scripts/ci/ansible-collection-pin-parity-test\.sh([[:space:]]|$)' "$VERIFY_SH"; then
  echo "verify-ansible-pin-parity-wiring-test: FAIL — $VERIFY_SH never invokes" \
    "scripts/ci/ansible-collection-pin-parity-test.sh; the pin-parity regression" \
    "test exists but is unreachable from 'make prepush' (GH-896)." >&2
  exit 1
fi

echo "verify-ansible-pin-parity-wiring-test: PASS — verify.sh invokes ansible-collection-pin-parity-test.sh"
