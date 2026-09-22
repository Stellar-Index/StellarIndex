#!/usr/bin/env bash
# check-change-class.sh — the CI path-filter decision core
# (mirrors ci.yml's `preflight` job's path-filter rules).
#
# WHY THIS EXISTS SEPARATELY FROM THE dorny/paths-filter STEP IN ci.yml.
# The filter that decides whether integration-test-shard/build/test/vuln/
# fuzz-smoke/web-explorer/web-status/ansible-check RUN AT ALL lives inside
# a third-party action's JS glob engine, which cannot be exercised offline
# without Docker/`act` — exactly the thing this repo's fixers are told
# never to reach for. So the RULE (which prefixes/extensions belong to
# which change class) is restated here, in five-line grep functions any
# CI run or laptop can execute in milliseconds, and `preflight` in ci.yml
# runs BOTH this script and the paths-filter action against the same diff
# and fails loudly if they disagree — so a change to one that is not
# mirrored in the other is caught the same day, not discovered as a
# silently-wedged or silently-skipped job weeks later.
#
# THE STAKES OF GETTING THIS WRONG IN EITHER DIRECTION. Too narrow and a
# storage/pipeline/sources/api change ships without the integration shards
# that are its only real coverage (the exact shape of the 2026-07-01
# sponsors/markets/blend regressions the shard matrix was built to catch).
# Too broad and every docs/scripts-only change pays a 20-minute Docker
# round-trip for zero additional signal — the problem this filter exists
# to remove per the plan's measured numbers.
#
# WHY THE integration CLASS IS A SUPERSET OF INT_TEST_PKGS. The Makefile's
# INT_TEST_PKGS is the list of packages the Docker suite BUILDS AND RUNS;
# this class decides whether that suite runs AT ALL for a given diff. If a
# package is in the first list but its directory is in neither classifier,
# its `//go:build integration` tests compile in the unconditional compile
# gate and are EXECUTED BY NOTHING for a change confined to that package —
# which is how scripts/ops/fx-history-backfill's INV-3 money-invariant
# regression (operator fx_quotes corrections must carry a positive derive
# generation) sat in no executing gate (T424/T449), and cmd/stellarindex-ops
# (F-1334) and internal/ops/archive (W6-tst-1) the same. So every
# INT_TEST_PKGS directory belongs here, and
# test/controlwiring/integration_pkgs_trigger_evidence_test.go fails if one
# is missing from this regex, from ci.yml's filter, or from
# prepush-integration-required.sh.
#
# Classes (must stay identical in substance to the `filters:` block in
# .github/workflows/ci.yml's preflight job):
#   integration  — internal/storage/**, internal/pipeline/**,
#                  internal/sources/**, internal/api/**,
#                  internal/ops/archive/**, cmd/stellarindex-ops/**,
#                  migrations/**, scripts/ops/**, test/integration/**,
#                  test/harness/**, go.mod
#   go           — any *.go file, go.mod, go.sum, openapi/** (Go spec-parity
#                  tests read the spec directly)
#   web          — web/**, openapi/**
#   ansible      — configs/ansible/**
#
# Usage:
#   git diff --name-only "$BASE" "$HEAD" | check-change-class.sh <class>
#   check-change-class.sh <class> <file> [<file> ...]
#
# Exit 0  — at least one changed file belongs to <class> (job should RUN).
# Exit 1  — no changed file belongs to <class> (job may be SKIPPED).
# Exit 2  — usage error (unknown class, or no files supplied at all — an
#           empty diff naming zero files is a caller bug, not "skip
#           everything", and must not be read as a quiet all-clear).
set -euo pipefail

class_integration() {
  grep -E '^(internal/(storage|pipeline|sources|api|ops/archive)/|cmd/stellarindex-ops/|migrations/|scripts/ops/|test/(integration|harness)/)|^go\.mod$'
}

class_go() {
  # openapi/** is a Go-test dependency (internal/api/v1's
  # handler_spec_fields_test.go and its spec-parity siblings read the spec
  # file directly), not just a web one — a diff confined to it must still
  # trigger the go-test job. Mirrored in ci.yml's preflight `go` filter.
  grep -E '(^|/)[^/]+\.go$|^go\.mod$|^go\.sum$|^openapi/'
}

class_web() {
  grep -E '^(web/|openapi/)'
}

class_ansible() {
  grep -E '^configs/ansible/'
}

usage() {
  echo "usage: check-change-class.sh <integration|go|web|ansible> [<file> ...]" >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
CLASS="$1"
shift

case "$CLASS" in
  integration | go | web | ansible) ;;
  *) usage ;;
esac

# Always fully drain stdin, whether or not $# supplied files on argv, so
# an upstream `git diff | check-change-class.sh ...` never sees a broken
# pipe (lint-shell-sigpipe.sh's exact class of defect) when argv already
# satisfied the match and this script could otherwise exit before
# reading the rest of the producer's output.
STDIN_FILES=""
if [ ! -t 0 ]; then
  STDIN_FILES="$(cat)"
fi

ALL_FILES=""
if [ "$#" -gt 0 ]; then
  ALL_FILES="$(printf '%s\n' "$@")"
fi
if [ -n "$STDIN_FILES" ]; then
  ALL_FILES="${ALL_FILES}${ALL_FILES:+$'\n'}${STDIN_FILES}"
fi

if [ -z "$ALL_FILES" ]; then
  echo "check-change-class: FAIL — no changed files supplied; an empty diff must not be read as a class decision" >&2
  exit 2
fi

case "$CLASS" in
  integration) printf '%s\n' "$ALL_FILES" | class_integration >/dev/null && exit 0 ;;
  go) printf '%s\n' "$ALL_FILES" | class_go >/dev/null && exit 0 ;;
  web) printf '%s\n' "$ALL_FILES" | class_web >/dev/null && exit 0 ;;
  ansible) printf '%s\n' "$ALL_FILES" | class_ansible >/dev/null && exit 0 ;;
esac
exit 1
