#!/usr/bin/env bash
# Fuzz smoke for Stellar Index.
#
# Runs every `func Fuzz*` target in the tree for a short generative budget.
# A SMOKE, not a campaign: 30s per target finds shallow crashers the seed
# corpus misses and keeps the job inside a few CI minutes. Deep runs are an
# operator activity (`go test -fuzz=FuzzX -fuzztime=1h ./pkg/`).
#
# Targets are DISCOVERED, not listed: a hardcoded list drifts (someone adds
# `FuzzNewThing`, nobody adds it here, it is never fuzzed and the job stays
# green). The cost is that a broken pattern would smoke NOTHING and exit 0, so
# the script REFUSES to pass on an empty discovery and prints a self-accounting
# line (found / run / passed) a reader can check against the tree.
#
# A crasher makes `go test` exit non-zero AND writes the reproducing input to
# `<pkg>/testdata/fuzz/<FuzzName>/<hash>`. Commit that file as a seed-corpus
# entry and the crasher becomes a permanent regression test under plain
# `go test`.
#
# Scope and budget: discovery covers the whole tree (fail-closed on zero), but
# a PR smokes only targets in packages it changed; FUZZ_BASE (CI passes the
# PR's base sha) selects them, unset runs all. Per-target time is
# min(30s, FUZZ_BUDGET / selected), never under 8s: -fuzztime covers worker
# start-up (~2s on a loaded runner) and a 3s floor produced a phantom "context
# deadline exceeded" crasher. The cap keeps a wave that adds 160 targets inside
# the job timeout (the tree went 5 -> 166 in one PR; 166 x 30s is 83 min)
# instead of being cancelled with nothing reported. The accounting line prints
# the budget actually used.
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

# Discover (package-dir, target) pairs: targets are `func FuzzName(f *testing.F)`
# in _test.go files.
HITS_FILE="$(mktemp)"
trap 'rm -f "$HITS_FILE"' EXIT

# Driven by `git ls-files`, not a filesystem walk: only TRACKED files are listed,
# so ignored directories are invisible by construction. Agent tooling keeps full
# repo copies in an ignored dot-directory; a walk found 4 real targets plus 52
# duplicates, burned 30s on each and failed naming paths outside the build,
# breaking only on a maintainer's machine. It also retires an --exclude-dir glob
# that differed between GNU and BSD grep (BSD matched nothing, so the gate failed
# closed on a clean tree).
#
# POSIX-portable on purpose (no mapfile, no process substitution): it must run
# on macOS bash 3.2 as well as the ubuntu runner, or CI is the only place it is
# ever exercised.
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
# CI checks out refs/pull/N/merge; its first parent is the base tip this merge was built
# on, while the event's base sha is the tip at PR creation and goes stale as main moves.
if [ -n "${FUZZ_BASE:-}" ] && git rev-parse -q --verify 'HEAD^2' >/dev/null 2>&1; then
	FUZZ_BASE="$(git rev-parse 'HEAD^1')"
fi
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
	[ "$PER" -lt 8 ] && PER=8
	FUZZTIME="${PER}s"
fi
echo "fuzz-smoke: discovered ${FOUND} target(s), selected ${SEL}, budget ${FUZZTIME} each"

corpus_size() {
	if [ -d "$1" ]; then find "$1" -type f | wc -l; else echo 0; fi
}

fuzz_once() {
	go test -run 'xxxNoSuchTest' -fuzz "^${2}\$" -fuzztime "${FUZZTIME}" "$1"
}

RUN=0
PASSED=0
FAILED_TARGETS=""

while IFS="$(printf '\t')" read -r pkg target; do
	[ -n "$pkg" ] || continue
	RUN=$((RUN + 1))
	echo "--- fuzz ${target} (${pkg}) for ${FUZZTIME}"
	# -run with a name no test matches keeps this to the generative pass (the seed
	# corpus runs in the unit-test job). The -fuzz regex is anchored so a target
	# whose name prefixes another's cannot match its sibling (go test refuses >1).
	corpus="${pkg}/testdata/fuzz/${target}"
	before=$(corpus_size "$corpus")
	if fuzz_once "$pkg" "$target"; then
		PASSED=$((PASSED + 1))
	elif [ "$(corpus_size "$corpus")" -eq "$before" ] && fuzz_once "$pkg" "$target"; then
		# A crasher always writes its input. A failure that wrote none is go's
		# fuzz coordinator hitting -fuzztime while stopping workers ("context
		# deadline exceeded"); one clean re-run clears it, a second failure does not.
		echo "::warning::fuzz target ${target} in ${pkg} failed without writing an input and passed on re-run"
		PASSED=$((PASSED + 1))
	else
		echo "::error::fuzz target ${target} in ${pkg} FAILED. The reproducing input was written"
		echo "::error::to ${pkg}/testdata/fuzz/${target}/ — commit it as a seed-corpus entry so the"
		echo "::error::crasher becomes a permanent regression test, then fix the defect."
		FAILED_TARGETS="${FAILED_TARGETS} ${pkg}:${target}"
	fi
	# The loop body runs in a subshell (`while read < file`), so counters are
	# echoed out and re-read below rather than mutated here.
	printf '%s\t%s\t%s\n' "$RUN" "$PASSED" "$FAILED_TARGETS" >"${HITS_FILE}.tally"
done <"$SELECTED"

if [ -f "${HITS_FILE}.tally" ]; then
	IFS="$(printf '\t')" read -r RUN PASSED FAILED_TARGETS <"${HITS_FILE}.tally"
	rm -f "${HITS_FILE}.tally"
fi

# Self-accounting: check FOUND against `grep -rn 'func Fuzz' --include='*_test.go' .`.
# A run that smoked fewer targets than it discovered did not do its job.
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
