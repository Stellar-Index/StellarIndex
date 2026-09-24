#!/usr/bin/env bash
# node-healthcheck-header-test.sh — T238: the script's leading "Checks"
# comment block must describe the checks the script actually runs, not a
# stale headcount. It previously claimed "All 7 systemd services" over a
# 6-item list while SERVICES held 9 units and Check 1b (API /v1/healthz)
# existed uncounted and undocumented.
#
# Run: bash scripts/ci/node-healthcheck-header-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="configs/ansible/roles/archival-node/files/node-healthcheck.sh"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# Extract the real SERVICES array by sourcing just its declaration —
# the array body is plain bash (comments included), safe to eval in a
# subshell with no other side effects.
actual_count=$(
  sed -n '/^SERVICES=(/,/^)/p' "$SCRIPT" > /tmp/node-healthcheck-services-decl.sh
  # shellcheck disable=SC1091
  . /tmp/node-healthcheck-services-decl.sh
  echo "${#SERVICES[@]}"
)
rm -f /tmp/node-healthcheck-services-decl.sh

header_count=$(grep -oE 'All [0-9]+ systemd services' "$SCRIPT" | grep -oE '[0-9]+')

if [ "$header_count" = "$actual_count" ]; then
  ok "header service count ($header_count) matches SERVICES array ($actual_count)"
else
  bad "header claims $header_count systemd services, SERVICES array has $actual_count"
fi

header_block=$(sed -n '1,25p' "$SCRIPT")

has_check_1b=0
grep -q '^# --- Check 1b: API /v1/healthz' "$SCRIPT" && has_check_1b=1

case "$header_block" in
  *'1b.'*) header_mentions_1b=1 ;;
  *) header_mentions_1b=0 ;;
esac

if [ "$has_check_1b" -eq 1 ] && [ "$header_mentions_1b" -eq 1 ]; then
  # Check 1b's own comment block must exist AND the header must
  # reference it — the drift was the header omitting a real check,
  # not the check itself being missing.
  ok "header documents Check 1b (API /v1/healthz)"
else
  bad "Check 1b exists in the script but is absent from the header comment block, or the check itself is missing"
fi

echo
echo "node-healthcheck-header-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
