#!/usr/bin/env bash
# lint-pnpm-version-pin.sh — every pnpm/action-setup step must resolve its
# pnpm version from the package.json it's about to install for
# (`package_json_file:`), never a hardcoded `version:` input.
#
# T547 (audit-2026-09-18): several workflow steps pinned pnpm via
# `version: "10"` (major-only) while the package.json they install for
# declares an exact `packageManager: "pnpm@10.33.0"` — two independent
# copies of the same fact that can (and did) drift apart. action-setup
# reading `package_json_file:` makes the package.json the single source
# of truth; there is nothing left to drift.
#
# Usage: scripts/ci/lint-pnpm-version-pin.sh [workflows-dir]
set -euo pipefail

DIR="${1:-.github/workflows}"

if [ ! -d "$DIR" ]; then
  echo "lint-pnpm-version-pin: '$DIR' not found." >&2
  exit 2
fi

fail=0
checked=0
for file in "$DIR"/*.yml; do
  [ -f "$file" ] || continue
  # For each `uses: pnpm/action-setup...` line, its `with:` block (the
  # following few lines) must not carry a `version:` input.
  hit="$(grep -A 4 "uses:.*pnpm/action-setup" "$file" | grep -E "^\s*version:" || true)"
  count="$(grep -c "uses:.*pnpm/action-setup" "$file" || true)"
  if [ "${count:-0}" -gt 0 ] && [ -n "$hit" ]; then
    checked=$((checked + 1))
    echo "::error::${file}: pnpm/action-setup pinned via a bare 'version:' input; use 'package_json_file:' instead so it can't drift from the package.json's packageManager pin." >&2
    fail=1
  fi
done

echo "lint-pnpm-version-pin: ${checked} workflow file(s) with a bare 'version:' pnpm/action-setup step found."
exit $fail
