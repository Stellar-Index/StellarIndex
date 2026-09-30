#!/usr/bin/env bash
# doctor-test.sh — pins doctor.sh's gitleaks version check against the
# GITLEAKS_VERSION CI pins in .github/workflows/ci.yml: the pinned version
# reports ok, any other version warns. Tools are stubbed so the verdict does
# not depend on what this machine has installed.
#
# Run: bash scripts/dev/doctor-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

pin="$(sed -nE 's/^  GITLEAKS_VERSION: *v?([0-9][0-9.]*).*/\1/p' .github/workflows/ci.yml)"
if [ -z "$pin" ]; then
  echo "FAIL: no GITLEAKS_VERSION in .github/workflows/ci.yml" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
printf '#!/bin/sh\necho "%s"\n' "$tmp/gopath" >"$tmp/bin/go"
chmod +x "$tmp/bin/go"

fail=0

# run_with <gitleaks version output>: doctor's native profile with that stub.
run_with() {
  printf '#!/bin/sh\necho "%s"\n' "$1" >"$tmp/bin/gitleaks"
  chmod +x "$tmp/bin/gitleaks"
  PATH="$tmp/bin:/usr/bin:/bin" bash scripts/dev/doctor.sh --profile native 2>/dev/null
}

check() {
  local label="$1" want="$2" out="$3"
  if grep -Eq "$want" <<<"$out"; then
    echo "OK: $label"
  else
    echo "FAIL: $label — no line matching /$want/ in:" >&2
    printf '%s\n' "$out" | grep gitleaks >&2
    fail=1
  fi
}

check "pinned version is ok" "^  ok    gitleaks-version +$pin\$" "$(run_with "$pin")"
check "v-prefixed pinned version is ok" "^  ok    gitleaks-version +$pin\$" "$(run_with "v$pin")"
check "newer version warns" "^  warn  gitleaks-version +CI pins $pin; found 999\.0\.0\$" "$(run_with "999.0.0")"

exit "$fail"
