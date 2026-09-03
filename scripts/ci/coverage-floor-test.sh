#!/usr/bin/env bash
# coverage-floor-test.sh — fixture tests for scripts/ci/coverage-floor.sh
# (#340 item 6).
#
# A coverage gate is the easiest kind of gate to make vacuous: it reads a
# file that is generated somewhere else, so it can silently start
# measuring nothing and go on printing a green tick forever. This repo
# has that scar twice over — 11 of 37 CI gates were found vacuous in the
# August 2026 audit, and the weekly SLA run published an eleven-week-old
# all-green artifact for months.
#
# So the gate is required to be PROVABLY able to fail, and these fixtures
# are that proof. Every hard-failure branch gets a case that exercises it
# against a real profile on disk, including the two that matter most:
#
#   - a profile whose SCOPE shrank (coverage measured over far fewer
#     packages), which is how a coverage number goes UP while the tree
#     gets less safe;
#   - a coverage number under the floor, which must warn in report-only
#     mode and FAIL under COVERAGE_ENFORCE=1 — the same input, two
#     verdicts, which is the only way to show report-only is a
#     deliberate setting rather than a gate that cannot fail at all.
#
# Run: bash scripts/ci/coverage-floor-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/coverage-floor.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# mkprofile <file> <npkgs> <ncovered-per-pkg> <ntotal-per-pkg>
# Writes a syntactically real Go coverage profile: one package per
# generated dir, `ntotal` statement blocks each, the first `ncovered`
# of them with a non-zero hit count.
mkprofile() {
	local out="$1" npkgs="$2" ncov="$3" ntot="$4" p i count
	echo "mode: atomic" >"$out"
	p=0
	while [ "$p" -lt "$npkgs" ]; do
		i=0
		while [ "$i" -lt "$ntot" ]; do
			count=0
			[ "$i" -lt "$ncov" ] && count=1
			echo "example.com/m/pkg${p}/file.go:$((i + 1)).1,$((i + 1)).20 1 ${count}" >>"$out"
			i=$((i + 1))
		done
		p=$((p + 1))
	done
}

run() {
	OUT="$(env "$@" bash "$CHECK" 2>&1)"
	RC=$?
}

expect() {
	local name="$1" want_rc="$2" want_sub="${3:-}"
	if [ "$RC" -ne "$want_rc" ]; then
		echo "FAIL: $name — exit $RC, want $want_rc" >&2
		printf '%s\n' "$OUT" | sed 's/^/    /' >&2
		fail=$((fail + 1))
		return
	fi
	if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -q -- "$want_sub"; then
		echo "FAIL: $name — output missing '$want_sub'" >&2
		printf '%s\n' "$OUT" | sed 's/^/    /' >&2
		fail=$((fail + 1))
		return
	fi
	echo "ok: $name"
	pass=$((pass + 1))
}

# ─── liveness: the gate must refuse to pass when it measured nothing ───

run COVERAGE_PROFILE="$TMP/absent.cov"
expect "missing profile is a hard failure" 1 "did NOT run"

: >"$TMP/empty.cov"
run COVERAGE_PROFILE="$TMP/empty.cov"
expect "empty profile is a hard failure" 1 "did NOT run"

echo "not a coverage profile at all" >"$TMP/garbage.cov"
run COVERAGE_PROFILE="$TMP/garbage.cov"
expect "profile with no mode: header is a hard failure" 1 "no 'mode:' header"

echo "mode: atomic" >"$TMP/headeronly.cov"
run COVERAGE_PROFILE="$TMP/headeronly.cov"
expect "profile with zero statement blocks is a hard failure" 1 "0 statement blocks"

# ─── the scope floor: measuring LESS must not read as doing better ───
#
# 100% coverage over 3 packages. Without MIN_PACKAGES this is the
# highest-scoring profile in the file and would sail through.
mkprofile "$TMP/narrow.cov" 3 10 10
run COVERAGE_PROFILE="$TMP/narrow.cov" COVERAGE_FLOOR=10 MIN_PACKAGES=120
expect "narrowed scope fails even at 100% coverage" 1 "The measured SCOPE shrank"

run COVERAGE_PROFILE="$TMP/narrow.cov" COVERAGE_FLOOR=10 MIN_PACKAGES=120 COVERAGE_ENFORCE=0
expect "narrowed scope fails in report-only mode too" 1 "This is a hard failure even in"

# ─── the coverage verdict: report-only by default, enforceable ───

# 200 packages, 2 of every 10 statements covered → 20.0%.
mkprofile "$TMP/low.cov" 200 2 10
run COVERAGE_PROFILE="$TMP/low.cov" COVERAGE_FLOOR=54.0 MIN_PACKAGES=120
expect "below floor warns but passes by default" 0 "BELOW FLOOR (report-only"

run COVERAGE_PROFILE="$TMP/low.cov" COVERAGE_FLOOR=54.0 MIN_PACKAGES=120 COVERAGE_ENFORCE=1
expect "below floor FAILS under COVERAGE_ENFORCE=1" 1 "is below the 54.0% floor"

# 200 packages, 8 of every 10 statements covered → 80.0%.
mkprofile "$TMP/high.cov" 200 8 10
run COVERAGE_PROFILE="$TMP/high.cov" COVERAGE_FLOOR=54.0 MIN_PACKAGES=120
expect "above floor passes" 0 "coverage-floor: OK"

run COVERAGE_PROFILE="$TMP/high.cov" COVERAGE_FLOOR=54.0 MIN_PACKAGES=120 COVERAGE_ENFORCE=1
expect "above floor passes under enforcement too" 0 "coverage-floor: OK"

# ─── the self-accounting line must carry real numbers ───
run COVERAGE_PROFILE="$TMP/high.cov" COVERAGE_FLOOR=54.0 MIN_PACKAGES=120
expect "self-accounting line reports the package count" 0 "packages=200"
run COVERAGE_PROFILE="$TMP/high.cov" COVERAGE_FLOOR=54.0 MIN_PACKAGES=120
expect "self-accounting line reports the statement count" 0 "statements=2000"
run COVERAGE_PROFILE="$TMP/high.cov" COVERAGE_FLOOR=54.0 MIN_PACKAGES=120
expect "self-accounting line reports the measured total" 0 "total=80.0%"

echo
echo "coverage-floor-test: ${pass} passed, ${fail} failed (of $((pass + fail)) cases)"
[ "$fail" -eq 0 ] || exit 1
