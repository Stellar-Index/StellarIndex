#!/usr/bin/env bash
# lint-conflict-markers-test.sh — fixture tests for lint-conflict-markers.sh.
# Fixtures are written to a temp dir at run time, so no marker line is
# committed for the tree scan to find.
#
# Run: bash scripts/ci/lint-conflict-markers-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-conflict-markers.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
asserts=0

run() {
  OUT="$(bash "$LINT" "$@" 2>&1 </dev/null)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_text="${3:-}"
  asserts=$((asserts + 1))
  if [ "$RC" -eq "$want_rc" ] && { [ -z "$want_text" ] || grep -qF -- "$want_text" <<<"$OUT"; }; then
    echo "ok: $name"
    pass=$((pass + 1))
  else
    echo "FAIL: $name — exit $RC, want $want_rc${want_text:+ and output containing \"$want_text\"}" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}

lt='<<<<<<<'
gt='>>>>>>>'
eq='======='

printf '# Title\n\n%s HEAD\nours\n%s\ntheirs\n%s feature\n' "$lt" "$eq" "$gt" >"$TMP/marked.md"
run "$TMP/marked.md"
expect "a file with conflict markers fails" 1 "marked.md:3: $lt HEAD"
expect "the separator between the markers is reported" 1 "marked.md:5: $eq"
expect "the closing marker is reported" 1 "marked.md:7: $gt feature"

printf 'Title\n%s\n\nBody text.\n' "$eq" >"$TMP/setext.md"
run "$TMP/setext.md"
expect "a Markdown setext heading underline passes" 0

printf 'Section\n%s\n\nSub\n-------\n\n%s\n' "$eq" "$eq" >"$TMP/rule.rst"
run "$TMP/rule.rst"
expect "an RST/ASCII rule line passes" 0

printf '%s\n' "$eq" >"$TMP/rule-only.txt"
printf 'x\n%s y\n' "$gt" >>"$TMP/rule-only.txt"
run "$TMP/rule-only.txt"
expect "a lone closing marker still fails" 1 "rule-only.txt:3: $gt"

printf '\0\1\2%s HEAD\n%s\n%s x\n\0' "$lt" "$eq" "$gt" >"$TMP/blob.bin"
printf '%s HEAD\n' "$lt" >>"$TMP/blob.bin"
run "$TMP/blob.bin"
expect "a binary file is skipped" 0

run "$TMP/blob.bin" "$TMP/setext.md" "$TMP/marked.md"
expect "a mixed list fails on the text file only" 1 "1 file(s) carry"

printf 'a %s not at line start\n' "$lt" >"$TMP/inline.txt"
run "$TMP/inline.txt"
expect "a marker not at the start of a line passes" 0

printf '%sno-space\n' "$lt" >"$TMP/nospace.txt"
run "$TMP/nospace.txt"
expect "seven angle brackets without the trailing space pass" 0

run "$TMP/does-not-exist"
expect "nothing scannable is a usage error, not a pass" 2

echo
echo "lint-conflict-markers-test: ${pass} passed, ${fail} failed, ${asserts} assertions requested"
if [ "$asserts" -lt 11 ]; then
  echo "lint-conflict-markers-test: FAIL — only ${asserts} assertions ran; cases have been lost" >&2
  exit 1
fi
[ "$fail" -eq 0 ] || exit 1
