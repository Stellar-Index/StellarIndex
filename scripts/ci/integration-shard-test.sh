#!/usr/bin/env bash
# integration-shard-test.sh — fixture tests for scripts/ci/integration-shard.sh
# (the 4-way split of ci.yml's `integration tests (Docker)` job, 2026-08-29).
#
# The property that matters: the N shards PARTITION the test listing —
# every test lands in exactly one shard, no shard is empty, and the split is
# deterministic (each shard derives the same list independently, so a test
# that appears in no shard must be impossible). A shard that silently ran
# nothing would be a green with no basis, so the empty / bad-arg paths must
# fail loudly too.
#
# The stub listing carries `TestÜber`: Go identifiers admit Unicode letters,
# and the pre-#333 ASCII-only filter dropped such a test from the listing
# entirely — it then appeared in no shard's -run regex and ran nowhere. The
# union assertion below is what catches that.
#
# Section 6 pins the OTHER half of #333 F1: the shard-0-only package list is
# DERIVED from the Makefile's INT_TEST_PKGS (`make print-int-test-pkgs`)
# rather than hand-copied, so adding a package to the Makefile cannot leave
# it executed by no shard. Fixture makefiles prove it tracks additions and
# fails closed on divergence.
#
# Uses a stub listing (INTEGRATION_SHARD_LIST_FILE) and dry-run mode — no
# Docker, no network, no go build.
#
# Run: bash scripts/ci/integration-shard-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SUT="$PWD/scripts/ci/integration-shard.sh"
[[ -x "$SUT" ]] || { echo "integration-shard-test: missing/not executable $SUT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# Stub listing: 11 tests (NOT a multiple of 4), deliberately unsorted, one
# with a Unicode identifier (legal Go, and listable), plus an `ok` trailer
# line that the filter must drop.
STUB="$TMP/list.txt"
cat >"$STUB" <<'L'
TestZeta
TestAlpha
TestGamma_Two
TestBeta
TestDelta
TestEpsilon
TestEta
TestTheta
TestIota
TestKappa
TestÜber
ok  	github.com/Stellar-Index/StellarIndex/test/integration	0.5s
L
expected_sorted="$(grep -E '^Test' "$STUB" | LC_ALL=C sort)"
N=4

shard() { # shard IDX COUNT -> names on stdout, regex captured to $TMP/regex.IDX
  INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 \
    "$SUT" "$1" "$2" 2>"$TMP/stderr.$1"
}

# ─── 1. the N shards partition the listing exactly ─────────────────────
union="$TMP/union.txt"; : >"$union"
all_nonempty=1
for i in $(seq 0 $((N - 1))); do
  out="$(shard "$i" "$N")" || { bad "shard $i/$N exited non-zero"; all_nonempty=0; continue; }
  [[ -n "$out" ]] || { bad "shard $i/$N produced no tests"; all_nonempty=0; }
  printf '%s\n' "$out" >>"$union"
done
[[ $all_nonempty -eq 1 ]] && ok "every one of $N shards is non-empty"

if [[ "$(LC_ALL=C sort "$union")" == "$expected_sorted" ]]; then
  ok "union of $N shards == sorted listing (every test in exactly one shard; trailer line dropped)"
else
  bad "union of shards != listing"; diff <(LC_ALL=C sort "$union") <(printf '%s\n' "$expected_sorted") || true
fi
dups="$(LC_ALL=C sort "$union" | uniq -d)"
if [[ -z "$dups" ]]; then ok "no test appears in two shards"; else bad "duplicated across shards: $dups"; fi
if [[ "$(wc -l <"$union" | tr -d ' ')" == "11" ]]; then ok "shard sizes sum to 11"; else bad "shard sizes do not sum to 11"; fi
# Named explicitly so the failure reads as what it is, not as an off-by-one.
if grep -qx 'TestÜber' "$union"; then ok "a Unicode-named test lands in a shard"; else bad "TestÜber is in NO shard — the listing filter dropped it"; fi

# ─── 2. deterministic: same inputs, same slice ──────────────────────────
a="$(shard 1 "$N")"; b="$(shard 1 "$N")"
if [[ "$a" == "$b" ]]; then ok "shard 1 is byte-identical across two runs"; else bad "shard 1 differs between runs"; fi

# ─── 3. the -run regex is anchored and lists exactly the slice ──────────
regex="$(grep -o -- '-run .*' "$TMP/stderr.2" | sed 's/^-run //')"
want="^($(shard 2 "$N" | paste -sd '|' -))\$"
if [[ "$regex" == "$want" ]]; then ok "shard 2 -run regex == '^(names|joined)\$'"; else bad "regex '$regex' != '$want'"; fi

# ─── 4. fail-closed paths ───────────────────────────────────────────────
expect_fail() { # expect_fail DESC CMD...
  local desc="$1"; shift
  if "$@" >/dev/null 2>"$TMP/err"; then
    bad "$desc — exited 0"
  elif grep -q 'integration-shard: FAIL' "$TMP/err"; then
    ok "$desc — exits non-zero with a FAIL line"
  else
    bad "$desc — exited non-zero but without a FAIL line: $(cat "$TMP/err")"
  fi
}
printf 'TestOnlyOne\nTestOnlyTwo\n' >"$TMP/two.txt"
expect_fail "fewer tests (2) than shards (4)" \
  env INTEGRATION_SHARD_LIST_FILE="$TMP/two.txt" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" 3 4
: >"$TMP/empty.txt"
expect_fail "empty listing" \
  env INTEGRATION_SHARD_LIST_FILE="$TMP/empty.txt" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" 0 1
printf 'ok  \tpkg\t0.1s\n' >"$TMP/trailer.txt"
expect_fail "listing with only a trailer line" \
  env INTEGRATION_SHARD_LIST_FILE="$TMP/trailer.txt" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" 0 1
expect_fail "index == count" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" 4 4
expect_fail "non-integer index" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" x 4
expect_fail "zero shards" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" 0 0
expect_fail "missing args" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" 0
expect_fail "unreadable list file" \
  env INTEGRATION_SHARD_LIST_FILE="$TMP/nope.txt" INTEGRATION_SHARD_DRY_RUN=1 "$SUT" 0 4

# ─── 5. count=1 is the whole suite (local single-shard use) ─────────────
one="$(shard 0 1)"
if [[ "$one" == "$expected_sorted" ]]; then ok "1 shard == the full sorted listing"; else bad "1 shard != listing"; fi

# ─── 6. EXTRA_PKGS is derived from the Makefile, and fails closed ───────
# The shard-0-only package list used to be a hand-copied literal guarded by
# a "keep in lockstep" comment; a package added to INT_TEST_PKGS then ran
# under `make test-integration` and compiled under `make
# test-integration-build`, but no shard executed it (#333 F1). It is now
# read from `make print-int-test-pkgs` at run time — these fixtures pin
# that it TRACKS the Makefile rather than merely matching it today.
mkmakefile() { # mkmakefile FILE PKGS...
  local f="$1"; shift
  { printf 'INT_TEST_PKGS := %s\n' "$*"
    printf 'print-int-test-pkgs:\n\t@echo $(INT_TEST_PKGS)\n'; } >"$f"
}
# extras_of FILE -> the shard-0-only list the script reports for that makefile
extras_of() {
  INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 \
    INTEGRATION_SHARD_MAKEFILE="$1" "$SUT" 0 4 2>&1 >/dev/null |
    sed -n 's/^integration-shard: shard-0-only packages from [^:]*: //p'
}

mkmakefile "$TMP/mk3" './test/integration/...' './cmd/stellarindex-ops/...' './internal/ops/archive/...'
got="$(extras_of "$TMP/mk3")"
if [[ "$got" == "./cmd/stellarindex-ops/... ./internal/ops/archive/..." ]]; then
  ok "shard-0 extras == INT_TEST_PKGS minus the sharded package"
else
  bad "extras '$got' != './cmd/stellarindex-ops/... ./internal/ops/archive/...'"
fi

# The mutation the old hand-copied literal could not see: a 4th package.
mkmakefile "$TMP/mk4" './test/integration/...' './cmd/stellarindex-ops/...' './internal/ops/archive/...' './internal/newly/added/...'
got="$(extras_of "$TMP/mk4")"
if [[ "$got" == *"./internal/newly/added/..."* ]]; then
  ok "a package added to INT_TEST_PKGS appears in shard 0's extras"
else
  bad "added package missing from extras: '$got'"
fi

# Sharded package absent -> the Makefile target and the shards have
# diverged; running would test a different suite than it claims to.
mkmakefile "$TMP/mk_nosharded" './cmd/stellarindex-ops/...'
expect_fail "INT_TEST_PKGS without the sharded package" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 \
      INTEGRATION_SHARD_MAKEFILE="$TMP/mk_nosharded" "$SUT" 0 4
mkmakefile "$TMP/mk_empty" ''
expect_fail "INT_TEST_PKGS resolving empty" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 \
      INTEGRATION_SHARD_MAKEFILE="$TMP/mk_empty" "$SUT" 0 4
printf 'other:\n\t@echo hi\n' >"$TMP/mk_notarget"
expect_fail "makefile without the print-int-test-pkgs target" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 \
      INTEGRATION_SHARD_MAKEFILE="$TMP/mk_notarget" "$SUT" 0 4
expect_fail "unreadable makefile" \
  env INTEGRATION_SHARD_LIST_FILE="$STUB" INTEGRATION_SHARD_DRY_RUN=1 \
      INTEGRATION_SHARD_MAKEFILE="$TMP/nope.mk" "$SUT" 0 4

# The repo's own Makefile must satisfy the same contract (this is the
# parity check itself: the real INT_TEST_PKGS lists the sharded package
# and resolves non-empty).
got="$(extras_of Makefile)"
if [[ -n "$got" && "$got" != *"./test/integration/..."* ]]; then
  ok "the repo Makefile's INT_TEST_PKGS resolves ($got)"
else
  bad "repo Makefile derivation looks wrong: '$got'"
fi

echo "integration-shard-test: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
