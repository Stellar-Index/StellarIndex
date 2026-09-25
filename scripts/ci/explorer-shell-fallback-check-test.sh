#!/usr/bin/env bash
# explorer-shell-fallback-check-test.sh — fixture tests for
# scripts/ci/explorer-shell-fallback-check.sh (T329, audit-2026-09-18).
#
# Builds two throwaway trees (a synthetic `functions/` calling
# shellFallback(context, '<path>') and a synthetic `out/`) and asserts the
# checker passes when every discovered shell path has a real artefact and
# fails when one is missing.
#
# Run: bash scripts/ci/explorer-shell-fallback-check-test.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/explorer-shell-fallback-check.sh"

fail=0
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/functions/assets" "$tmp/functions/accounts"
cat > "$tmp/functions/assets/[[path]].js" <<'EOF'
import { shellFallback } from '../_shared/shellFallback.js';
export async function onRequest(context) {
  return shellFallback(context, '/assets/shell/');
}
EOF
cat > "$tmp/functions/accounts/[[path]].js" <<'EOF'
import { shellFallback } from '../_shared/shellFallback.js';
export async function onRequest(context) {
  return shellFallback(context, '/accounts/shell/');
}
EOF

# Case 1: both shell artefacts present -> passes.
mkdir -p "$tmp/out-complete/assets/shell" "$tmp/out-complete/accounts/shell"
echo ok > "$tmp/out-complete/assets/shell/index.html"
echo ok > "$tmp/out-complete/accounts/shell/index.html"
if bash "$CHECK" "$tmp/out-complete" "$tmp/functions" >/tmp/explorer-shell-check-test.out 2>&1; then
  echo "OK: passes when every shell artefact is present"
else
  echo "FAIL: expected pass with every shell artefact present"
  cat /tmp/explorer-shell-check-test.out
  fail=1
fi

# Case 2 (the regression this guards): one route's generateStaticParams
# stopped emitting the shell sentinel, so its artefact is missing while
# the build otherwise "succeeded" -- must fail closed, not pass silently.
mkdir -p "$tmp/out-missing/accounts/shell"
echo ok > "$tmp/out-missing/accounts/shell/index.html"
# out-missing/assets/shell/index.html deliberately absent.
if bash "$CHECK" "$tmp/out-missing" "$tmp/functions" >/tmp/explorer-shell-check-test.out 2>&1; then
  echo "FAIL: missing shell artefact for /assets/shell/ was accepted"
  cat /tmp/explorer-shell-check-test.out
  fail=1
else
  echo "OK: missing shell artefact rejected"
fi
if ! grep -q "missing shell artefact for fallback route '/assets/shell/'" /tmp/explorer-shell-check-test.out; then
  echo "FAIL: error did not name the missing route"
  fail=1
fi

# Case 3: vacuity guard -- zero shellFallback(...) calls discovered must
# fail, not report a clean 0-of-0.
mkdir -p "$tmp/empty-functions" "$tmp/out-complete"
if bash "$CHECK" "$tmp/out-complete" "$tmp/empty-functions" >/tmp/explorer-shell-check-test.out 2>&1; then
  echo "FAIL: 0 discovered shellFallback calls was accepted as clean"
  cat /tmp/explorer-shell-check-test.out
  fail=1
else
  echo "OK: 0 discovered shellFallback calls rejected (vacuity guard)"
fi

rm -f /tmp/explorer-shell-check-test.out
exit $fail
