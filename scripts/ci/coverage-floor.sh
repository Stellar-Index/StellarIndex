#!/usr/bin/env bash
# Coverage floor for Stellar Index (#340 item 6).
#
# `coverage.txt` has been produced by CI and uploaded as an artifact
# since the unit-test job existed, and nothing has ever read it. This
# turns it into a signal: compare total statement coverage against a
# checked-in floor and say so out loud.
#
# Report-only by default
# ----------------------
# A coverage THRESHOLD that fails the build on day one is a threshold
# people route around — they lower it, or they write a test that
# executes lines without asserting on them. So the verdict starts as a
# WARNING (exit 0). Flip it by setting COVERAGE_ENFORCE=1, once the
# number has been observed to be stable across a few weeks of PRs.
#
# What is NOT report-only
# -----------------------
# The distinction that makes this worth having: report-only applies to
# the coverage JUDGEMENT, never to the gate's own liveness. A gate that
# passes because it measured nothing is worse than no gate — this repo
# found 11 of 37 CI gates vacuous in an August 2026 audit, and the
# weekly SLA run published an eleven-week-old all-green artifact for
# months. So the following are HARD failures regardless of
# COVERAGE_ENFORCE:
#
#   - the profile is missing, empty, or has no `mode:` header
#   - the profile parses to zero statements
#   - the computed total disagrees with `go tool cover -func`, when the
#     toolchain and source tree are present for that cross-check
#   - the profile covers FEWER packages than MIN_PACKAGES
#
# That last one is the important one, and it is the reason this script
# exists rather than a one-line `awk` in the workflow. Coverage is a
# ratio, so the cheapest way to make it go up is to measure less: narrow
# `./...` to `./internal/api/...`, and the total leaps while the tree
# gets no safer. A floor on the DENOMINATOR is what stops that, and it
# cannot be satisfied by writing tests.
#
# Every run prints a self-accounting line — packages measured, statements
# counted, floor, verdict — so a reader can check the gate against the
# tree instead of trusting a green tick.
#
# Usage:
#   bash scripts/ci/coverage-floor.sh                    # report-only
#   COVERAGE_ENFORCE=1 bash scripts/ci/coverage-floor.sh # fail under floor
#   COVERAGE_PROFILE=x.cov COVERAGE_FLOOR=40 MIN_PACKAGES=100 bash ...
set -euo pipefail

PROFILE="${COVERAGE_PROFILE:-coverage.txt}"
# Raise the floor deliberately; never lower it to make a red PR green —
# that is the anti-fix this gate exists to make visible.
# Measured 55.7% over the whole tree on 2026-09-03 (go test ./...
# -covermode=atomic, 135 packages). 54.0 leaves ~1.7 points of headroom
# for the run-to-run wobble of atomic counters under -race, which is an
# order of magnitude more than that wobble actually is.
FLOOR="${COVERAGE_FLOOR:-54.0}"
# Denominator floor. 135 packages carry statements today; 120 absorbs
# ordinary package churn (and the odd package losing its last test)
# while still being far above any plausible "someone narrowed ./... to
# one subtree".
MIN_PACKAGES="${MIN_PACKAGES:-120}"

die() {
	echo "::error::$*"
	exit 1
}

[ -f "$PROFILE" ] ||
	die "coverage profile '$PROFILE' does not exist — the coverage floor did NOT run"
[ -s "$PROFILE" ] ||
	die "coverage profile '$PROFILE' is empty — the coverage floor did NOT run"
head -n 1 "$PROFILE" | grep -q '^mode:' ||
	die "coverage profile '$PROFILE' has no 'mode:' header — not a Go coverage profile"

