#!/usr/bin/env bash
# lint-test-patterns.sh — freeze two test patterns at today's count.
#
#   go-ast   a _test.go imports go/parser or go/ast (reads source as a tree)
#   md-read  a _test.go reads a *.md file (a string literal ending in .md,
#            beside a file read/walk call)
#
# Both pin prose or code shape instead of behaviour; a new check of either
# kind belongs in scripts/ci as a class lint. Baseline =
# scripts/ci/lint-test-patterns.baseline, one `<class> <file>` per file.
# A file outside the baseline fails; a baseline entry whose file no longer
# matches is stale and fails, so the list only shrinks.
#
# Usage: lint-test-patterns.sh [ROOT]   (the self-test passes fixture trees;
# TEST_PATTERNS_BASELINE overrides the baseline path).
set -euo pipefail
cd "$(dirname "$0")/../.."

root="${1:-.}"
BASELINE="${TEST_PATTERNS_BASELINE:-scripts/ci/lint-test-patterns.baseline}"

# Bare dir names when root is the default so paths match the baseline.
if [ "$root" = "." ]; then scan=(.); else scan=("$root"); fi

current=$(mktemp)
baseline_entries=$(mktemp)
trap 'rm -f "$current" "$baseline_entries"' EXIT

list() { # list <class> <regex> [<regex>...]: files matching every regex
  local class=$1 first=$2 f
  shift 2
  grep -rlE --include='*_test.go' --exclude-dir=node_modules --exclude-dir=.git "$first" "${scan[@]}" 2>/dev/null |
    while IFS= read -r f; do
      local ok=1 re
      for re in "$@"; do grep -qE "$re" "$f" || ok=0; done
      [ "$ok" -eq 1 ] && echo "$class ${f#./}"
    done || true
}

{
  list go-ast '^[[:space:]]*(import[[:space:]]+)?([A-Za-z_]+[[:space:]]+)?"go/(parser|ast)"'
  list md-read '[A-Za-z0-9_/.-]\.md["`]' '(ReadFile|ReadDir|WalkDir|filepath\.Walk|filepath\.Glob|os\.Open)\('
} | LC_ALL=C sort -u > "$current"

if [ -f "$BASELINE" ]; then
  { grep -vE '^[[:space:]]*(#|$)' "$BASELINE" || true; } | LC_ALL=C sort -u > "$baseline_entries"
fi

new=$(LC_ALL=C comm -13 "$baseline_entries" "$current")
stale=$(LC_ALL=C comm -23 "$baseline_entries" "$current")
fail=0

if [ -n "$new" ]; then
  echo "TEST-PATTERNS: new _test.go file(s) not in $BASELINE."
  echo "  go-ast / md-read tests are frozen; add a class lint under scripts/ci instead."
  printf '%s\n' "$new" | sed 's/^/  + /'
  fail=1
fi
if [ -n "$stale" ]; then
  echo "TEST-PATTERNS: stale baseline entr(y/ies) — file gone or no longer matches."
  echo "  Delete the line(s) from $BASELINE (shrink-only)."
  printf '%s\n' "$stale" | sed 's/^/  - /'
  fail=1
fi
[ "$fail" -eq 0 ] || exit 1
echo "lint-test-patterns: OK (matches baseline)."
