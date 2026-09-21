#!/usr/bin/env bash
# sla-probe-concurrency-doc-test.sh — regression coverage for T487.
#
# The documented SLA_PROBE_CONCURRENCY default in README.md and the
# placeholder install.sh writes into /etc/default/stellarindex-healthchecks
# must match the wrapper's actual default (sla-probe.sh's
# `${SLA_PROBE_CONCURRENCY:-N}`), not a stale value left over from
# before the default was lowered.
#
# Run: bash configs/healthchecks/sla-probe-concurrency-doc-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
HC_DIR="configs/healthchecks"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

ACTUAL_DEFAULT="$(grep -o 'SLA_PROBE_CONCURRENCY:-[0-9]*' "$HC_DIR/sla-probe.sh" | grep -o '[0-9]*$')"

if [ -z "$ACTUAL_DEFAULT" ]; then
  bad "T487: could not extract SLA_PROBE_CONCURRENCY default from sla-probe.sh"
else
  ok "T487: sla-probe.sh's SLA_PROBE_CONCURRENCY default is $ACTUAL_DEFAULT"

  if grep -q "SLA_PROBE_CONCURRENCY=${ACTUAL_DEFAULT}\b" "$HC_DIR/README.md"; then
    ok "T487: README.md documents SLA_PROBE_CONCURRENCY=$ACTUAL_DEFAULT (matches the wrapper default)"
  else
    bad "T487: README.md's SLA_PROBE_CONCURRENCY placeholder does not match the wrapper default ($ACTUAL_DEFAULT)"
  fi

  if grep -q "# SLA_PROBE_CONCURRENCY=${ACTUAL_DEFAULT}\$" "$HC_DIR/install.sh"; then
    ok "T487: install.sh's env-file placeholder documents SLA_PROBE_CONCURRENCY=$ACTUAL_DEFAULT"
  else
    bad "T487: install.sh's env-file placeholder does not match the wrapper default ($ACTUAL_DEFAULT)"
  fi
fi

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
