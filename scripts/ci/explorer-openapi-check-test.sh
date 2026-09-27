#!/usr/bin/env bash
# explorer-openapi-check-test.sh — fixture tests for
# scripts/ci/explorer-openapi-check.sh (GH-1300).
#
# The defect: the docs page links /openapi/stellar-index.v1.yaml, that file
# only reaches the static export via a `prebuild` lifecycle script, and
# nothing verified its presence or content afterwards — a build where the
# lifecycle hook silently didn't fire shipped a dead link with a green build.
#
# Run: bash scripts/ci/explorer-openapi-check-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/explorer-openapi-check.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! grep -q -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

mkdir -p "$TMP/spec-dir" "$TMP/missing/out" "$TMP/stale/out/openapi" "$TMP/ok/out/openapi"
echo "openapi: 3.0.0" >"$TMP/spec-dir/spec.yaml"

# 1. `out/` not built at all — the exact scenario a silently-skipped
# lifecycle hook produces: build succeeds, index.html exists, spec missing.
OUT="$(bash "$CHECK" "$TMP/missing/out" "$TMP/spec-dir/spec.yaml" 2>&1)"
RC=$?
expect "missing shipped spec fails" 1 "missing"

# 2. `out/openapi/...` exists but drifted from the canonical spec (a stale
# copy from a prior build, or a partial write).
echo "openapi: 2.0.0" >"$TMP/stale/out/openapi/stellar-index.v1.yaml"
OUT="$(bash "$CHECK" "$TMP/stale/out" "$TMP/spec-dir/spec.yaml" 2>&1)"
RC=$?
expect "stale shipped spec fails" 1 "does not match"

# 3. exact copy present — the healthy case must pass.
cp "$TMP/spec-dir/spec.yaml" "$TMP/ok/out/openapi/stellar-index.v1.yaml"
OUT="$(bash "$CHECK" "$TMP/ok/out" "$TMP/spec-dir/spec.yaml" 2>&1)"
RC=$?
expect "matching shipped spec passes" 0 "matches"

# 4. the actual wiring: package.json's postbuild:openapi runs
# `bash ../../scripts/ci/explorer-openapi-check.sh out` from within
# web/explorer, supplying only OUT and relying on SPEC's default. Build a
# real correct export there and confirm the default resolves the canonical
# spec rather than a nonexistent web/explorer/openapi/....
mkdir -p "$TMP/wired/web/explorer/out/openapi"
cp "$PWD/openapi/stellar-index.v1.yaml" "$TMP/wired/web/explorer/out/openapi/stellar-index.v1.yaml"
OUT="$(cd "$TMP/wired/web/explorer" && bash "$CHECK" out 2>&1)"
RC=$?
expect "default spec arg from web/explorer cwd passes" 0 "matches"

echo "explorer-openapi-check-test: ${pass} passed, ${fail} failed."
[ "$fail" -eq 0 ]
