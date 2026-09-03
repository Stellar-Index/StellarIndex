#!/usr/bin/env bash
# lint-doc-links.sh — thin wrapper; the logic and its reasoning live in
# scripts/ci/lint_doc_links.py. Exit code = number of failures.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
exec python3 scripts/ci/lint_doc_links.py "$@"
