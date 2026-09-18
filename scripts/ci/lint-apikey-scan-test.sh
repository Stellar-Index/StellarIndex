#!/usr/bin/env bash
# lint-apikey-scan-test.sh — fixture tests for the credential-keyspace
# walk ban (scripts/ci/lint-apikey-scan.sh, class finding K051).
#
# The gate's value is that the defect is invisible where it is written:
# a `SCAN apikey:*` lookup is correct, passes its tests, and only costs
# anything once the deployment holds other people's keys. So what the
# gate does is pinned rather than assumed:
#
#   - a Redis scan in internal/auth is CAUGHT, whatever its pattern;
#   - building the apikey:* wildcard is CAUGHT in any package, through
#     the typed builder or a literal, even when the scan call itself is
#     on another line;
#   - ONE marked walk passes; a SECOND marked walk fails;
#   - a marker that exempts nothing fails (the allowance only shrinks);
#   - a comment that merely names the pattern is not a use;
#   - _test.go files are exempt;
#   - a tree with no internal/auth FAILS rather than passing vacuously;
#   - the repo's own tree is clean, with exactly its one sanctioned walk.
#
# Run: bash scripts/ci/lint-apikey-scan-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-apikey-scan.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
check() { # check <desc> <want-exit> <root>
  local desc="$1" want="$2" root="$3" got
  bash "$LINT" "$root" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then echo "  ok   $desc"; pass=$((pass + 1))
  else echo "  FAIL $desc (exit $got, want $want)"; fail=$((fail + 1)); fi
}
mk() { # mk <case> <relative/file.go> <body>
  mkdir -p "$TMP/$1/$(dirname "$2")"
  printf '%s\n' "$3" > "$TMP/$1/$2"
}
clean_auth() { mk "$1" internal/auth/doc.go 'package auth'; }

echo "lint-apikey-scan-test: detection"

mk scan internal/auth/find.go 'package auth
func f() { iter := s.rdb.Scan(ctx, 0, pattern, 1000).Iterator(); _ = iter }'
check "a Redis scan in internal/auth is caught, whatever its pattern" 1 "$TMP/scan"

mk keys internal/auth/find.go 'package auth
func f() { all, _ := s.rdb.Keys(ctx, pattern).Result(); _ = all }'
check "KEYS in internal/auth is caught" 1 "$TMP/keys"

clean_auth elsewhere
mk elsewhere internal/api/v1/handler.go 'package v1
func f() {
	match := cachekeys.APIKey("*").String()
	iter := rdb.Scan(ctx, 0,
		match, 1000).Iterator()
	_ = iter
}'
check "the typed wildcard is caught outside internal/auth, scan on another line" 1 "$TMP/elsewhere"

clean_auth literal
mk literal cmd/tool/main.go 'package main
func f() { iter := rdb.Scan(ctx, 0, "apikey:*", 100).Iterator(); _ = iter }'
check "a literal apikey:* wildcard is caught under cmd/" 1 "$TMP/literal"

echo "lint-apikey-scan-test: the one sanctioned walk"

mk one internal/auth/walk.go 'package auth
func walk() {
	// apikey-scan-ok: the one sanctioned walk.
	iter := s.rdb.Scan(ctx, 0, cachekeys.APIKey("*").String(), 1000).Iterator()
	_ = iter
}'
check "one marked walk passes" 0 "$TMP/one"

mk two internal/auth/walk.go 'package auth
func walk() {
	// apikey-scan-ok: the one sanctioned walk.
	iter := s.rdb.Scan(ctx, 0, cachekeys.APIKey("*").String(), 1000).Iterator()
	_ = iter
}'
mk two internal/auth/another.go 'package auth
func another() {
	// apikey-scan-ok: just this once, it is only an admin path.
	iter := s.rdb.Scan(ctx, 0, cachekeys.APIKey("*").String(), 1000).Iterator()
	_ = iter
}'
check "a second marked walk fails" 1 "$TMP/two"

mk stale internal/auth/walk.go 'package auth
// apikey-scan-ok: left behind after the walk was deleted.
func walk() {}'
check "a marker that exempts nothing fails" 1 "$TMP/stale"

echo "lint-apikey-scan-test: not a use"

mk comment internal/auth/doc.go 'package auth
// Never rdb.Scan(ctx, 0, cachekeys.APIKey("*").String(), n) here: the
// apikey:* keyspace is attacker-sized.
func f() {}'
check "a comment naming the scan and the wildcard is not a use" 0 "$TMP/comment"

mk testonly internal/auth/real.go 'package auth
func g() {}'
mk testonly internal/auth/seed_test.go 'package auth
func TestSeed(t *testing.T) { iter := rdb.Scan(ctx, 0, "apikey:*", 10).Iterator(); _ = iter }'
check "_test.go is exempt" 0 "$TMP/testonly"

clean_auth concrete
mk concrete internal/api/v1/handler.go 'package v1
func f(hash string) { _ = rdb.Get(ctx, cachekeys.APIKey(hash).String()) }'
check "a concrete-key read through the builder passes" 0 "$TMP/concrete"

echo "lint-apikey-scan-test: vacuity"

mk noauth internal/other/x.go 'package other
func f() {}'
check "a tree with no internal/auth FAILS rather than passing vacuously" 1 "$TMP/noauth"

echo "lint-apikey-scan-test: the real tree"
check "the repo's own production code is clean" 0 "."
out="$(bash "$LINT" . 2>&1)"
case "$out" in
  *"1 sanctioned walk(s)"*) echo "  ok   the repo holds exactly its one sanctioned walk"; pass=$((pass + 1)) ;;
  *) echo "  FAIL the repo does not report exactly one sanctioned walk: $out"; fail=$((fail + 1)) ;;
esac

echo "lint-apikey-scan-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
