#!/usr/bin/env bash
# integration-shard.sh — run ONE deterministic slice of the Docker-backed
# integration suite (ci.yml `integration-test` matrix, 2026-08-29).
#
# Why: main CI's wall-clock was ~23 min and the single long pole was the
# `integration tests (Docker)` job at ~1230 s (20.5 min) — every other job
# finishes in <= 6 min. test/integration has ~158 top-level tests, zero
# t.Parallel(), and each spins its own testcontainers, so `make
# test-integration` runs them strictly serially. The Makefile's 35m-deadline
# history comment already said it: "if this needs raising a third time,
# split the suite". This is that split — across runners, not inside the
# process (no t.Parallel(): the tests share container-backed fixtures and
# were never written for it).
#
# How: the test list comes from `go test -list '^Test'` (compile only, no
# Docker), is sorted, and line i goes to shard (i mod SHARD_COUNT). Every
# shard derives the same sorted list, so the shards PARTITION the suite by
# construction: each test lands in exactly one shard, and none is dropped.
# scripts/ci/integration-shard-test.sh pins that property on a stub list.
# The slice then runs with the SAME go test flags as `make test-integration`
# (-tags=integration; only the deadline shrinks with the slice).
#
# Shard 0 additionally runs the non-sharded packages `make test-integration`
# covers (INT_TEST_PKGS: cmd/stellarindex-ops, internal/ops/archive — they
# finish in seconds), so the union of all shards == the Makefile target.
#
# Fail-closed: an empty shard, an empty listing, a bad index, or a listing
# that yields fewer tests than shards all exit non-zero — a shard that ran
# nothing must never report green.
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
#                                CI runner, so 3x headroom — the 20m→35m
#                                history in the Makefile is what happens with
#                                no headroom).
set -euo pipefail

cd "$(dirname "$0")/../.."

SHARDED_PKG="./test/integration/..."
# Keep in lockstep with INT_TEST_PKGS in the Makefile (minus SHARDED_PKG).
EXTRA_PKGS="./cmd/stellarindex-ops/... ./internal/ops/archive/..."
TIMEOUT="${INTEGRATION_SHARD_TIMEOUT:-15m}"

die() { echo "integration-shard: FAIL — $*" >&2; exit 1; }

[ $# -eq 2 ] || die "usage: $0 SHARD_INDEX SHARD_COUNT (got $# args)"
idx="$1"; count="$2"
[[ "$idx" =~ ^[0-9]+$ ]] || die "SHARD_INDEX must be a non-negative integer, got '$idx'"
[[ "$count" =~ ^[0-9]+$ ]] || die "SHARD_COUNT must be a positive integer, got '$count'"
[ "$count" -ge 1 ] || die "SHARD_COUNT must be >= 1, got $count"
[ "$idx" -lt "$count" ] || die "SHARD_INDEX $idx out of range for SHARD_COUNT $count"

# ── full sorted listing (identical on every shard) ──────────────────────
if [ -n "${INTEGRATION_SHARD_LIST_FILE:-}" ]; then
  [ -r "$INTEGRATION_SHARD_LIST_FILE" ] || die "list file not readable: $INTEGRATION_SHARD_LIST_FILE"
  raw="$(cat "$INTEGRATION_SHARD_LIST_FILE")"
else
  raw="$(go test -tags=integration -list '^Test' "$SHARDED_PKG")"
fi
# `go test -list` also prints "ok <pkg> <time>" trailer lines; keep only
# test identifiers. LC_ALL=C makes the sort byte-order-stable across
# runners so every shard agrees on line numbers.
all="$(printf '%s\n' "$raw" | grep -E '^Test[A-Za-z0-9_]*$' | LC_ALL=C sort -u || true)"
[ -n "$all" ] || die "test listing is empty — nothing to shard (build-tag or listing breakage?)"
total="$(printf '%s\n' "$all" | wc -l | tr -d ' ')"
[ "$total" -ge "$count" ] || die "only $total tests listed but $count shards requested — some shard would be empty"

# ── this shard's slice: line i -> shard (i mod count) ───────────────────
mine="$(printf '%s\n' "$all" | awk -v n="$count" -v s="$idx" 'NR % n == s')"
[ -n "$mine" ] || die "shard $idx/$count selected zero tests out of $total"
n_mine="$(printf '%s\n' "$mine" | wc -l | tr -d ' ')"
regex="^($(printf '%s\n' "$mine" | paste -sd '|' -))\$"

echo "integration-shard: shard $idx of $count — $n_mine of $total tests in $SHARDED_PKG" >&2
if [ "${INTEGRATION_SHARD_DRY_RUN:-0}" = "1" ]; then
  echo "integration-shard: -run $regex" >&2
  printf '%s\n' "$mine"
  exit 0
fi

# Same invocation as the Makefile's test-integration target, narrowed to
# this shard's tests; -timeout is the per-slice deadline (see header).
go test -tags=integration -timeout "$TIMEOUT" -run "$regex" "$SHARDED_PKG"

if [ "$idx" -eq 0 ]; then
  echo "integration-shard: shard 0 also runs the non-sharded packages: $EXTRA_PKGS" >&2
  # shellcheck disable=SC2086  # intentional word-split of the package list
  go test -tags=integration -timeout "$TIMEOUT" $EXTRA_PKGS
fi
