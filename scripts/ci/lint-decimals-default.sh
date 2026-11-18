#!/usr/bin/env bash
# lint-decimals-default.sh — no token scale defaults to 7.
#
# A Soroban token's decimals are whatever its contract declares; a scale that was
# not declared, or could not be read, is unknown and is served absent. Stamping
# the classic/native 7 in its place divides an 18-dp token's amounts by 10^7 in
# every client. 7 is right only where the protocol fixes it (native, classic,
# SAC) or for a placeholder the scale-unknown flag keeps off the wire.
#
# Fails on a non-test Go line under internal/ or cmd/ that
#   - sets a decimals-named field or variable to 7, defaultTokenDecimals or
#     StandardDecimals (`Decimals: 7`, `baseDecimals := 7`), or
#   - returns one of those from a function whose name contains "decimals",
# unless the line, or a comment line directly above it, carries
# `decimals-7: <reason>`. Constant declarations are exempt: naming the
# protocol scale is not applying it. A marker on a line that no longer sets the
# scale fails as stale.
#
# Self-test: scripts/ci/lint-decimals-default-test.sh.
set -euo pipefail
cd "$(dirname "$0")/../.."

go_files=()
while IFS= read -r f; do go_files+=("$f"); done \
  < <(find internal cmd -name '*.go' ! -name '*_test.go' 2>/dev/null | LC_ALL=C sort)
[ "${#go_files[@]}" -gt 0 ] || { echo "lint-decimals-default: no Go files under internal/ or cmd/" >&2; exit 1; }

out=$(awk '
  BEGIN {
    scale  = "(7|defaultTokenDecimals|([A-Za-z_][A-Za-z0-9_]*\\.)?StandardDecimals)"
    assign = "(^|[^A-Za-z0-9_])[A-Za-z0-9_]*[Dd]ecimals[[:space:]]*(:|:?=)[[:space:]]*" scale "([^A-Za-z0-9_.]|$)"
    ret    = "^[[:space:]]*return[[:space:]]+" scale "([^A-Za-z0-9_]|$)"
    resolver = "^func[[:space:]]+(\\([^)]*\\)[[:space:]]*)?[A-Za-z0-9_]*[Dd]ecimals[A-Za-z0-9_]*[[:space:]]*[[(]"
    marker = "decimals-7:[[:space:]]+[^[:space:]]"
  }
  FNR == 1 { prev = ""; fn = ""; inconst = 0 }
  /^[[:space:]]*\/\// { prev = $0; next }
  {
    if ($0 ~ /^func[[:space:]]/) fn = $0
    if ($0 ~ /^const[[:space:]]*\(/) { inconst = 1; prev = ""; next }
    if (inconst && $0 ~ /^\)/) { inconst = 0; prev = ""; next }
    code = $0
    sub(/[[:space:]]*\/\/.*$/, "", code)
    hit = 0
    if (!inconst && code !~ /^[[:space:]]*const[[:space:]]/ && code ~ assign) hit = 1
    if (fn ~ resolver && code ~ ret) hit = 1
    marked = ($0 ~ marker) || (prev ~ marker)
    if (hit && !marked) print "UNMARKED " FILENAME ":" FNR ":" $0
    if (!hit && marked) print "STALE " FILENAME ":" FNR ":" $0
    prev = ""
  }' "${go_files[@]}")

if [ -n "$out" ]; then
  if grep -q '^UNMARKED ' <<<"$out"; then
    echo "lint-decimals-default ❌ token scale set to 7 without a reason:" >&2
    grep '^UNMARKED ' <<<"$out" | sed 's/^UNMARKED /  /' >&2
    echo "  → serve an undeclared or unread Soroban scale absent; if 7 is fixed by the protocol here" >&2
    echo "    (native, classic, SAC) mark the line // decimals-7: <reason>." >&2
  fi
  if grep -q '^STALE ' <<<"$out"; then
    echo "lint-decimals-default ❌ decimals-7 marker on a line that no longer sets the scale; remove it:" >&2
    grep '^STALE ' <<<"$out" | sed 's/^STALE /  /' >&2
  fi
  exit 1
fi
echo "lint-decimals-default: OK (${#go_files[@]} files)."
