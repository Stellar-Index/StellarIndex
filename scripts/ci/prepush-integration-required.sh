#!/usr/bin/env bash
# Exit 0 when a commit range touches a surface that requires the Docker-backed
# integration suite; exit 1 when the portable and Linux verification lanes are
# sufficient. This policy is shared by prepush.sh and its fixtures.
#
# CI does NOT run the integration shards unconditionally: ci.yml's
# integration-test-shard steps are gated on
# `needs.preflight.outputs.integration == 'true'`, i.e. on the preflight path
# filter (mirrored in scripts/ci/check-change-class.sh). So this list is not a
# local-only optimisation with a CI backstop — when BOTH classifiers omit a
# path, the suite runs in no lane at all for a diff confined to it. That is
# T424/T449: scripts/ops/fx-history-backfill's `//go:build integration` INV-3
# regression (operator fx_quotes corrections must carry a positive derive
# generation) compiled in the unconditional compile gate and executed nowhere.
# This list must therefore stay a superset of the Makefile's INT_TEST_PKGS
# directories as well as of every path that can change what the suite
# observes. When it is narrower, a change lands locally green and CI red — or,
# for an INT_TEST_PKGS-only directory, green everywhere while its money
# invariant goes unchecked. The CI-red shape is exactly what
# happened on 2026-09-04: a commit touching only internal/api/v1 changed
# /v1/history's read to fold both stored market directions, and
# test/integration/coverage_floor_test.go pinned the behaviour it replaced.
# That suite stands up the real router and drives served surfaces, so a
# handler change reaches it; the filter did not know that and skipped the
# shards. Widening a path here costs local minutes. Omitting one costs a red
# main.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 BASE HEAD" >&2
  exit 2
fi

git cat-file -e "$1^{commit}" 2>/dev/null || { echo "integration-policy: unknown base $1" >&2; exit 2; }
git cat-file -e "$2^{commit}" 2>/dev/null || { echo "integration-policy: unknown head $2" >&2; exit 2; }

while IFS= read -r changed; do
  case "$changed" in
    migrations/*|test/fixtures/*|test/integration/*|test/harness/*|internal/storage/*|internal/pipeline/*|internal/projector/*|internal/dispatcher/*|internal/sources/*|internal/api/*|internal/ops/archive/*|cmd/stellarindex-ops/*|scripts/ops/*)
      echo "integration-policy: required by $changed"
      exit 0
      ;;
  esac
done < <(git diff --name-only "$1"..."$2")

echo "integration-policy: not required"
exit 1
