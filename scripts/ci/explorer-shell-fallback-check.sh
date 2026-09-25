#!/usr/bin/env bash
#
# explorer-shell-fallback-check.sh — fail the explorer build if a CF Pages
# shell-fallback Function's target shell isn't actually in the static
# export.
#
# T329 (audit-2026-09-18): every functions/**/[[path]].js that calls
# shellFallback(context, '<path>') serves out<path>index.html for any
# slug outside the pre-rendered set (accounts, assets, contracts, issuers,
# ledgers, lending, markets, transactions, sources, external/assets,
# embed/{asset,currency,pair}, insights/{creators,sponsors}). Nothing
# checked that the shell artefact actually exists — a route whose
# generateStaticParams stopped emitting the 'shell' sentinel would 404 or
# 503 on every long-tail slug with the build going green.
#
# The expected list is DERIVED from the Functions themselves (not
# hardcoded here) so a newly added fallback route is covered without a
# second edit.
#
# Usage: scripts/ci/explorer-shell-fallback-check.sh [out-dir] [functions-dir]
set -euo pipefail

OUT="${1:-web/explorer/out}"
FUNCTIONS="${2:-web/explorer/functions}"

if [ ! -d "$OUT" ]; then
  echo "explorer-shell-fallback-check: '$OUT' not found — build the explorer first (pnpm build)." >&2
  exit 2
fi
if [ ! -d "$FUNCTIONS" ]; then
  echo "explorer-shell-fallback-check: '$FUNCTIONS' not found." >&2
  exit 2
fi

missing=0
checked=0
while IFS= read -r shell_path; do
  [ -n "$shell_path" ] || continue
  checked=$((checked + 1))
  artefact="${OUT}${shell_path}index.html"
  if [ -f "$artefact" ]; then
    echo "explorer-shell-fallback-check: OK  ${shell_path} -> ${artefact}"
  else
    echo "::error::missing shell artefact for fallback route '${shell_path}' (expected ${artefact})" >&2
    missing=$((missing + 1))
  fi
done < <(
  grep -rhoE "shellFallback\(context, '[^']+'\)" "$FUNCTIONS" \
    | sed -E "s/.*'([^']+)'.*/\1/" \
    | sort -u
)

if [ "$checked" -eq 0 ]; then
  echo "::error::explorer-shell-fallback-check found 0 shellFallback(...) call(s) under $FUNCTIONS — the discovery pattern broke or the Functions moved. Refusing to pass vacuously." >&2
  exit 1
fi

echo "explorer-shell-fallback-check: checked ${checked} fallback route(s), ${missing} missing."
if [ "$missing" -ne 0 ]; then
  exit 1
fi
