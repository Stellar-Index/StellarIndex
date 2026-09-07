#!/usr/bin/env bash
# check-dependabot-toolchain-bump-test.sh — fixture tests for the
# Dependabot toolchain-bump guard (scripts/ci/check-dependabot-toolchain-bump.sh).
#
# The load-bearing case reproduces PR #495's actual go.mod diff verbatim
# (github.com/Stellar-Index/StellarIndex, dependabot/go_modules/
# go-minor-patch-4a08daf3b0, open as of 2026-09-07):
#
#   -go 1.25.10
#   -
#   -toolchain go1.25.13
#   +go 1.26.0
#
# alongside its eight ordinary module bumps in the same grouped PR. A
# gate that only reacts to a bare `go.mod` diff without distinguishing
# the toolchain lines from an ordinary `require` bump, or that fires for
# every author instead of only dependabot[bot], is not this fix.
#
# Offline: builds unified diffs with `diff -u` against temp files, no
# git repository and no network.
#
# Run: bash scripts/ci/check-dependabot-toolchain-bump-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-dependabot-toolchain-bump.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
asserts=0

# gomod_diff <old-content> <new-content> — unified diff, paths rewritten
# to a/go.mod b/go.mod exactly as `git diff` would emit them, since the
# script keys off the `+++ b/<path>` header.
gomod_diff() {
  printf '%s' "$1" > "$TMP/old"
  printf '%s' "$2" > "$TMP/new"
  diff -u "$TMP/old" "$TMP/new" 2>/dev/null | sed \
    -e "1s#.*#--- a/go.mod#" \
    -e "2s#.*#+++ b/go.mod#"
  return 0
}

package_json_diff() {
  printf '%s' "$1" > "$TMP/old"
  printf '%s' "$2" > "$TMP/new"
  diff -u "$TMP/old" "$TMP/new" 2>/dev/null | sed \
    -e "1s#.*#--- a/web/status/package.json#" \
    -e "2s#.*#+++ b/web/status/package.json#"
  return 0
}

# run <author> <diff-text>
run() {
  OUT="$(printf '%s\n' "$2" | bash "$CHECK" "$1" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  asserts=$((asserts + 1))
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! grep -qF -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# ── PR #495's actual shape ───────────────────────────────────────────

OLD_GOMOD_495='module github.com/Stellar-Index/StellarIndex

go 1.25.10

toolchain go1.25.13

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/golang-migrate/migrate/v4 v4.19.1
)
'
NEW_GOMOD_495='module github.com/Stellar-Index/StellarIndex

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/alicebob/miniredis/v2 v2.39.0
	github.com/golang-migrate/migrate/v4 v4.19.1
)
'
DIFF_495="$(gomod_diff "$OLD_GOMOD_495" "$NEW_GOMOD_495")"

run "dependabot[bot]" "$DIFF_495"
expect "PR #495's real diff, dependabot author → BLOCKED" 1 "check-dependabot-toolchain-bump"

run "dependabot[bot]" "$DIFF_495"
expect "PR #495's real diff names the go.mod toolchain line" 1 "go.mod: -go 1.25.10"

# ── Same diff, different actor: a human deliberately bumping Go is not
# what this gate exists to catch. ────────────────────────────────────

run "ash" "$DIFF_495"
expect "identical go.mod diff, human author → NOT blocked" 0

# ── An ordinary dependabot module bump with no toolchain line must
# still pass — the gate is scoped to the language-version lines only. ─

OLD_ORDINARY='module github.com/Stellar-Index/StellarIndex

go 1.25.10

toolchain go1.25.13

require (
	github.com/BurntSushi/toml v1.6.0
)
'
NEW_ORDINARY='module github.com/Stellar-Index/StellarIndex

go 1.25.10

toolchain go1.25.13

require (
	github.com/BurntSushi/toml v1.7.0
)
'
run "dependabot[bot]" "$(gomod_diff "$OLD_ORDINARY" "$NEW_ORDINARY")"
expect "dependabot bumping an ordinary require line only → NOT blocked" 0

# ── toolchain directive added/changed without touching `go` line ─────

OLD_TC='module x

go 1.25.10
'
NEW_TC='module x

go 1.25.10

toolchain go1.25.14
'
run "dependabot[bot]" "$(gomod_diff "$OLD_TC" "$NEW_TC")"
expect "dependabot adding/bumping toolchain directive alone → BLOCKED" 1

# ── package.json engines / packageManager ────────────────────────────

OLD_PKG='{
  "name": "web-status",
  "engines": {
    "node": ">=20"
  },
  "packageManager": "pnpm@10.32.0"
}
'
NEW_PKG='{
  "name": "web-status",
  "engines": {
    "node": ">=22"
  },
  "packageManager": "pnpm@10.33.0"
}
'
run "dependabot[bot]" "$(package_json_diff "$OLD_PKG" "$NEW_PKG")"
expect "dependabot bumping package.json engines/packageManager → BLOCKED" 1

# A dependency-only package.json bump (no engines/packageManager touched)
# must not trip the gate.
OLD_PKG2='{
  "name": "web-status",
  "engines": {
    "node": ">=20"
  },
  "dependencies": {
    "next": "15.0.0"
  }
}
'
NEW_PKG2='{
  "name": "web-status",
  "engines": {
    "node": ">=20"
  },
  "dependencies": {
    "next": "15.1.0"
  }
}
'
run "dependabot[bot]" "$(package_json_diff "$OLD_PKG2" "$NEW_PKG2")"
expect "dependabot bumping an ordinary package.json dependency → NOT blocked" 0

# ── No diff at all (e.g. the paths given to `git diff -- go.mod
# '**/package.json'` produced nothing) must not block. ───────────────

run "dependabot[bot]" ""
expect "empty diff, dependabot author → NOT blocked" 0

# ── Usage error ───────────────────────────────────────────────────────

OUT="$(: | bash "$CHECK" 2>&1)"; RC=$?
expect "missing author argument → usage error" 2

echo
echo "check-dependabot-toolchain-bump-test: ${pass} passed, ${fail} failed, ${asserts} assertions requested"
if [ "$asserts" -lt 9 ]; then
  echo "check-dependabot-toolchain-bump-test: FAIL — only ${asserts} assertions ran; cases have been lost" >&2
  exit 1
fi
[ "$fail" -eq 0 ] || exit 1
