#!/usr/bin/env bash
# Exit 0 when a commit range touches a surface that requires the Docker-backed
# integration suite; exit 1 when the portable and Linux verification lanes are
# sufficient. This policy is shared by prepush.sh and its fixtures.
#
# CI runs the integration shards UNCONDITIONALLY (ci.yml's
# integration-test-shard matrix has no path filter). This list is therefore an
# optimisation for the local lane alone, and it is only sound while it stays a
# superset of every path that can change what the suite observes. When it is
# narrower, a change lands locally green and CI red — which is exactly what
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
    migrations/*|test/fixtures/*|test/integration/*|test/harness/*|internal/storage/*|internal/pipeline/*|internal/projector/*|internal/dispatcher/*|internal/sources/*|internal/api/*)
      echo "integration-policy: required by $changed"
      exit 0
      ;;
  esac
done < <(git diff --name-only "$1"..."$2")

echo "integration-policy: not required"
exit 1
