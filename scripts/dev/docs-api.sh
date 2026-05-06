#!/usr/bin/env bash
# Regenerate the rendered API reference from openapi/rates-engine.v1.yaml.
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
SCALAR_VERSION="1.34.10"

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"

OUT_DIR="docs/reference/api"
mkdir -p "$OUT_DIR"

# Copy the OpenAPI spec next to the rendered HTML. Scalar fetches
# it via the relative URL at view time, so it must live under the
# same CF Pages project root.
cp openapi/rates-engine.v1.yaml "$OUT_DIR/rates-engine.v1.yaml"

cat > "$OUT_DIR/index.html" <<EOF
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Rates Engine — API reference</title>
    <meta
      name="description"
      content="Comprehensive Stellar-network pricing API. REST + SSE endpoints for VWAP / TWAP / OHLC across on-chain DEXes, classic SDEX, and major exchanges."
    />
    <link rel="canonical" href="https://docs.ratesengine.net/" />
    <style>
      html, body { margin: 0; padding: 0; }
    </style>
  </head>
  <body>
    <script
      id="api-reference"
      data-url="./rates-engine.v1.yaml"
      data-configuration='{
        "theme": "default",
        "layout": "modern",
        "showSidebar": true,
        "hideDownloadButton": false,
        "metaData": {
          "title": "Rates Engine — API reference",
          "description": "Stellar pricing API: VWAP / TWAP / OHLC + SSE."
        }
      }'
    ></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@${SCALAR_VERSION}/dist/browser/standalone.js"></script>
  </body>
</html>
EOF

cat > "$OUT_DIR/README.md" <<'EOF'
<!-- GENERATED FILE - DO NOT EDIT. Source: openapi/rates-engine.v1.yaml -->
---
title: Generated API reference
last_verified: 2026-05-06
status: generated
---

# API reference

GENERATED FILE — do not edit by hand. Source of truth:
[`openapi/rates-engine.v1.yaml`](../../../openapi/rates-engine.v1.yaml).

The rendered reference is [`index.html`](index.html), which loads
[Scalar](https://scalar.com/)'s standalone bundle from a pinned
CDN URL and points it at the colocated `rates-engine.v1.yaml`.

To regenerate: `make docs-api`. CI verifies the rendered output
is in sync with the spec on every PR that touches either side.
EOF

echo "✓ $OUT_DIR regenerated (Scalar ${SCALAR_VERSION})"
