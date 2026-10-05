#!/usr/bin/env bash
# History citations (dates, ticket ids, #nnn, PR n) in product Go comments; see scripts/ci/lint-comments/main.go.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
exec go run ./scripts/ci/lint-comments "$@"
