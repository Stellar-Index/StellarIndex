#!/usr/bin/env bash
# lint-api-restartsec-parity-test.sh — pins the deployed API unit's
# RestartSec to the value its own reference copy's comment specifies
# (SL15): "API restart policy is the strictest of the three ... Keep the
# cooldown short" (deploy/systemd/stellarindex-api.service). The
# deployed template had drifted to double that value (10s vs 5s).
#
# Runs the real lint-deploy-systemd-authority.sh gate against the
# repo's actual reference/template pair (no fixture tree) and asserts
# it reports no undeclared RestartSec divergence for stellarindex-api.
#
# Run: bash scripts/ci/lint-api-restartsec-parity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/lint-deploy-systemd-authority.sh"
J2="configs/ansible/roles/archival-node/templates/systemd/stellarindex-api.service.j2"
REF="deploy/systemd/stellarindex-api.service"

pass=0
fail=0

expect_no_diverged() {
  local name="$1"
  OUT="$(bash "$GATE" 2>&1)"
  if grep -q 'DIVERGED:.*stellarindex-api.service.*RestartSec' <<<"$OUT"; then
    echo "FAIL: $name — gate reported an undeclared RestartSec divergence" >&2
    printf '%s\n' "$OUT" | grep 'RestartSec' | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# The values the reference copy's own comment says to keep in sync.
ref_val=$(grep -m1 '^RestartSec=' "$REF" | cut -d= -f2)
j2_val=$(grep -m1 '^RestartSec=' "$J2" | cut -d= -f2)

if [ "$ref_val" != "5s" ]; then
  echo "FAIL: reference copy RestartSec is '$ref_val', expected 5s (its own documented rationale)" >&2
  fail=$((fail + 1))
else
  echo "ok: reference copy keeps the documented 5s cooldown"
  pass=$((pass + 1))
fi

if [ "$j2_val" != "5s" ]; then
  echo "FAIL: deployed template RestartSec is '$j2_val', expected 5s to match the reference" >&2
  fail=$((fail + 1))
else
  echo "ok: deployed template matches the reference at 5s"
  pass=$((pass + 1))
fi

expect_no_diverged 'authority gate reports no undeclared RestartSec divergence for stellarindex-api.service'

echo
echo "lint-api-restartsec-parity-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
