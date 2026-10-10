#!/usr/bin/env bash
# lint-test-patterns-test.sh — fixture tests for lint-test-patterns.sh.
# Run: bash scripts/ci/lint-test-patterns-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-test-patterns.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# shellcheck source=scripts/ci/lib/test-expect.sh
. "$PWD/scripts/ci/lib/test-expect.sh"

R="$TMP/root"

# run <baseline-content> — gate over the fixture tree in $R.
run() {
  printf '%s' "$1" > "$TMP/baseline"
  OUT="$(TEST_PATTERNS_BASELINE="$TMP/baseline" bash "$LINT" "$R" 2>&1)"
  RC=$?
}

fresh() { rm -rf "$R"; mkdir -p "$R/pkg/a"; }

fresh
printf 'package a\n\nimport "testing"\n' > "$R/pkg/a/clean_test.go"
run ''
expect "clean tree, empty baseline passes" 0 "OK"

fresh
printf 'package a\n\nimport (\n\t"go/parser"\n)\n' > "$R/pkg/a/ast_test.go"
run ''
expect "go/parser import outside baseline fails" 1 "+ go-ast"
run "go-ast $R/pkg/a/ast_test.go
"
expect "go/parser import in baseline passes" 0 "OK"

fresh
printf 'package a\n\nimport a "go/ast"\n' > "$R/pkg/a/alias_test.go"
run ''
expect "aliased go/ast import fails" 1 "+ go-ast"

fresh
printf 'package a\n\nfunc f() { os.ReadFile("../README.md") }\n' > "$R/pkg/a/md_test.go"
run ''
expect "reading a .md file fails" 1 "+ md-read"

fresh
printf 'package a\n\nconst name = "README.md"\n' > "$R/pkg/a/name_test.go"
run ''
expect ".md literal with no file read passes" 0 "OK"

fresh
printf 'package a\n\nimport "go/parser"\n' > "$R/pkg/a/prod.go"
run ''
expect "non-test file importing go/parser passes" 0 "OK"

fresh
printf 'package a\n' > "$R/pkg/a/clean_test.go"
run "go-ast $R/pkg/a/gone_test.go
"
expect "stale baseline entry fails" 1 "- go-ast"

echo "lint-test-patterns-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
