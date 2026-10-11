#!/usr/bin/env bash
# cache-r1-annotation-test.sh — configs/prometheus/rules.r1/cache.yml's
# stellarindex_redis_replication_broken description must not use
# `{{ $labels.expected }}`: the expr compares two metrics and produces no `expected`
# label, so it renders empty ("fewer than expected (< )."). Use `{{ $value }}` as
# deploy/monitoring/rules/cache.yml does.
#
# Pins that the dead `$labels.expected` reference stays gone.
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
