#!/usr/bin/env bash
# Preview the docs.stellarindex.io site locally on :8080.
#
# The deployed site is exactly `docs/reference/api/` (CF Pages project
# `stellarindex-docs`, wrangler pages deploy docs/reference/api
# --project-name stellarindex-docs — see docs/operations/cf-pages-setup.md).
# `make docs-api` regenerates it from openapi/stellar-index.v1.yaml; this
# script only serves whatever is currently on disk there, so run
# `make docs-api` first if you want a fresh render.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
DOCS_DIR="$REPO_ROOT/docs/reference/api"
PORT="${DOCS_SERVE_PORT:-8080}"

if [[ ! -f "$DOCS_DIR/index.html" ]]; then
  echo "docs-serve: $DOCS_DIR/index.html missing — run 'make docs-api' first" >&2
  exit 1
fi

command -v python3 >/dev/null 2>&1 || {
  echo "docs-serve: python3 not found on PATH" >&2
  exit 1
}

echo "docs-serve: serving $DOCS_DIR on http://localhost:$PORT (Ctrl-C to stop)"
cd "$DOCS_DIR"
exec python3 -m http.server "$PORT"
