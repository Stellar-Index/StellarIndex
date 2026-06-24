#!/usr/bin/env bash
#
# explorer-file-budget.sh — fail the explorer build if the static export
# approaches Cloudflare Pages' hard limit of 20,000 files per deployment.
#
# Why this exists (SEO plan R1, confirmed by Spike A 2026-06-24): Next static
# export emits ~2 files per page (HTML + RSC .txt). Pre-rendering the curated
# entity sets (richlist accounts, active contracts, …) pushes the file count
# toward the cap; exceeding it makes the CF Pages deploy fail OPAQUELY. This
# turns that into a clear, early build failure with a pointer to the fix
# (lower the pre-render caps in generateStaticParams, or shard Pages projects).
#
# Usage: scripts/ci/explorer-file-budget.sh [out-dir]
#   env CF_PAGES_FILE_LIMIT  (default 20000) — the hard CF cap
#   env CF_PAGES_FILE_MARGIN (default 1500)  — headroom; fail before the cap
set -euo pipefail

OUT="${1:-web/explorer/out}"
LIMIT="${CF_PAGES_FILE_LIMIT:-20000}"
MARGIN="${CF_PAGES_FILE_MARGIN:-1500}"

if [ ! -d "$OUT" ]; then
  echo "explorer-file-budget: '$OUT' not found — build the explorer first (pnpm build)." >&2
  exit 2
fi

count=$(find "$OUT" -type f | wc -l | tr -d ' ')
ceiling=$((LIMIT - MARGIN))

echo "explorer static-export files: ${count}  (ceiling ${ceiling}, CF hard cap ${LIMIT})"

if [ "$count" -gt "$ceiling" ]; then
  echo "::error::explorer file count ${count} exceeds the ${ceiling} ceiling." >&2
  echo "Cloudflare Pages rejects deployments over ${LIMIT} files. Reduce the" >&2
  echo "curated pre-render caps (SEO plan R1/D6 — generateStaticParams for" >&2
  echo "accounts/contracts/ledgers) or shard across Pages projects." >&2
  exit 1
fi

echo "explorer-file-budget: OK ($((ceiling - count)) files of headroom)."
