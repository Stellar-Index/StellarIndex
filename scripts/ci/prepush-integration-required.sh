#!/usr/bin/env bash
# Exit 0 when a commit range touches a surface that requires the Docker-backed
# integration suite; exit 1 when the portable and Linux verification lanes are
# sufficient. This policy is shared by prepush.sh and its fixtures.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 BASE HEAD" >&2
  exit 2
fi

git cat-file -e "$1^{commit}" 2>/dev/null || { echo "integration-policy: unknown base $1" >&2; exit 2; }
git cat-file -e "$2^{commit}" 2>/dev/null || { echo "integration-policy: unknown head $2" >&2; exit 2; }

while IFS= read -r changed; do
  case "$changed" in
    migrations/*|test/fixtures/*|test/integration/*|test/harness/*|internal/storage/*|internal/pipeline/*|internal/projector/*|internal/dispatcher/*|internal/sources/*)
      echo "integration-policy: required by $changed"
      exit 0
      ;;
  esac
done < <(git diff --name-only "$1"..."$2")

echo "integration-policy: not required"
exit 1
