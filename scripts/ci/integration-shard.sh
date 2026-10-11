#!/usr/bin/env bash
# integration-shard.sh - run ONE deterministic slice of the Docker-backed integration suite
# (ci.yml `integration-test` matrix). The suite is serial and slow, so it is split across runners,
# not inside the process (tests share container fixtures and are not t.Parallel-safe).
#
# The sorted `go test -list '^Test'` output is deterministic, and line i goes to shard (i mod
# SHARD_COUNT), so shards partition the suite (integration-shard-test.sh pins that).
# Shard 0 also runs the non-sharded packages, derived from the Makefile (`make
# print-int-test-pkgs`) so a package added there cannot be run by no shard.
#
# Fail-closed: an empty shard or listing, a bad index, or fewer tests than shards exits non-zero.
#
# Usage: scripts/ci/integration-shard.sh SHARD_INDEX SHARD_COUNT
#   SHARD_INDEX in [0, SHARD_COUNT), SHARD_COUNT >= 1.
# Env (tests / local use):
#   INTEGRATION_SHARD_LIST_FILE  read test names from this file instead of
#                                `go test -list` (one name per line).
#   INTEGRATION_SHARD_DRY_RUN=1  print the shard's test names (stdout) and
#                                the -run regex (stderr); do not run go test.
#   INTEGRATION_SHARD_TIMEOUT    go test -timeout for the slice (default 15m:
#                                a quarter of the serial suite is ~5 min on a
#                                CI runner, so 3x headroom).
#   INTEGRATION_SHARD_MAKEFILE   makefile to read INT_TEST_PKGS from (default
#                                `Makefile`); the self-test points it at
#                                fixtures to prove the derivation tracks.
set -euo pipefail

cd "$(dirname "$0")/../.."

SHARDED_PKG="./test/integration/..."
TIMEOUT="${INTEGRATION_SHARD_TIMEOUT:-15m}"
MAKEFILE="${INTEGRATION_SHARD_MAKEFILE:-Makefile}"

die() { echo "integration-shard: FAIL — $*" >&2; exit 1; }

[ $# -eq 2 ] || die "usage: $0 SHARD_INDEX SHARD_COUNT (got $# args)"
idx="$1"; count="$2"
[[ "$idx" =~ ^[0-9]+$ ]] || die "SHARD_INDEX must be a non-negative integer, got '$idx'"
[[ "$count" =~ ^[0-9]+$ ]] || die "SHARD_COUNT must be a positive integer, got '$count'"
[ "$count" -ge 1 ] || die "SHARD_COUNT must be >= 1, got $count"
[ "$idx" -lt "$count" ] || die "SHARD_INDEX $idx out of range for SHARD_COUNT $count"

# Shard-0-only packages: INT_TEST_PKGS minus SHARDED_PKG. Fail closed on anything odd, since
# the shard would otherwise run a different suite than the Makefile target.
command -v make >/dev/null || die "make not on PATH — cannot derive INT_TEST_PKGS from $MAKEFILE"
[ -r "$MAKEFILE" ] || die "makefile not readable: $MAKEFILE"
int_pkgs="$(make -s -f "$MAKEFILE" print-int-test-pkgs)" ||
  die "'make -f $MAKEFILE print-int-test-pkgs' failed — is the target still there?"
[ -n "${int_pkgs//[[:space:]]/}" ] || die "INT_TEST_PKGS resolved empty from $MAKEFILE"
read -r -a int_pkgs_arr <<<"$int_pkgs"   # split on whitespace, no globbing
EXTRA_PKGS=()
sharded_listed=0
for pkg in "${int_pkgs_arr[@]}"; do
  if [ "$pkg" = "$SHARDED_PKG" ]; then sharded_listed=1; continue; fi
  EXTRA_PKGS+=("$pkg")
done
[ "$sharded_listed" -eq 1 ] ||
  die "INT_TEST_PKGS ($int_pkgs) does not list $SHARDED_PKG — this script shards that package by test name, so the Makefile target and the shards have diverged"

# Full sorted listing (identical on every shard).
if [ -n "${INTEGRATION_SHARD_LIST_FILE:-}" ]; then
  [ -r "$INTEGRATION_SHARD_LIST_FILE" ] || die "list file not readable: $INTEGRATION_SHARD_LIST_FILE"
  raw="$(cat "$INTEGRATION_SHARD_LIST_FILE")"
else
  raw="$(go test -tags=integration -list '^Test' "$SHARDED_PKG")"
fi
# Keep only test identifiers (`go test -list` also prints ok/? trailers, which contain
# whitespace). `[^[:space:]]` not ASCII, since Go identifiers admit Unicode letters. LC_ALL=C
# keeps the sort byte-stable so every shard agrees on line numbers.
all="$(printf '%s\n' "$raw" | grep -E '^Test[^[:space:]]*$' | LC_ALL=C sort -u || true)"
[ -n "$all" ] || die "test listing is empty — nothing to shard (build-tag or listing breakage?)"
total="$(printf '%s\n' "$all" | wc -l | tr -d ' ')"
[ "$total" -ge "$count" ] || die "only $total tests listed but $count shards requested — some shard would be empty"

# This shard's slice: line i -> shard (i mod count).
mine="$(printf '%s\n' "$all" | awk -v n="$count" -v s="$idx" 'NR % n == s')"
[ -n "$mine" ] || die "shard $idx/$count selected zero tests out of $total"
n_mine="$(printf '%s\n' "$mine" | wc -l | tr -d ' ')"
regex="^($(printf '%s\n' "$mine" | paste -sd '|' -))\$"

echo "integration-shard: shard $idx of $count — $n_mine of $total tests in $SHARDED_PKG" >&2
echo "integration-shard: shard-0-only packages from $MAKEFILE: ${EXTRA_PKGS[*]:-(none)}" >&2
if [ "${INTEGRATION_SHARD_DRY_RUN:-0}" = "1" ]; then
  echo "integration-shard: -run $regex" >&2
  printf '%s\n' "$mine"
  exit 0
fi

# Same flags as the Makefile's test-integration; -v gives per-test timing for later weighted splits.
go test -tags=integration -v -timeout "$TIMEOUT" -run "$regex" "$SHARDED_PKG"

if [ "$idx" -eq 0 ] && [ "${#EXTRA_PKGS[@]}" -gt 0 ]; then
  echo "integration-shard: shard 0 also runs the non-sharded packages: ${EXTRA_PKGS[*]}" >&2
  go test -tags=integration -timeout "$TIMEOUT" "${EXTRA_PKGS[@]}"
fi
