#!/usr/bin/env bash
# Regenerate the rendered API reference from openapi/stellar-index.v1.yaml.
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

# Subresource Integrity hash for the pinned bundle above. The docs
# host loads this script from a third-party CDN (jsdelivr); SRI makes
# the browser reject the script if jsdelivr ever serves bytes that
# don't match this hash (CDN compromise / tampering). It MUST be
# recomputed whenever SCALAR_VERSION changes, or the script will be
# blocked and the docs page will render blank:
#
#   curl -sL "https://cdn.jsdelivr.net/npm/@scalar/api-reference@${SCALAR_VERSION}/dist/browser/standalone.js" \
#     | openssl dgst -sha384 -binary | openssl base64 -A
#
SCALAR_SRI="sha384-lqNSpgZBaLA+vZvHYhcvbchU39mp7CC1Els+8Cxe2rZ34jCZp7iQM8ySSD4KZId5"

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"

OUT_DIR="docs/reference/api"
mkdir -p "$OUT_DIR"

# Copy the OpenAPI spec next to the rendered HTML. Scalar fetches
# it via the relative URL at view time, so it must live under the
# same CF Pages project root.
cp openapi/stellar-index.v1.yaml "$OUT_DIR/stellar-index.v1.yaml"

cat > "$OUT_DIR/index.html" <<EOF
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Stellar Index — API reference</title>
    <meta
      name="description"
      content="The Stellar Index API. REST + SSE endpoints for ledgers, transactions, contracts, assets, supply, history, analytics, and VWAP / TWAP / OHLC prices across on-chain DEXes, classic SDEX, and major exchanges."
    />
    <link rel="canonical" href="https://docs.stellarindex.io/" />
    <link rel="icon" type="image/svg+xml" href="/icon.svg" />

    <!-- Open Graph / Twitter card for shareable preview -->
    <meta property="og:type" content="website" />
    <meta property="og:site_name" content="Stellar Index — docs" />
    <meta property="og:title" content="Stellar Index — API reference" />
    <meta property="og:description" content="The Stellar Index API: explorer, history, supply, analytics, and VWAP / TWAP / OHLC prices. Public, no-auth, REST + streaming." />
    <meta property="og:url" content="https://docs.stellarindex.io/" />
    <meta property="og:image" content="https://docs.stellarindex.io/og.svg" />
    <meta property="og:image:width" content="1200" />
    <meta property="og:image:height" content="630" />
    <meta name="twitter:card" content="summary_large_image" />
    <meta name="twitter:title" content="Stellar Index — API reference" />
    <meta name="twitter:description" content="The Stellar Index API: explorer, history, supply, analytics, and VWAP / TWAP / OHLC prices. Public, no-auth, REST + streaming." />
    <meta name="twitter:image" content="https://docs.stellarindex.io/og.svg" />

    <style>
      html, body { margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif; }
      .re-topbar {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 16px;
        padding: 8px 16px;
        /* Explorer canvas (globals.css --color-canvas), not slate-navy. */
        background: #0a0b0d;
        color: #f4f6fa;
        font-size: 13px;
        border-bottom: 1px solid #1e2227;
      }
      .re-topbar a { color: #94a3b8; text-decoration: none; transition: color 0.1s; }
      .re-topbar a:hover { color: #4fd1b5; }
      .re-topbar .re-brand { font-weight: 600; color: #f4f6fa; display: flex; align-items: center; gap: 8px; }
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
      <a class="re-brand" href="https://stellarindex.io">
        <!-- The official Stellar mark, matching the explorer sidebar and the
             favicon. Was a light-blue rounded square with a chart line — the
             pre-2026-08-24 placeholder — so docs.stellarindex.io carried a
             different logo from every other surface. -->
        <svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <path d="M12.003 1.716c-1.37 0-2.7.27-3.948.78A10.18 10.18 0 0 0 2.66 7.901a10.136 10.136 0 0 0-.797 3.954c0 .258.01.516.027.775a1.942 1.942 0 0 1-1.055 1.88L0 14.934v1.902l2.463-1.26.072-.032v.005l.77-.39.758-.385.066-.039 14.807-7.56 1.666-.847 3.392-1.732V2.694L17.792 5.86 3.744 13.025l-.104.055-.017-.115a8.286 8.286 0 0 1-.071-1.105c0-2.255.88-4.377 2.474-5.977a8.462 8.462 0 0 1 2.71-1.82 8.513 8.513 0 0 1 3.2-.654h.067a8.41 8.41 0 0 1 4.09 1.055l1.628-.83.126-.066a10.11 10.11 0 0 0-5.845-1.853zM24 7.143 5.047 16.808l-1.666.847L0 19.382v1.902l3.282-1.671 2.91-1.485 14.058-7.153.105-.055.016.115c.05.369.072.743.072 1.11 0 2.255-.88 4.383-2.475 5.978a8.461 8.461 0 0 1-2.71 1.82 8.305 8.305 0 0 1-3.2.654h-.06c-1.441 0-2.86-.369-4.102-1.061l-.066.033-1.683.857c.594.418 1.232.776 1.903 1.062a10.11 10.11 0 0 0 3.947.797 10.09 10.09 0 0 0 7.17-2.975 10.136 10.136 0 0 0 2.969-7.18c0-.259-.005-.523-.027-.781a1.942 1.942 0 0 1 1.055-1.88L24 9.044z"/>
        </svg>
        Stellar<span style="font-weight:300">Index</span>
      </a>
      <nav class="re-links">
        <a href="https://stellarindex.io">Explorer</a>
        <a href="https://stellarindex.io/methodology">Methodology</a>
        <a href="https://stellarindex.io/sdk">Go SDK</a>
        <a href="https://stellarindex.io/changelog">Changelog</a>
        <a href="https://status.stellarindex.io"><span class="re-pulse" aria-hidden></span> Status</a>
        <a href="https://github.com/Stellar-Index/StellarIndex" target="_blank" rel="noopener">GitHub ↗</a>
      </nav>
    </header>
    <script
      id="api-reference"
      data-url="./stellar-index.v1.yaml"
      data-configuration='{
        "theme": "default",
        "layout": "modern",
        "showSidebar": true,
        "hideDownloadButton": false,
        "metaData": {
          "title": "Stellar Index — API reference",
          "description": "The Stellar Index API: explorer, history, supply, analytics, and prices."
        }
      }'
    ></script>
    <script
      src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@${SCALAR_VERSION}/dist/browser/standalone.js"
      integrity="${SCALAR_SRI}"
      crossorigin="anonymous"
    ></script>
  </body>
</html>
EOF

cat > "$OUT_DIR/README.md" <<'EOF'
<!-- GENERATED FILE - DO NOT EDIT. Source: openapi/stellar-index.v1.yaml -->
---
title: Generated API reference
last_verified: 2026-05-06
status: generated
---

# API reference

GENERATED FILE — do not edit by hand. Source of truth:
[`openapi/stellar-index.v1.yaml`](../../../openapi/stellar-index.v1.yaml).

The rendered reference is [`index.html`](index.html), which loads
[Scalar](https://scalar.com/)'s standalone bundle from a pinned
CDN URL and points it at the colocated `stellar-index.v1.yaml`.

To regenerate: `make docs-api`. CI verifies the rendered output
is in sync with the spec on every PR that touches either side.
EOF

echo "✓ $OUT_DIR regenerated (Scalar ${SCALAR_VERSION})"
