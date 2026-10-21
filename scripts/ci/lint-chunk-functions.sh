#!/usr/bin/env bash
# lint-chunk-functions.sh — no tracked code or script may call the two
# TimescaleDB chunk functions that destroy data with no undo.
#
# WHY. On r1 (2026-10-21) merge_chunks() on compressed chunks left 4 chunks
# unreadable. count(*) reads chunk metadata, so the check run after it said
# nothing was wrong; a PITR copy was the only way back. drop_chunks() deletes
# whole chunks with no undo either. Retention belongs in add_retention_policy
# (pinned by retention_policy_test.go), repairs in a reviewed procedure.
#
# A line may quote the call as a warning when it carries `DO NOT RUN`.
# Not scanned: markdown (prose); tests (throwaway databases); and the two
# migrations already applied, which are immutable (lint-migration-immutability).
# LINT_ROOT is the test seam.
set -euo pipefail
root="${LINT_ROOT:-$(git rev-parse --show-toplevel)}"
cd "$root"
self='scripts/ci/lint-chunk-functions'
hits=$(git ls-files -z -- '*.go' '*.sql' '*.sh' '*.j2' '*.yml' '*.yaml' '*.py' \
  | grep -zvE "^($self|test/|migrations/0062_widen_hot_chunk_intervals\.up\.sql$|migrations/0207_price_source_contributions_window_not_null\.up\.sql$)|_test\.go$" \
  | xargs -0 -r grep -nE '\b(merge_chunks|drop_chunks)[[:space:]]*\(' 2>/dev/null \
  | grep -v 'DO NOT RUN' || true)
if [ -n "$hits" ]; then
  echo "lint-chunk-functions: merge_chunks()/drop_chunks() destroy chunks with no undo:" >&2
  echo "$hits" >&2
  echo "Use add_retention_policy for retention; for a repair follow docs/operations/runbooks/timescale.md#stellarindex_pg_compressed_chunks_unreadable. Quote a warning with 'DO NOT RUN' on the line." >&2
  exit 1
fi
echo "lint-chunk-functions: OK"
