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

# Stub listing: 10 tests (NOT a multiple of 4), deliberately unsorted, with
# an `ok` trailer line and a subtest-looking line that `go test -list` never
# emits at top level but a sloppy filter might keep.
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
if [[ "$(wc -l <"$union" | tr -d ' ')" == "10" ]]; then ok "shard sizes sum to 10"; else bad "shard sizes do not sum to 10"; fi

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

echo "integration-shard-test: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
