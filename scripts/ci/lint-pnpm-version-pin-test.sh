#!/usr/bin/env bash
# lint-pnpm-version-pin-test.sh — fixture test for
# scripts/ci/lint-pnpm-version-pin.sh (T547, audit-2026-09-18).
#
# Run: bash scripts/ci/lint-pnpm-version-pin-test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINT="$SCRIPT_DIR/lint-pnpm-version-pin.sh"

fail=0
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/drifting.yml" <<'EOF'
jobs:
  build:
    steps:
      - uses: pnpm/action-setup@deadbeef  # v6
        with:
          version: "10"
EOF

cat > "$tmp/pinned.yml" <<'EOF'
jobs:
  build:
    steps:
      - uses: pnpm/action-setup@deadbeef  # v6
        with:
          package_json_file: web/explorer/package.json
EOF

if bash "$LINT" "$tmp" >/tmp/lint-pnpm-version-pin-test.out 2>&1; then
  echo "FAIL: drifting.yml's bare 'version:' input was accepted"
  cat /tmp/lint-pnpm-version-pin-test.out
  fail=1
else
  echo "OK: bare 'version:' pnpm/action-setup input rejected"
fi

rm -f "$tmp/drifting.yml"
if bash "$LINT" "$tmp" >/tmp/lint-pnpm-version-pin-test.out 2>&1; then
  echo "OK: package_json_file-only tree passes"
else
  echo "FAIL: package_json_file-only tree was rejected"
  cat /tmp/lint-pnpm-version-pin-test.out
  fail=1
fi

rm -f /tmp/lint-pnpm-version-pin-test.out
exit $fail
