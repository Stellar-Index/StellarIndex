#!/usr/bin/env bash
# Regenerate the rendered API reference from openapi/stellar-atlas.v1.yaml.
#
# Output is a Scalar API reference page: a small static index.html
# that loads @scalar/api-reference from a pinned CDN bundle and
# points it at a spec file copied alongside it.
#
# CI verifies the rendered output is in sync with the spec on every
# PR that touches either side. To regenerate locally:
#
#     make docs-api
#
# No Node install needed — Scalar's standalone bundle is fetched
# at view time from the CDN, so this script only needs `cp` to copy
# the spec next to the index.html.

set -euo pipefail

# CDN-pinned Scalar standalone bundle. Bumping requires updating
# this constant and re-running `make docs-api` so the committed
# index.html records the new version. The standalone bundle is
# self-contained: HTML, CSS, and JS in one URL.
SCALAR_VERSION="1.55.3"

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"

OUT_DIR="docs/reference/api"
mkdir -p "$OUT_DIR"

# Copy the OpenAPI spec next to the rendered HTML. Scalar fetches
# it via the relative URL at view time, so it must live under the
# same CF Pages project root.
cp openapi/stellar-atlas.v1.yaml "$OUT_DIR/stellar-atlas.v1.yaml"

cat > "$OUT_DIR/index.html" <<EOF
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Stellar Atlas — API reference</title>
    <meta
      name="description"
      content="Comprehensive Stellar-network pricing API. REST + SSE endpoints for VWAP / TWAP / OHLC across on-chain DEXes, classic SDEX, and major exchanges."
    />
    <link rel="canonical" href="https://docs.stellaratlas.xyz/" />
    <link rel="icon" type="image/svg+xml" href="/icon.svg" />

    <!-- Open Graph / Twitter card for shareable preview -->
    <meta property="og:type" content="website" />
    <meta property="og:site_name" content="Stellar Atlas — docs" />
    <meta property="og:title" content="Stellar Atlas — API reference" />
    <meta property="og:description" content="Stellar pricing API: VWAP / TWAP / OHLC + SSE. Public, no-auth, REST + streaming." />
    <meta property="og:url" content="https://docs.stellaratlas.xyz/" />
    <meta property="og:image" content="https://docs.stellaratlas.xyz/og.svg" />
    <meta property="og:image:width" content="1200" />
    <meta property="og:image:height" content="630" />
    <meta name="twitter:card" content="summary_large_image" />
    <meta name="twitter:title" content="Stellar Atlas — API reference" />
    <meta name="twitter:description" content="Stellar pricing API: VWAP / TWAP / OHLC + SSE. Public, no-auth, REST + streaming." />
    <meta name="twitter:image" content="https://docs.stellaratlas.xyz/og.svg" />

    <style>
      html, body { margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif; }
      .re-topbar {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 16px;
        padding: 8px 16px;
        background: #0f172a;
        color: #e2e8f0;
        font-size: 13px;
        border-bottom: 1px solid #1e293b;
      }
      .re-topbar a { color: #94a3b8; text-decoration: none; transition: color 0.1s; }
      .re-topbar a:hover { color: #38bdf8; }
      .re-topbar .re-brand { font-weight: 600; color: #e2e8f0; display: flex; align-items: center; gap: 8px; }
      .re-topbar .re-brand svg { width: 18px; height: 18px; }
      .re-topbar .re-links { display: flex; gap: 16px; align-items: center; }
      .re-topbar .re-pulse {
        display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #10b981;
        box-shadow: 0 0 0 2px rgba(16, 185, 129, 0.2);
      }
    </style>
  </head>
  <body>
    <header class="re-topbar">
      <a class="re-brand" href="https://stellaratlas.xyz">
        <svg viewBox="0 0 32 32" fill="none">
          <rect width="32" height="32" rx="6" fill="#0ea5e9"/>
          <path d="M 6 22 L 11 19 L 14 21 L 19 13 L 23 17 L 27 9" stroke="white" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" fill="none"/>
        </svg>
        Stellar Atlas
      </a>
      <nav class="re-links">
        <a href="https://stellaratlas.xyz">Explorer</a>
        <a href="https://stellaratlas.xyz/methodology">Methodology</a>
        <a href="https://stellaratlas.xyz/sdk">Go SDK</a>
        <a href="https://stellaratlas.xyz/changelog">Changelog</a>
        <a href="https://status.stellaratlas.xyz"><span class="re-pulse" aria-hidden></span> Status</a>
        <a href="https://github.com/StellarAtlas/stellar-atlas" target="_blank" rel="noopener">GitHub ↗</a>
      </nav>
    </header>
    <script
      id="api-reference"
      data-url="./stellar-atlas.v1.yaml"
      data-configuration='{
        "theme": "default",
        "layout": "modern",
        "showSidebar": true,
        "hideDownloadButton": false,
        "metaData": {
          "title": "Stellar Atlas — API reference",
          "description": "Stellar pricing API: VWAP / TWAP / OHLC + SSE."
        }
      }'
    ></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@${SCALAR_VERSION}/dist/browser/standalone.js"></script>
  </body>
</html>
EOF

cat > "$OUT_DIR/README.md" <<'EOF'
<!-- GENERATED FILE - DO NOT EDIT. Source: openapi/stellar-atlas.v1.yaml -->
---
title: Generated API reference
last_verified: 2026-05-06
status: generated
---

# API reference

GENERATED FILE — do not edit by hand. Source of truth:
[`openapi/stellar-atlas.v1.yaml`](../../../openapi/stellar-atlas.v1.yaml).

The rendered reference is [`index.html`](index.html), which loads
[Scalar](https://scalar.com/)'s standalone bundle from a pinned
CDN URL and points it at the colocated `stellar-atlas.v1.yaml`.

To regenerate: `make docs-api`. CI verifies the rendered output
is in sync with the spec on every PR that touches either side.
EOF

echo "✓ $OUT_DIR regenerated (Scalar ${SCALAR_VERSION})"
