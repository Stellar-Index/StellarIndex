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
# path, the suite runs in no lane at all for a diff confined to it: its
# `//go:build integration` tests compile in the unconditional gate and
# execute nowhere.
# This list must therefore stay a superset of the Makefile's INT_TEST_PKGS
# directories as well as of every path that can change what the suite
# observes. When it is narrower, a change lands locally green and CI red — or,
# for an INT_TEST_PKGS-only directory, green everywhere while its money
# invariant goes unchecked. The suite stands up the real router and drives
# served surfaces, so a handler-only change (internal/api/v1) reaches it
# and must run the shards. Widening a path here costs local minutes. Omitting one costs a red
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
    # 44 of the 54 top-level internal/ directories, most with no obvious
    # "integration" flavour. Mirrored in ci.yml and check-change-class.sh;
    # test/controlwiring/integration_pkgs_trigger_evidence_test.go checks
    # all three against the same derived closure.
    migrations/*|test/fixtures/*|test/integration/*|test/harness/*|internal/*|cmd/stellarindex-ops/*|scripts/ops/*|go.mod)
      echo "integration-policy: required by $changed"
      exit 0
      ;;
  esac
done < <(git diff --name-only "$1"..."$2")

echo "integration-policy: not required"
exit 1
