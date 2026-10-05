#!/usr/bin/env bash
# ADR template, Decision/Invariant immutability, and ADR/invariant/anchor citations.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
exec python3 scripts/ci/lint_adr_refs.py "$@"
