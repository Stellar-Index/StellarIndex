#!/usr/bin/env bash
# lint-go-toolchain-parity-test.sh — fixture tests for the toolchain-
# parity gate (scripts/ci/lint-go-toolchain-parity.sh).
#
# Pins the gate non-vacuous: it must CATCH a setup-go step with no
# go-version-file, CATCH one that pins a hardcoded go-version alongside
# go-version-file (which wins over it in the real action), PASS the
# correct shape, and FAIL rather than pass on a root with no workflows
# or no setup-go steps at all.
#
# Run: bash scripts/ci/lint-go-toolchain-parity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-go-toolchain-parity.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
asserts=0

check() { # check <desc> <want-exit> <root>
  local desc="$1" want="$2" root="$3" got
  asserts=$((asserts + 1))
  bash "$LINT" "$root" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want $want)"
    fail=$((fail + 1))
  fi
}

mk() { # mk <dir> <file> <steps-yaml>
  mkdir -p "$TMP/$1"
  cat > "$TMP/$1/$2" <<YML
name: fixture
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
$3
YML
}

# ── correct shape: passes ────────────────────────────────────────────
mk good ci.yml "      - uses: actions/checkout@$(printf '0%.0s' {1..40})  # v7.0.1
      - uses: actions/setup-go@$(printf '1%.0s' {1..40})  # v7.0.0
        with:
          go-version-file: go.mod
          cache: true
      - run: go build ./..."
check "go-version-file only -> pass" 0 "$TMP/good"

# ── missing go-version-file entirely ─────────────────────────────────
mk missing ci.yml "      - uses: actions/setup-go@$(printf '1%.0s' {1..40})  # v7.0.0
        with:
          cache: true
      - run: go build ./..."
check "no go-version-file at all -> FAIL" 1 "$TMP/missing"

# ── hardcoded go-version, no go-version-file: exactly #495's risk ────
mk hardcoded ci.yml "      - uses: actions/setup-go@$(printf '1%.0s' {1..40})  # v7.0.0
        with:
          go-version: '1.25'
      - run: go build ./..."
check "hardcoded go-version, no go-version-file -> FAIL" 1 "$TMP/hardcoded"

# ── both set: go-version overrides go-version-file in the real action,
# so this must fail even though go-version-file is technically present.
mk both ci.yml "      - uses: actions/setup-go@$(printf '1%.0s' {1..40})  # v7.0.0
        with:
          go-version-file: go.mod
          go-version: '1.25'
      - run: go build ./..."
check "go-version-file AND a hardcoded go-version -> FAIL (the hardcode wins)" 1 "$TMP/both"

# ── last step in the file is a setup-go step (END-of-file block close) ─
mk lastline ci.yml "      - uses: actions/setup-go@$(printf '1%.0s' {1..40})  # v7.0.0
        with:
          go-version-file: go.mod"
check "setup-go step is the final lines of the file -> still evaluated, pass" 0 "$TMP/lastline"

mk lastline_bad ci.yml "      - uses: actions/setup-go@$(printf '1%.0s' {1..40})  # v7.0.0
        with:
          cache: true"
check "setup-go step is the final lines, missing go-version-file -> FAIL" 1 "$TMP/lastline_bad"

# ── multiple setup-go steps in one file: one bad step must still fail
# the whole gate, not be averaged away by the good ones. ──────────────
mk mixed ci.yml "      - uses: actions/setup-go@$(printf '1%.0s' {1..40})  # v7.0.0
        with:
          go-version-file: go.mod
      - run: echo between steps
      - uses: actions/setup-go@$(printf '2%.0s' {1..40})  # v7.0.0
        with:
          go-version: '1.25'"
check "one good + one bad setup-go step in the same file -> FAIL" 1 "$TMP/mixed"

# ── vacuous roots must FAIL, never pass silently ─────────────────────
mkdir -p "$TMP/empty"
check "empty root, no workflow files -> FAIL (vacuous)" 1 "$TMP/empty"

mk nosetup ci.yml "      - run: echo hello"
check "workflow with no setup-go step at all -> FAIL (vacuous)" 1 "$TMP/nosetup"

# ── the repo's own workflow tree is clean ────────────────────────────
check "repo's own .github/workflows/ passes" 0 ".github/workflows"

echo
echo "lint-go-toolchain-parity-test: ${pass} passed, ${fail} failed, ${asserts} assertions requested"
if [ "$asserts" -lt 9 ]; then
  echo "lint-go-toolchain-parity-test: FAIL — only ${asserts} assertions ran; cases have been lost" >&2
  exit 1
fi
[ "$fail" -eq 0 ] || exit 1
