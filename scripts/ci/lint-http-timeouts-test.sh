#!/usr/bin/env bash
# lint-http-timeouts-test.sh — fixture tests for the unbounded-HTTP gate
# (scripts/ci/lint-http-timeouts.sh, #371 F5).
#
# The gate's value is that a hang is invisible: `http.DefaultClient` with
# a dead peer blocks forever, and the symptom is "the backfill is slow"
# or "the command didn't come back", not an error anyone greps for. Two
# real instances shipped before it existed. So its behaviour is pinned
# rather than assumed:
#
#   - a production use is CAUGHT;
#   - a `// http-timeout-ok:` marker on the line, or anywhere in the
#     comment block above it, passes;
#   - a comment merely NAMING http.DefaultClient is not a use;
#   - _test.go files are exempt (a hung test fails loudly anyway);
#   - a clean tree passes;
#   - a root with no Go files FAILS rather than passing vacuously.
#
# Run: bash scripts/ci/lint-http-timeouts-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-http-timeouts.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
check() { # check <desc> <want-exit> <dir>
  local desc="$1" want="$2" dir="$3" got
  bash "$LINT" "$dir" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then echo "  ok   $desc"; pass=$((pass + 1))
  else echo "  FAIL $desc (exit $got, want $want)"; fail=$((fail + 1)); fi
}
mk() { mkdir -p "$TMP/$1"; printf '%s\n' "$3" > "$TMP/$1/$2"; }

echo "lint-http-timeouts-test: detection"

mk bad svc.go 'package svc
func f() { resp, _ := http.DefaultClient.Do(req); _ = resp }'
check "a production http.DefaultClient use is caught" 1 "$TMP/bad"

mk good svc.go 'package svc
func f() { c := &http.Client{Timeout: 30 * time.Second}; resp, _ := c.Do(req); _ = resp }'
check "an explicitly bounded client passes" 0 "$TMP/good"

echo "lint-http-timeouts-test: escape hatch"

mk okinline svc.go 'package svc
func f() { resp, _ := http.DefaultClient.Do(req) // http-timeout-ok: localhost probe, ctx always set
_ = resp }'
check "marker on the line passes" 0 "$TMP/okinline"

mk okblock svc.go 'package svc
// http-timeout-ok: this is a loopback health probe whose caller always
// supplies a deadline, and a stuck local socket is a bigger problem.
func f() { resp, _ := http.DefaultClient.Do(req); _ = resp }'
check "marker in the comment block above passes" 0 "$TMP/okblock"

mk quoted svc.go 'package svc
// Never use http.DefaultClient here — it has no timeout.
func f() { c := &http.Client{Timeout: time.Minute}; _ = c }'
check "a comment naming http.DefaultClient is not a use" 0 "$TMP/quoted"

echo "lint-http-timeouts-test: scope"

mk testonly svc_test.go 'package svc
func TestF(t *testing.T) { resp, _ := http.DefaultClient.Do(req); _ = resp }'
mk testonly real.go 'package svc
func g() {}'
check "_test.go is exempt (a hung test fails loudly)" 0 "$TMP/testonly"

mkdir -p "$TMP/empty"
check "a root with no Go files FAILS rather than passing vacuously" 1 "$TMP/empty"

echo "lint-http-timeouts-test: the real tree"
check "the repo's own production code is clean" 0 "internal cmd pkg"

echo "lint-http-timeouts-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
