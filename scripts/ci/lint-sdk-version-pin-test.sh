#!/usr/bin/env bash
# lint-sdk-version-pin-test.sh — fixture tests for the go-stellar-sdk
# pin-parity gate (scripts/ci/lint-sdk-version-pin.sh).
#
# Proves the gate CATCHES a VERSIONS.md tag that has fallen behind
# go.mod (Q274: VERSIONS.md stayed on v0.6.0 after go.mod moved to
# v0.7.3), PASSES when they agree, and FAILS closed on missing inputs
# rather than passing vacuously.
#
# Run: bash scripts/ci/lint-sdk-version-pin-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-sdk-version-pin.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
asserts=0

check() { # check <desc> <want-exit> <gomod> <versions>
  local desc="$1" want="$2" gomod="$3" versions="$4" got
  asserts=$((asserts + 1))
  bash "$LINT" "$gomod" "$versions" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want $want)"
    fail=$((fail + 1))
  fi
}

mk_gomod() { # mk_gomod <path> <version>
  cat > "$1" <<GOMOD
module example.com/fixture

go 1.26.0

require github.com/stellar/go-stellar-sdk $2 // fixture
GOMOD
}

mk_versions() { # mk_versions <path> <tag>
  cat > "$1" <<MD
| Repo | SHA | Last commit | Tag | Our dependency? |
| ---- | --- | ----------- | --- | --------------- |
| \`stellar/go-stellar-sdk\` | \`deadbeef\` | 2026-08-20 | \`$2\` | **Go library — direct dep.** |
MD
}

# ── stale row: go.mod moved on, VERSIONS.md didn't (Q274's shape) ────
mk_gomod "$TMP/stale.mod" "v0.7.3"
mk_versions "$TMP/stale.md" "v0.6.0"
check "VERSIONS.md behind go.mod -> FAIL" 1 "$TMP/stale.mod" "$TMP/stale.md"

# ── matching pin: passes ─────────────────────────────────────────────
mk_gomod "$TMP/ok.mod" "v0.7.3"
mk_versions "$TMP/ok.md" "v0.7.3"
check "VERSIONS.md matches go.mod -> pass" 0 "$TMP/ok.mod" "$TMP/ok.md"

# ── missing inputs fail closed, not vacuously ────────────────────────
check "missing go.mod -> FAIL" 1 "$TMP/nope.mod" "$TMP/ok.md"
check "missing VERSIONS.md -> FAIL" 1 "$TMP/ok.mod" "$TMP/nope.md"

mk_gomod "$TMP/norow.mod" "v0.7.3"
: > "$TMP/norow.md"
check "VERSIONS.md with no sdk row -> FAIL" 1 "$TMP/norow.mod" "$TMP/norow.md"

# ── the repo's own VERSIONS.md and go.mod agree ──────────────────────
check "repo's own go.mod and VERSIONS.md agree" 0 "go.mod" "VERSIONS.md"

echo
echo "lint-sdk-version-pin-test: ${pass} passed, ${fail} failed, ${asserts} assertions requested"
if [ "$asserts" -lt 6 ]; then
  echo "lint-sdk-version-pin-test: FAIL — only ${asserts} assertions ran; cases have been lost" >&2
  exit 1
fi
[ "$fail" -eq 0 ] || exit 1
