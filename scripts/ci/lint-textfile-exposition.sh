#!/usr/bin/env bash
# lint-textfile-exposition.sh — thin wrapper; the logic and its reasoning
# live in scripts/ci/lint_textfile_exposition.py. Exit code = number of
# findings.
#
# Every line a node_exporter textfile-collector producer writes must parse
# as Prometheus exposition format, because one bad line makes node_exporter
# reject the WHOLE file — and with it every unrelated family that shares
# it (r1 2026-09-10).
#
# Usage: lint-textfile-exposition.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
exec python3 scripts/ci/lint_textfile_exposition.py "$@"
