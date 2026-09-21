#!/usr/bin/env bash
#
# changelog-canonical-citation-test.sh — regression guard for RSWP-074.
#
# CHANGELOG.md's "canonical URL" cluster (explorer home page + top-level
# page canonicals) used to cite the work as bare "#1094/#1095/#1097" and
# "#1167". Those numbers were never real GitHub PRs for this repo — they
# were flavor text — and the issue tracker has since allocated real, live
# issues under those same numbers for unrelated findings. A reader
# following the citation lands on the wrong issue. The citations were
# replaced with the actual commit hashes that shipped the canonical-URL
# work, which cannot collide with the issue/PR numbering space.
#
# Run: bash scripts/ci/changelog-canonical-citation-test.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHANGELOG="$ROOT/CHANGELOG.md"

fail=0

if [ ! -f "$CHANGELOG" ]; then
  echo "changelog-canonical-citation-test: FAIL — $CHANGELOG not found" >&2
  exit 1
fi

# The stale citations must be gone from the canonical-URL cluster.
for stale in '#1094/#1095/#1097' '#1094-1097' 'Companion to #1167'; do
  if grep -qF -- "$stale" "$CHANGELOG"; then
    echo "changelog-canonical-citation-test: FAIL — stale citation '$stale' still present in CHANGELOG.md (RSWP-074: it now resolves to an unrelated live issue, not the canonical-URL PR)" >&2
    fail=1
  fi
done

# The replacement commit hashes — which actually shipped the
# canonical-URL work — must be present instead.
for sha in 92d8ccda1 b66321fa0 176c8b2e7 08465cb44; do
  if ! grep -qF -- "$sha" "$CHANGELOG"; then
    echo "changelog-canonical-citation-test: FAIL — expected commit citation '$sha' missing from CHANGELOG.md" >&2
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "changelog-canonical-citation-test: OK — canonical-URL cluster cites commit hashes, not the colliding issue numbers"
