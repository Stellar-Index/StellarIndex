#!/usr/bin/env bash
# lint-sdk-version-pin.sh — VERSIONS.md's pinned `stellar/go-stellar-sdk`
# tag must match the version go.mod actually requires.
#
# VERSIONS.md exists so a decode note's file/line citations stay
# re-verifiable against the SHA it claims. If go.mod bumps the SDK and
# VERSIONS.md's row is left on the old tag (Q274: go.mod moved to
# v0.7.3 while the row stayed on v0.6.0/dd844ab3), the row silently
# points every future reader at the wrong upstream commit while
# claiming to be the source of truth.
#
# Usage: bash scripts/ci/lint-sdk-version-pin.sh [go.mod] [VERSIONS.md]
set -euo pipefail

cd "$(dirname "$0")/../.."

GOMOD="${1:-go.mod}"
VERSIONS="${2:-VERSIONS.md}"

if [[ ! -f "$GOMOD" ]]; then
  echo "lint-sdk-version-pin: FAIL — $GOMOD not found" >&2
  exit 1
fi
if [[ ! -f "$VERSIONS" ]]; then
  echo "lint-sdk-version-pin: FAIL — $VERSIONS not found" >&2
  exit 1
fi

gomod_require_line="$(grep -E '^require[[:space:]]+github\.com/stellar/go-stellar-sdk[[:space:]]+v[0-9]' "$GOMOD" || true)"
want="$(awk '{print $3; exit}' <<<"$gomod_require_line")"

if [[ -z "$want" ]]; then
  echo "lint-sdk-version-pin: FAIL — $GOMOD has no github.com/stellar/go-stellar-sdk require line" >&2
  exit 1
fi

# shellcheck disable=SC2016  # literal backticks in the markdown row pattern, not expansion
sdk_row_pattern='^\| `stellar/go-stellar-sdk`'
sdk_row="$(grep -E "$sdk_row_pattern" "$VERSIONS" || true)"
got_raw="$(awk -F'|' '{print $5; exit}' <<<"$sdk_row")"
got="${got_raw//[ \`]/}"

if [[ -z "$got" ]]; then
  echo "lint-sdk-version-pin: FAIL — $VERSIONS has no stellar/go-stellar-sdk pinned-snapshots row" >&2
  exit 1
fi

if [[ "$got" != "$want" ]]; then
  echo "lint-sdk-version-pin: FAIL — $VERSIONS pins go-stellar-sdk at $got but $GOMOD requires $want"
  exit 1
fi

echo "lint-sdk-version-pin: OK — $VERSIONS and $GOMOD agree on go-stellar-sdk $want"
