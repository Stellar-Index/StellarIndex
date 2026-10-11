#!/usr/bin/env bash
# Coverage floor: compares total statement coverage in coverage.txt to a checked-in floor.
# Report-only by default (warning, exit 0); CI sets COVERAGE_ENFORCE=1 so a drop fails the PR.
#
# Hard failures even in report-only mode, because a gate that measured nothing must not pass:
# missing/empty profile or no `mode:` header, zero statements, a total that disagrees with
# `go tool cover -func`, or fewer packages than MIN_PACKAGES. The package floor stops
# "narrow ./... to raise the ratio"; writing tests cannot satisfy it.
#
# Usage:
#   bash scripts/ci/coverage-floor.sh                    # report-only
#   COVERAGE_ENFORCE=1 bash scripts/ci/coverage-floor.sh # fail under floor
#   COVERAGE_PROFILE=x.cov COVERAGE_FLOOR=40 MIN_PACKAGES=100 bash ...
set -euo pipefail

PROFILE="${COVERAGE_PROFILE:-coverage.txt}"
# Raise deliberately; never lower it to turn a red PR green. Headroom covers atomic-counter wobble.
FLOOR="${COVERAGE_FLOOR:-54.0}"
# Denominator floor: absorbs package churn but catches someone narrowing ./... to one subtree.
MIN_PACKAGES="${MIN_PACKAGES:-120}"

die() {
	echo "::error::$*"
	exit 1
}

[ -f "$PROFILE" ] ||
	die "coverage profile '$PROFILE' does not exist — the coverage floor did NOT run"
[ -s "$PROFILE" ] ||
	die "coverage profile '$PROFILE' is empty — the coverage floor did NOT run"
# sigpipe-ok: `head -n 1` writes one short profile header ("mode: atomic"), far under a pipe buffer.
head -n 1 "$PROFILE" | grep -q '^mode:' ||
	die "coverage profile '$PROFILE' has no 'mode:' header — not a Go coverage profile"

# Total is computed from the profile, not `go tool cover -func`, because the tool resolves
# packages against the source tree and cannot be pointed at a synthetic profile (untestable gate).
# Fields: 2 = statements in the block, 3 = hit count.
read -r TOTAL COVERED STATEMENTS <<EOF
$(awk 'NR > 1 {
		# A profile merged across packages lists a shared block once per
		# package that exercised it. `go tool cover -func` aggregates by
		# block identity (file:start,end) and counts a block covered if
		# any entry hit it; summing raw lines double-counts those blocks
		# and drifted from the tool by a rounding step. Aggregate the
		# same way so the cross-check below compares like with like.
		key = $1; if (!(key in stmts)) stmts[key] = $2; if ($3 > 0) hit[key] = 1
	}
	END {
		for (k in stmts) { tot += stmts[k]; if (k in hit) cov += stmts[k] }
		printf "%.1f %d %d\n", (tot ? 100 * cov / tot : 0), cov + 0, tot + 0
	}' "$PROFILE")
EOF

[ -n "$TOTAL" ] ||
	die "could not compute a total from '$PROFILE' — the coverage floor did NOT run"
[ "$STATEMENTS" -gt 0 ] ||
	die "coverage profile has 0 statement blocks — the coverage floor did NOT run"

# Cross-check against the tool when it can run, and judge the floor on its figure (what
# developers see locally). Gaps up to 0.1 are rounding (the tool totals by function); wider is drift.
# Not piped into head/tail: a pipeline would swallow the gate's error text.
if command -v go >/dev/null 2>&1; then
	FUNC_OUT="$(mktemp)"
	trap 'rm -f "$FUNC_OUT"' EXIT
	if go tool cover -func="$PROFILE" >"$FUNC_OUT" 2>&1; then
		TOOL_TOTAL="$(awk '$1 == "total:" { gsub(/%/, "", $NF); print $NF }' "$FUNC_OUT")"
		if [ -n "$TOOL_TOTAL" ]; then
			DRIFT="$(awk -v a="$TOTAL" -v b="$TOOL_TOTAL" 'BEGIN { d = a - b; if (d < 0) d = -d; print (d > 0.1001) ? 1 : 0 }')"
			[ "$DRIFT" -eq 0 ] ||
				die "computed total ${TOTAL}% disagrees with 'go tool cover -func' (${TOOL_TOTAL}%) by more than a rounding step.
The gate's arithmetic has drifted from the tool; fix the gate, not the floor."
			[ "$TOOL_TOTAL" = "$TOTAL" ] ||
				echo "coverage-floor: profile arithmetic ${TOTAL}% vs 'go tool cover -func' ${TOOL_TOTAL}% — a rounding step; the tool's figure is used"
			TOTAL="$TOOL_TOTAL"
		fi
	fi
fi

# Distinct packages: profile lines are <module>/<pkg>/<file>.go:<from>,<to> <stmts> <count>.
# split() rather than a bracket-expression regex, whose unescaped / breaks BSD awk.
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
