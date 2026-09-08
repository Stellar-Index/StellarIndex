#!/usr/bin/env bash
# lint-doc-links.sh — thin wrapper; the logic and its reasoning live in
# scripts/ci/lint_doc_links.py. Exit code = number of failures.
#
# Usage:
#   lint-doc-links.sh              every tracked + untracked markdown file
#   lint-doc-links.sh FILE...      only these files as link SOURCES; link
#                                   TARGETS still resolve against the full
#                                   tree (see lint_doc_links.py's docstring)
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
exec python3 scripts/ci/lint_doc_links.py "$@"
