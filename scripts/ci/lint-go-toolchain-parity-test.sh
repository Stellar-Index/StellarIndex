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

# Fixture-only invocations must not be entangled with the real repo's
# docker/ dir (its container-pin drift is exercised separately below);
# point DOCKER_ROOT at an empty dir so those checks isolate workflow
# parsing from container-pin parity.
EMPTY_DOCKER_ROOT="$TMP/no-docker-here"
mkdir -p "$EMPTY_DOCKER_ROOT"

check() { # check <desc> <want-exit> <root> [docker-root]
  local desc="$1" want="$2" root="$3" docker_root="${4:-$EMPTY_DOCKER_ROOT}" got
  asserts=$((asserts + 1))
  DOCKER_ROOT="$docker_root" bash "$LINT" "$root" >/dev/null 2>&1
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

# ── container-pin glob must match Dockerfiles that don't START with
# "Dockerfile" (e.g. stellarindex-foo.Dockerfile), not just docker/Dockerfile
# and docker/verify/Dockerfile (RLT-045: `-name 'Dockerfile*'` missed these). ─
GOMOD_TOOLCHAIN="$(awk '/^toolchain[ \t]+go/ { sub(/^go/, "", $2); print $2; exit }' go.mod)"

mkdir -p "$TMP/dockerbad"
printf 'FROM golang:9.9.9-alpine@sha256:%040d AS builder\n' 0 > "$TMP/dockerbad/stellarindex-fixture.Dockerfile"
check "suffix-named Dockerfile with a mismatched pin -> FAIL (glob must find it)" 1 "$TMP/good" "$TMP/dockerbad"

mkdir -p "$TMP/dockergood"
printf 'FROM golang:%s-alpine@sha256:%040d AS builder\n' "$GOMOD_TOOLCHAIN" 0 > "$TMP/dockergood/stellarindex-fixture.Dockerfile"
check "suffix-named Dockerfile with a matching pin -> pass" 0 "$TMP/good" "$TMP/dockergood"

# ── the repo's own workflow tree is clean, but the container-pin check
# runs against the real docker/ dir regardless of ROOTS. Six
# stellarindex-*.Dockerfile files there pin golang:1.27-alpine while
# go.mod resolves to 1.26.8 (RLT-045): a `find docker -name 'Dockerfile*'`
# glob only ever matched docker/verify/Dockerfile, so the drift on the six
# `stellarindex-*.Dockerfile` files went undetected. With the glob fixed
# to `-iname '*Dockerfile*'` the gate now catches it and must FAIL here
# until those pins are corrected with a real digest lookup.
check "repo's own .github/workflows/ passes, but docker/ container pins are out of parity (RLT-045)" 1 ".github/workflows" "docker"

echo
echo "lint-go-toolchain-parity-test: ${pass} passed, ${fail} failed, ${asserts} assertions requested"
if [ "$asserts" -lt 9 ]; then
  echo "lint-go-toolchain-parity-test: FAIL — only ${asserts} assertions ran; cases have been lost" >&2
  exit 1
fi
[ "$fail" -eq 0 ] || exit 1
