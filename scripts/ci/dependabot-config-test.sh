#!/usr/bin/env bash
# dependabot-config-test.sh — structural assertions on .github/dependabot.yml
# for the two gaps found in the 2026-09-18 reverification sweep (F145/K083,
# RLT-201):
#
#   1. The `gomod` `groups.go-minor-patch` group must exclude
#      github.com/stellar/go-stellar-sdk. Without an exclude, a same-PR
#      grouped minor/patch bump can land it (as
#      dependabot/go_modules/go-minor-patch commit ccb6d19b2 did, taking
#      go.mod from v0.6.0 to v0.7.3) skipping the manual SHA + compat-pass
#      update VERSIONS.md documents for that pin (ADR-0013).
#   2. A `docker` ecosystem entry must watch /docker/verify — the /docker
#      entry is documented non-recursive and docker/verify/Dockerfile sits
#      one directory below it, invisible to Dependabot without its own entry.
#
# Offline: reads the checked-in YAML with grep/awk only, no network, no
# git repository required.
#
# Run: bash scripts/ci/dependabot-config-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CONFIG="${1:-.github/dependabot.yml}"

pass=0
fail=0

assert_contains() {
  local desc="$1" needle="$2" haystack="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $desc" >&2
    echo "  expected to find: $needle" >&2
  fi
}

if [ ! -f "$CONFIG" ]; then
  echo "dependabot-config-test: FAIL — $CONFIG not found" >&2
  exit 1
fi

# Isolate the go-minor-patch group block: from its header to the next
# line at the same or lower indent (a new top-level "- package-ecosystem"
# entry, or EOF).
GOMOD_GROUP="$(awk '
  /^      go-minor-patch:/ { grab = 1; print; next }
  grab && /^  - package-ecosystem/ { exit }
  grab { print }
' "$CONFIG")"

assert_contains \
  "go-minor-patch group excludes the ADR-0013-pinned go-stellar-sdk module" \
  "github.com/stellar/go-stellar-sdk" \
  "$GOMOD_GROUP"

if [[ -n "$GOMOD_GROUP" ]]; then
  assert_contains \
    "go-minor-patch group has an exclude-patterns key" \
    "exclude-patterns:" \
    "$GOMOD_GROUP"
fi

# Every docker package-ecosystem block's directory value.
DOCKER_DIRS="$(awk '
  /^  - package-ecosystem: docker/ { ecosystem = 1; next }
  /^  - package-ecosystem:/ { ecosystem = 0 }
  ecosystem && /^    directory:/ { print $2 }
' "$CONFIG")"

assert_contains \
  "a docker ecosystem entry watches /docker/verify (non-recursive gap)" \
  "/docker/verify" \
  "$DOCKER_DIRS"

echo "dependabot-config-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
