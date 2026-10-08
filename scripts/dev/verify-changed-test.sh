#!/usr/bin/env bash
# verify-changed-test.sh — fixture tests for scripts/dev/verify-changed.sh.
#
# The script's value is selection plus summary: it tests only the packages an
# edit can reach, sees uncommitted and untracked edits, prints counts and the
# log path instead of the test output, shows failing lines when a test fails,
# and refuses a bad base. Each is pinned against a fixture module so a change
# to the selection or the summary fails here rather than at push time.
#
# Fixture repositories are created under mktemp with global and system git
# config masked. The fixture module has no external dependencies, so the real
# toolchain's module cache is enough; no network.
set -uo pipefail
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
VERIFY="$PWD/scripts/dev/verify-changed.sh"

TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$TMP"' EXIT
export TMPDIR="$TMP"   # the script's logs land under $TMPDIR/verify-changed; keep them out of the real one

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid

pass=0
fail=0
ok()  { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { local d="$1"; shift; echo "  FAIL $d"; local l; for l in "$@"; do echo "       $l"; done; fail=$((fail + 1)); }
expect_exit() { if [ "$3" -eq "$2" ]; then ok "$1"; else bad "$1" "exit $3, want $2"; fi; }
expect_has() { if grep -qF -- "$2" <<<"$3"; then ok "$1"; else bad "$1" "missing: $2" "$3"; fi; }
expect_not() { if grep -qF -- "$2" <<<"$3"; then bad "$1" "present but must not be: $2"; else ok "$1"; fi; }
put() { mkdir -p "$(dirname "$1/$2")"; printf '%s\n' "$3" > "$1/$2"; }

# A module of three packages: core <- api (imports core); other stands alone.
R="$TMP/mod"; mkdir -p "$R" && git -C "$R" init -q
put "$R" go.mod $'module fixture.invalid/m\n\ngo 1.22\n'
put "$R" core/core.go $'package core\n\nfunc Answer() int { return 42 }\n'
put "$R" core/core_test.go $'package core\n\nimport "testing"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal("wrong answer")\n\t}\n}\n'
put "$R" api/api.go $'package api\n\nimport "fixture.invalid/m/core"\n\nfunc Double() int { return core.Answer() * 2 }\n'
put "$R" api/api_test.go $'package api\n\nimport "testing"\n\nfunc TestDouble(t *testing.T) {\n\tif Double() != 84 {\n\t\tt.Fatalf("got %d", Double())\n\t}\n}\n'
put "$R" other/other.go $'package other\n\nfunc Nothing() {}\n'
put "$R" other/other_test.go $'package other\n\nimport "testing"\n\nfunc TestNothing(t *testing.T) { Nothing() }\n'
git -C "$R" add -A && git -C "$R" commit -q -m base && git -C "$R" branch -q base

echo "verify-changed-test: nothing changed"
out="$(cd "$R" && "$VERIFY" base 2>&1)"; rc=$?
expect_exit "a clean tree passes" 0 "$rc"
expect_has "and says no package was affected" "go: no package affected" "$out"

echo "verify-changed-test: an uncommitted edit selects the package and its dependents"
put "$R" core/core.go $'package core\n\nfunc Answer() int { return 41 + 1 }\n'
out="$(cd "$R" && "$VERIFY" base 2>&1)"; rc=$?
expect_exit "an unstaged edit to core passes" 0 "$rc"
expect_has "vets core and api, not other" "go vet: ok (2 package(s))" "$out"
expect_has "counts the two passing packages" "go test: 2 ok, 0 FAIL, 0 no test files" "$out"
expect_has "and points at the full log" "full output $TMP/verify-changed/go-test-" "$out"
expect_not "without echoing the test output" "ok  	fixture.invalid" "$out"

echo "verify-changed-test: a failing test shows its lines and fails the run"
put "$R" api/api.go $'package api\n\nimport "fixture.invalid/m/core"\n\nfunc Double() int { return core.Answer() * 3 }\n'
out="$(cd "$R" && "$VERIFY" base 2>&1)"; rc=$?
expect_exit "a broken dependent fails" 1 "$rc"
expect_has "counts the failure" "go test: 1 ok, 1 FAIL," "$out"
expect_has "and shows the failing assertion" "got 126" "$out"
expect_not "but not the passing package's line" $'ok  \tfixture.invalid/m/core' "$out"
git -C "$R" checkout -q -- api/api.go

echo "verify-changed-test: an untracked package is seen"
put "$R" fresh/fresh.go $'package fresh\n\nfunc New() bool { return true }\n'
put "$R" fresh/fresh_test.go $'package fresh\n\nimport "testing"\n\nfunc TestNew(t *testing.T) {\n\tif !New() {\n\t\tt.Fatal("not new")\n\t}\n}\n'
out="$(cd "$R" && "$VERIFY" base 2>&1)"; rc=$?
expect_exit "untracked files pass" 0 "$rc"
expect_has "and are tested alongside the edited packages" "go test: 3 ok, 0 FAIL," "$out"
rm -rf "$R/fresh"

echo "verify-changed-test: a comment-only edit affects nothing"
git -C "$R" checkout -q -- core/core.go
put "$R" other/other.go $'package other\n\n// Nothing does nothing.\nfunc Nothing() {}\n'
out="$(cd "$R" && "$VERIFY" base 2>&1)"; rc=$?
expect_exit "a comment-only edit passes" 0 "$rc"
expect_has "and runs no test" "go: no package affected" "$out"
git -C "$R" checkout -q -- other/other.go

echo "verify-changed-test: refusals"
out="$(cd "$R" && "$VERIFY" no-such-ref 2>&1)"; rc=$?
expect_exit "an unknown base exits 2" 2 "$rc"
expect_has "and names it" "no such base: no-such-ref" "$out"
out="$(cd "$TMP" && "$VERIFY" base 2>&1)"; rc=$?
expect_exit "outside a work tree exits 2" 2 "$rc"

echo "verify-changed-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
