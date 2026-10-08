#!/usr/bin/env bash
#
# explorer-openapi-check.sh — assert the OpenAPI contract shipped in the
# static export is present and byte-identical to the canonical spec. The
# docs page links /openapi/stellar-index.v1.yaml by absolute path, but the
# file only reaches `out/` via a `build` lifecycle script copying it into
# `public/openapi/` (web/explorer/package.json); whether a package manager
# runs that hook is a config detail, not a repo invariant, so a build in
# which it silently didn't run must fail loudly here rather than ship a
# dead link.
#
# Usage: scripts/ci/explorer-openapi-check.sh [out-dir] [spec-path]
# Both paths may be relative to the caller's cwd. SPEC defaults to the
# canonical spec resolved relative to this script's own location, so the
# default works whether invoked from the repo root or from web/explorer
# (the way package.json's postbuild:openapi does).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

OUT="${1:-web/explorer/out}"
SPEC="${2:-$REPO_ROOT/openapi/stellar-index.v1.yaml}"
SHIPPED="$OUT/openapi/stellar-index.v1.yaml"

if [ ! -d "$OUT" ]; then
  echo "explorer-openapi-check: '$OUT' not found — build the explorer first." >&2
  exit 2
fi

if [ ! -f "$SPEC" ]; then
  echo "explorer-openapi-check: canonical spec '$SPEC' not found." >&2
  exit 2
fi

if [ ! -f "$SHIPPED" ]; then
  echo "::error::$SHIPPED missing — the docs page links it by absolute path but the build copy did not run" >&2
  exit 1
fi

if ! cmp -s "$SPEC" "$SHIPPED"; then
  echo "::error::$SHIPPED does not match $SPEC — shipped contract is stale" >&2
  exit 1
fi

echo "explorer-openapi-check: $SHIPPED matches $SPEC."