# The total is computed FROM THE PROFILE rather than by shelling out to
# `go tool cover -func`, for one reason: `go tool cover` resolves each
# package against the source tree, so it cannot be pointed at a
# synthetic profile — which would make every interesting branch of this
# gate untestable, and an untestable gate is the thing being guarded
# against. The arithmetic is the same ratio the tool prints (covered
# statements / total statements); verified byte-equal against
# `go tool cover -func` on the real repo profile at 55.7% on 2026-09-03,
# and cross-checked on every run below whenever the toolchain can run.
#
# Field 2 is the statement count for the block, field 3 the hit count.
read -r TOTAL COVERED STATEMENTS <<EOF
$(awk 'NR > 1 { tot += $2; if ($3 > 0) cov += $2 }
	END { printf "%.1f %d %d\n", (tot ? 100 * cov / tot : 0), cov + 0, tot + 0 }' "$PROFILE")
EOF

[ -n "$TOTAL" ] ||
	die "could not compute a total from '$PROFILE' — the coverage floor did NOT run"
[ "$STATEMENTS" -gt 0 ] ||
	die "coverage profile has 0 statement blocks — the coverage floor did NOT run"

# Cross-check against the tool when it can actually run (real profile,
# real source tree, Go on PATH). Silent when unavailable — that is the
# synthetic-fixture case — but a DISAGREEMENT is a hard failure: it
# would mean this script's arithmetic has drifted from what every
# developer sees locally.
#
# NOT piped into head/tail: a gate's error text arrives on the channel a
# pipeline throws away, and this repo has been bitten by exactly that.
if command -v go >/dev/null 2>&1; then
	FUNC_OUT="$(mktemp)"
	trap 'rm -f "$FUNC_OUT"' EXIT
	if go tool cover -func="$PROFILE" >"$FUNC_OUT" 2>&1; then
		TOOL_TOTAL="$(awk '$1 == "total:" { gsub(/%/, "", $NF); print $NF }' "$FUNC_OUT")"
		if [ -n "$TOOL_TOTAL" ] && [ "$TOOL_TOTAL" != "$TOTAL" ]; then
			die "computed total ${TOTAL}% disagrees with 'go tool cover -func' (${TOOL_TOTAL}%).
The gate's arithmetic has drifted from the tool; fix the gate, not the floor."
		fi
	fi
fi

# Distinct packages represented. Profile lines look like
#   <module>/<pkg>/<file>.go:<from>,<to> <stmts> <count>
# so the package is the path with the file component stripped.
#
# The trailing component is removed with split(), not with a
# `sub(/\/[^/]*$/, ...)` regex: an unescaped `/` inside a bracket
# expression terminates the regex literal in BSD awk (macOS), and
# escaping it is not portable the other way. The first draft here used
# the regex, produced ZERO packages on macOS, and was caught only
# because the scope floor then fired — a reminder that this gate's
# fail-closed behaviour is load-bearing, not decoration.
PACKAGES="$(awk 'NR > 1 {
		sub(/:[0-9]+\.[0-9]+,.*$/, "")
		n = split($0, a, "/")
		s = a[1]
		for (i = 2; i < n; i++) s = s "/" a[i]
		print s
	}' "$PROFILE" | sort -u | wc -l | tr -d ' ')"

echo "coverage-floor: packages=${PACKAGES} statements=${STATEMENTS} covered=${COVERED} total=${TOTAL}% floor=${FLOOR}% min_packages=${MIN_PACKAGES} enforce=${COVERAGE_ENFORCE:-0}"

if [ "$PACKAGES" -lt "$MIN_PACKAGES" ]; then
	die "coverage profile covers only ${PACKAGES} packages, below the ${MIN_PACKAGES} floor.
The measured SCOPE shrank. Coverage is a ratio, so this raises the
percentage while covering less code — the failure mode this floor
exists to catch. Either the test command was narrowed from ./... or a
large part of the tree stopped building. This is a hard failure even in
report-only mode: fix the scope, do not lower MIN_PACKAGES."
fi

# Pure-shell float compare — no bc/python dependency on the runner.
UNDER="$(awk -v t="$TOTAL" -v f="$FLOOR" 'BEGIN { print (t < f) ? 1 : 0 }')"
if [ "$UNDER" = "1" ]; then
	MSG="total coverage ${TOTAL}% is below the ${FLOOR}% floor (scripts/ci/coverage-floor.sh).
Add tests for what you changed, or — if the drop is a deliberate,
reviewed consequence of deleting well-covered code — lower COVERAGE_FLOOR
in the same PR and say why in the commit message."
	if [ "${COVERAGE_ENFORCE:-0}" = "1" ]; then
		die "$MSG"
	fi
	echo "::warning::$MSG"
	echo "coverage-floor: BELOW FLOOR (report-only; set COVERAGE_ENFORCE=1 to make this fail)"
	exit 0
fi

echo "coverage-floor: OK — ${TOTAL}% >= ${FLOOR}% across ${PACKAGES} packages"
