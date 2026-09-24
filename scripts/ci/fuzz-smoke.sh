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
# Scope and budget
# ----------------
# Discovery covers the whole tree (fail-closed on zero), but a PR smokes
# only the targets of the packages it changed: FUZZ_BASE (CI passes the
# PR's base sha) selects them. Unset, every target runs. The per-target
# time is min(30s, FUZZ_BUDGET / selected), never under 3s, so a wave
# that adds 160 targets still fits the job instead of being cancelled at
# the timeout with nothing reported (the tree went 5 -> 166 targets in
# one PR; 166 x 30s is 83 min). The accounting line prints the budget
# actually used.
#
# Usage:
#   bash scripts/ci/fuzz-smoke.sh                        # every target, budget-derived time
#   FUZZ_BASE=origin/main bash scripts/ci/fuzz-smoke.sh  # targets in packages changed since main
#   FUZZTIME=5s bash scripts/ci/fuzz-smoke.sh            # explicit per-target time
set -euo pipefail

FUZZ_BUDGET="${FUZZ_BUDGET:-600}" # seconds of generative fuzzing per job

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

# Discovery is driven by `git ls-files`, not a filesystem walk, and that
# is the whole trick: it lists only TRACKED files, so every ignored
# directory is invisible by construction. Agent tooling keeps live
# worktrees in an ignored dot-directory, each a full copy of this repo —
# a filesystem walk found 4 real targets plus 52 duplicates from throwaway
# checkouts, burned 30s on each, and failed naming paths that are not part
# of the build. That would only ever break on a maintainer's machine,
# which is exactly the kind of gate nobody trusts afterwards. It also
# retires a brittle --exclude-dir glob whose behaviour differed between
# GNU and BSD grep (the BSD form silently matched nothing, so the gate
# failed closed on a clean tree).
#
# Deliberately POSIX-portable below (no mapfile, no process substitution):
# this has to run on a maintainer's macOS bash 3.2 as well as on the
# ubuntu runner, or the only place it is ever exercised is CI.
git ls-files -z '*_test.go' |
	xargs -0 grep -l -E '^func Fuzz[A-Za-z0-9_]*\(f \*testing\.F\)' |
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

SELECTED="$HITS_FILE"
if [ -n "${FUZZ_BASE:-}" ]; then
	# A shallow CI checkout lacks the base commit; fetching it alone is enough
	# for a tree-to-tree diff, no history needed.
	git cat-file -e "${FUZZ_BASE}^{commit}" 2>/dev/null || git fetch -q --depth=1 origin "$FUZZ_BASE"
	DIRS_FILE="$(mktemp)"
	git diff --name-only "$FUZZ_BASE" HEAD -- '*.go' | while IFS= read -r f; do
		printf './%s\n' "$(dirname "$f")"
	done | sort -u >"$DIRS_FILE"
	SELECTED="${HITS_FILE}.selected"
	awk -F'\t' 'NR==FNR{d[$1]=1;next} ($1 in d)' "$DIRS_FILE" "$HITS_FILE" >"$SELECTED"
	rm -f "$DIRS_FILE"
fi
SEL="$(wc -l <"$SELECTED" | tr -d ' ')"
if [ "$SEL" -eq 0 ]; then
	echo "fuzz-smoke: discovered ${FOUND} target(s); none in the packages changed since ${FUZZ_BASE} — nothing to smoke"
	exit 0
fi
if [ -z "${FUZZTIME:-}" ]; then
	PER=$((FUZZ_BUDGET / SEL))
	[ "$PER" -gt 30 ] && PER=30
	[ "$PER" -lt 3 ] && PER=3
	FUZZTIME="${PER}s"
fi
echo "fuzz-smoke: discovered ${FOUND} target(s), selected ${SEL}, budget ${FUZZTIME} each"

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
done <"$SELECTED"

if [ -f "${HITS_FILE}.tally" ]; then
	IFS="$(printf '\t')" read -r RUN PASSED FAILED_TARGETS <"${HITS_FILE}.tally"
	rm -f "${HITS_FILE}.tally"
fi

# Self-accounting: a reader can check FOUND against
# `grep -rn 'func Fuzz' --include='*_test.go' .`. A run that smoked
# fewer targets than it discovered did not do its job.
echo "fuzz-smoke: ${PASSED} passed of ${RUN} run (${SEL} selected of ${FOUND} discovered), ${FUZZTIME} each"

if [ "$RUN" -ne "$SEL" ]; then
	echo "::error::fuzz-smoke ran ${RUN} of ${SEL} selected targets — the loop did not"
	echo "::error::cover everything it selected. Failing closed."
	exit 1
fi

if [ -n "$(printf '%s' "$FAILED_TARGETS" | tr -d ' ')" ]; then
	echo "::error::fuzz-smoke failed:${FAILED_TARGETS}"
	exit 1
fi

if [ "$PASSED" -ne "$SEL" ]; then
	echo "::error::fuzz-smoke: ${PASSED} passed but ${SEL} selected — failing closed."
	exit 1
fi
