#!/usr/bin/env bash
# Exit 0 when a commit range touches a surface that requires the Docker-backed
# integration suite; exit 1 when the portable and Linux verification lanes are
# sufficient. Shared by prepush.sh and its fixtures.
#
# CI does not run the integration shards unconditionally: ci.yml gates them on
# `needs.preflight.outputs.integration == 'true'` (the preflight path filter,
# mirrored in scripts/ci/check-change-class.sh). So when BOTH classifiers omit
# a path, the suite runs in no lane for a diff confined to it: its
# `//go:build integration` tests compile in the unconditional gate and execute
# nowhere.
# This list must stay a superset of the Makefile's INT_TEST_PKGS directories
# and of every path that can change what the suite observes (it drives the real
# router, so a handler-only change in internal/api/v1 must run the shards).
# Too narrow means green locally and red in CI, or green everywhere with a
# money invariant unchecked. Widening costs local minutes; omitting costs a red
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
    # internal/* as a whole, not an enumerated subset: `go list -deps -test
    # -tags integration ./test/integration/... ./test/harness/...` pulls in
    # 44 of the 54 top-level internal/ directories. Mirrored in ci.yml and
    # check-change-class.sh; test/controlwiring/integration_pkgs_trigger_evidence_test.go
    # checks all three against the same derived closure.
    migrations/*|test/fixtures/*|test/integration/*|test/harness/*|internal/*|cmd/stellarindex-ops/*|scripts/ops/*|go.mod)
      echo "integration-policy: required by $changed"
      exit 0
      ;;
  esac
done < <(git diff --name-only "$1"..."$2")

echo "integration-policy: not required"
exit 1
