#!/usr/bin/env bash
# check-verify-image-pins-test.sh — fixture tests for
# scripts/ci/check-verify-image-pins.sh.
#
# The gate is worth having only if it is non-vacuous: it must FAIL when the
# Dockerfile ARG default and the script's CONVERTER_VERSION differ, PASS when
# they agree, and refuse to pass when either constant is absent, doubled or
# the file is missing. Fixtures are synthetic Dockerfile / docs-postman.sh
# pairs — no Docker, no network, no repo state.
#
# Run: bash scripts/ci/check-verify-image-pins-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-verify-image-pins.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# write_dockerfile <path> <arg-line>... — a minimal verifier Dockerfile with
# the given ARG lines in the pin block.
write_dockerfile() {
  local path="$1"
  shift
  {
    printf 'FROM golang:1.25\n'
    printf 'ARG GOFUMPT_VERSION=v0.8.0\n'
    for line in "$@"; do printf '%s\n' "$line"; done
    printf 'RUN npm install --global "openapi-to-postmanv2@${OPENAPI_TO_POSTMAN_VERSION}"\n'
  } > "$path"
}

# write_script <path> <constant-line>... — a minimal docs-postman.sh with the
# given constant lines.
write_script() {
  local path="$1"
  shift
  {
    printf '#!/usr/bin/env bash\nset -euo pipefail\n'
    for line in "$@"; do printf '%s\n' "$line"; done
    printf 'echo generated\n'
  } > "$path"
}

run() {
  OUT="$(DOCKERFILE="$1" DOCS_POSTMAN_SH="$2" bash "$CHECK" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  if [ -n "$want_sub" ] && ! grep -qF -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# ── Pins agree → OK ──────────────────────────────────────────────────
write_dockerfile "$TMP/Dockerfile-ok" 'ARG OPENAPI_TO_POSTMAN_VERSION=6.0.1'
write_script "$TMP/script-ok.sh" 'CONVERTER_VERSION="6.0.1"'
run "$TMP/Dockerfile-ok" "$TMP/script-ok.sh"
expect 'image and script pin the same version → OK' 0 'OK — openapi-to-postmanv2 6.0.1'

# ── Script bumped, Dockerfile default left behind → FAIL naming both ─
write_script "$TMP/script-bumped.sh" 'CONVERTER_VERSION="6.1.0"'
run "$TMP/Dockerfile-ok" "$TMP/script-bumped.sh"
expect 'script pin ahead of the image default → FAIL' 1 'OPENAPI_TO_POSTMAN_VERSION=6.0.1'
expect 'the failure names the script pin too' 1 'CONVERTER_VERSION="6.1.0"'

# ── Dockerfile bumped, script left behind → FAIL ─────────────────────
write_dockerfile "$TMP/Dockerfile-bumped" 'ARG OPENAPI_TO_POSTMAN_VERSION=6.1.0'
run "$TMP/Dockerfile-bumped" "$TMP/script-ok.sh"
expect 'image default ahead of the script pin → FAIL' 1 'pin differs'

# ── Dockerfile without the ARG → FAIL (not vacuous) ──────────────────
write_dockerfile "$TMP/Dockerfile-noarg"
run "$TMP/Dockerfile-noarg" "$TMP/script-ok.sh"
expect 'Dockerfile lacking the ARG → FAIL' 1 "no 'ARG OPENAPI_TO_POSTMAN_VERSION"

# ── Script without the constant → FAIL (not vacuous) ─────────────────
write_script "$TMP/script-noconst.sh"
run "$TMP/Dockerfile-ok" "$TMP/script-noconst.sh"
expect 'script lacking CONVERTER_VERSION → FAIL' 1 "no 'CONVERTER_VERSION"

# ── Two ARG definitions → FAIL ────────────────────────────────────────
write_dockerfile "$TMP/Dockerfile-twice" 'ARG OPENAPI_TO_POSTMAN_VERSION=6.0.1' 'ARG OPENAPI_TO_POSTMAN_VERSION=6.1.0'
run "$TMP/Dockerfile-twice" "$TMP/script-ok.sh"
expect 'ARG defined twice → FAIL' 1 'more than once'

# ── Single-quoted or unquoted script constant is not the pinned form ─
# The script's contract is a double-quoted literal on its own line; a
# different spelling would silently dodge the gate, so it must be a FAIL.
write_script "$TMP/script-unquoted.sh" 'CONVERTER_VERSION=6.0.1'
run "$TMP/Dockerfile-ok" "$TMP/script-unquoted.sh"
expect 'unquoted CONVERTER_VERSION is not accepted → FAIL' 1 "no 'CONVERTER_VERSION"

# ── Missing file → FAIL (defensive) ──────────────────────────────────
run "$TMP/does-not-exist" "$TMP/script-ok.sh"
expect 'missing Dockerfile → FAIL' 1 'file not found'

echo "check-verify-image-pins-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
