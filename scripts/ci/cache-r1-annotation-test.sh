#!/usr/bin/env bash
# cache-r1-annotation-test.sh — regression guard for Q269.
#
# configs/prometheus/rules.r1/cache.yml's stellarindex_redis_replication_broken
# alert used `{{ $labels.expected }}` in its description, but the rule's
# expr (`redis_connected_slaves < on(instance) redis_expected_slaves`)
# never produces a label named `expected` — it compares against a
# second metric, not a threshold label. The interpolation always
# rendered as an empty string, so a firing page read "fewer than
# expected (< )." The live multi-host twin (deploy/monitoring/rules/
# cache.yml) already uses `{{ $value }}` for the analogous alert; the
# r1 copy is INERT (redis_expected_slaves has no producer on r1) so the
# defect never paged, but the annotation text was still wrong.
#
# This test pins that the dead `$labels.expected` reference is gone.
#
# Run: bash scripts/ci/cache-r1-annotation-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
FILE="configs/prometheus/rules.r1/cache.yml"

pass=0
fail=0

# shellcheck disable=SC2016  # literal $labels.expected must NOT expand — it is the string we grep for
if grep -q '\$labels\.expected' "$FILE"; then
  echo "FAIL: $FILE still interpolates the undefined label \$labels.expected"
  fail=$((fail + 1))
else
  echo "PASS: $FILE does not reference the undefined \$labels.expected"
  pass=$((pass + 1))
fi

echo "cache-r1-annotation-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
