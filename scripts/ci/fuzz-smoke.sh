#!/usr/bin/env bash
# Fuzz smoke for Stellar Index (#340 item 5).
#
# Runs every `func Fuzz*` target in the tree for a short generative
# budget. This is a SMOKE, not a fuzzing campaign: 30s per target finds
# the shallow crashers a seed corpus does not reach, and keeps the whole
# job inside a few CI minutes. Deep runs are an operator activity
# (`go test -fuzz=FuzzX -fuzztime=1h ./pkg/`).
#
# Why the target list is DISCOVERED rather than listed
# ----------------------------------------------------
# A hardcoded list is a registry that drifts: someone adds
# `FuzzNewThing`, nobody adds it here, and it is never fuzzed while the
# job stays green. Discovery makes a new target automatically covered.
#
# The cost of discovery is that a broken pattern silently smokes NOTHING
# and still exits 0 — a gate that did not run, reporting clean by
# printing nothing. So this script REFUSES to pass on an empty
# discovery, and prints a self-accounting line (targets found / run /
# passed) that a reader can check against the tree.
#
# Failures
# --------
# A crasher makes `go test` exit non-zero AND writes the reproducing
# input to `<pkg>/testdata/fuzz/<FuzzName>/<hash>`. That file is the
# deliverable: commit it as a seed-corpus entry and the crasher becomes
# a permanent regression test that runs under plain `go test`.
#
# Usage:
#   bash scripts/ci/fuzz-smoke.sh            # 30s per target
#   FUZZTIME=5s bash scripts/ci/fuzz-smoke.sh
set -euo pipefail

FUZZTIME="${FUZZTIME:-30s}"

command -v go >/dev/null 2>&1 || {
	echo "::error::go is not on PATH — the fuzz smoke did NOT run"
	exit 1
}

cd "$(dirname "$0")/../.."

# Discover (package-dir, target) pairs. Fuzz targets live in _test.go
# files and are declared `func FuzzName(f *testing.F)`.
#
# Deliberately POSIX-portable (no mapfile / no process substitution):
# this has to be runnable on a maintainer's macOS bash 3.2 as well as on
# the ubuntu runner, or the only place it is ever exercised is CI.
HITS_FILE="$(mktemp)"
trap 'rm -f "$HITS_FILE"' EXIT

# --exclude-dir is why this greps rather than using `go list ./...`:
# .claude/worktrees holds live agent worktrees, each a full copy of this
# repo. Without the exclusion a maintainer with ten agents running
# discovers 4 real targets plus 40 duplicates from throwaway checkouts,
# burns 30s on each, and reports a failure naming paths that are not part
# of the build. CI never has them, so this would only ever break locally
# — which is exactly the kind of gate nobody trusts afterwards. vendor
# and node_modules are excluded for the same reason.
grep -rl --include='*_test.go' \
	--exclude-dir=.claude --exclude-dir=.git \
	--exclude-dir=vendor --exclude-dir=node_modules \
	-E '^func Fuzz[A-Za-z0-9_]*\(f \*testing\.F\)' . |
	sed 's#^\./##' |
	while IFS= read -r file; do
		dir="$(dirname "$file")"
		grep -oE '^func Fuzz[A-Za-z0-9_]*' "$file" |
			sed 's/^func //' |
			while IFS= read -r name; do
				printf '%s\t%s\n' "./$dir" "$name"
			done
	done | sort -u >"$HITS_FILE"

FOUND="$(wc -l <"$HITS_FILE" | tr -d ' ')"
if [ "$FOUND" -eq 0 ]; then
	echo "::error::fuzz-smoke found ZERO fuzz targets. Either the tree genuinely has none"
	echo "::error::(in which case delete this job) or the discovery pattern broke. A gate that"
	echo "::error::silently smokes nothing is worse than no gate — failing closed."
	exit 1
fi

echo "fuzz-smoke: discovered ${FOUND} target(s), budget ${FUZZTIME} each"

RUN=0
PASSED=0
FAILED_TARGETS=""

while IFS="$(printf '\t')" read -r pkg target; do
	[ -n "$pkg" ] || continue
	RUN=$((RUN + 1))
	echo "--- fuzz ${target} (${pkg}) for ${FUZZTIME}"
	# -run with a name no test matches keeps this to the generative pass;
	# the seed corpus already runs in the unit-test job. The -fuzz regex
	# is anchored so a target whose name prefixes another's cannot pull
	# in its sibling (go test refuses more than one match).
	if go test -run 'xxxNoSuchTest' -fuzz "^${target}\$" -fuzztime "${FUZZTIME}" "${pkg}"; then
		PASSED=$((PASSED + 1))
	else
		echo "::error::fuzz target ${target} in ${pkg} FAILED. The reproducing input was written"
		echo "::error::to ${pkg}/testdata/fuzz/${target}/ — commit it as a seed-corpus entry so the"
		echo "::error::crasher becomes a permanent regression test, then fix the defect."
		FAILED_TARGETS="${FAILED_TARGETS} ${pkg}:${target}"
	fi
	# The loop body runs in a subshell under `while read < file`, so the
	# counters are echoed out and re-read below rather than mutated here.
	printf '%s\t%s\t%s\n' "$RUN" "$PASSED" "$FAILED_TARGETS" >"${HITS_FILE}.tally"
done <"$HITS_FILE"

if [ -f "${HITS_FILE}.tally" ]; then
	IFS="$(printf '\t')" read -r RUN PASSED FAILED_TARGETS <"${HITS_FILE}.tally"
	rm -f "${HITS_FILE}.tally"
fi

# Self-accounting: a reader can check FOUND against
# `grep -rn 'func Fuzz' --include='*_test.go' .`. A run that smoked
# fewer targets than it discovered did not do its job.
echo "fuzz-smoke: ${PASSED} passed of ${RUN} run (${FOUND} discovered), ${FUZZTIME} each"

if [ "$RUN" -ne "$FOUND" ]; then
	echo "::error::fuzz-smoke ran ${RUN} of ${FOUND} discovered targets — the loop did not"
	echo "::error::cover everything it found. Failing closed."
	exit 1
fi

if [ -n "$(printf '%s' "$FAILED_TARGETS" | tr -d ' ')" ]; then
	echo "::error::fuzz-smoke failed:${FAILED_TARGETS}"
	exit 1
fi

if [ "$PASSED" -ne "$FOUND" ]; then
	echo "::error::fuzz-smoke: ${PASSED} passed but ${FOUND} discovered — failing closed."
	exit 1
fi
