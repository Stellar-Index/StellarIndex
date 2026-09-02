#!/usr/bin/env bash
# lint-replay-plan-test.sh — fixture tests for the replay-plan tripwire.
#
# lint-replay-plan.sh is the gate that stops a decoder / asset allow-list
# change from landing without stating what happens to already-served
# history (the 2026-08-27 e17288bd fiat widening: 190,228 rows missing
# for a day, no replay planned). Its verdict has to be a function of the
# inputs and nothing else — so its behaviour is pinned here rather than
# assumed:
#
#   - a watched-path change with no `Replay-Plan:` trailer fails — for
#     the canonical allow-list, a decode.go, a decode_<sibling>.go and a
#     venue pairs.go allow-list alike;
#   - a watched-path change declared with `Replay-Plan: <plan>` passes;
#   - `Replay-Plan: none — <reason>` passes (no served history affected);
#   - a trailer with no value, or a bare `none` with no reason, does not
#     count;
#   - the trailer may sit on ANY commit in the range (one plan per range);
#   - a change to an un-watched file (incl. a decoder's *_test.go) needs
#     no trailer;
#   - no BASE_SHA (or a BASE_SHA outside this history) skips, rather
#     than failing a first push;
#   - and the verdict does not depend on how BIG the commit range is (the
#     SIGPIPE-under-pipefail class lint-baseline-growth-test.sh pins).
#
# Plus the wrong-command ADVISORY (#333): a plan that replays a PROJECTED
# source with backfill/ch-rebuild warns (never fails), the projected
# commands do not, and the projector source names are DERIVED from the
# fixture's own registry rather than listed in the gate.
#
# Run: bash scripts/ci/lint-replay-plan-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/lint-replay-plan.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# mkrepo <bulk-commits> — build a throwaway repo carrying a copy of the
# gate at the same relative path the real one lives at (the gate cds to
# `dirname $0/../..`, so the layout is what points it at the fixture
# rather than at this repo). Seeds one file per watched class plus an
# un-watched neighbour. Leaves BASE set to the seed commit.
mkrepo() {
  local bulk="${1:-0}"
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo/scripts/ci" "$TMP/repo/internal/canonical" \
           "$TMP/repo/internal/sources/demo" "$TMP/repo/internal/projector"
  cp "$GATE" "$TMP/repo/scripts/ci/lint-replay-plan.sh"
  (
    cd "$TMP/repo" || exit 1
    git init -q .
    git config user.email t@t.invalid
    git config user.name t
    git config commit.gpgsign false
    printf 'package canonical\n\nvar knownFiatCodes = map[string]struct{}{"USD": {}}\n' \
      > internal/canonical/asset_fiat.go
    printf 'package demo\n\nfunc decode() {}\n' > internal/sources/demo/decode.go
    printf 'package demo\n\nfunc decodeRewards() {}\n' > internal/sources/demo/decode_rewards.go
    printf 'package demo\n\nfunc DefaultPairs() {}\n' > internal/sources/demo/pairs.go
    printf 'package demo\n\nfunc TestDecode() {}\n' > internal/sources/demo/decode_test.go
    printf 'package demo\n\nfunc helper() {}\n' > internal/sources/demo/helper.go
    # The projector registry + the source constant it refers to, so the
    # wrong-command advisory has a real derivation to resolve (it reads
    # the repo it is run in, not this one). `other` is deliberately NOT
    # in the registry: a non-projected name must not warn.
    printf 'package demo\n\nconst SourceName = "demo"\n' > internal/sources/demo/consumer.go
    printf 'package projector\n\nfunc buildSource(name string) {\n\tswitch name {\n\tcase demo.SourceName:\n\t}\n}\n' \
      > internal/projector/registry.go
    git add -A
    git commit -qm "base"
    git rev-parse HEAD > "$TMP/base"
    local i
    for ((i = 0; i < bulk; i++)); do
      printf 'filler %d\n' "$i" > "filler$i.txt"
      git add -A
      {
        printf 'chore: filler %d\n\n' "$i"
        for ((j = 0; j < 100; j++)); do
          printf 'padding line %d %d aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' "$i" "$j"
        done
      } > "$TMP/msg"
      git commit -q -F "$TMP/msg"
    done
  )
  BASE="$(cat "$TMP/base")"
}

# touch_commit <file> <commit-message> — append a line to <file> in the
# fixture repo and commit it with <commit-message>.
touch_commit() {
  (
    cd "$TMP/repo" || exit 1
    printf '// changed %s\n' "$RANDOM" >> "$1"
    git add -A
    printf '%s\n' "$2" > "$TMP/cmsg"
    git commit -q -F "$TMP/cmsg"
  )
}

# runGate [base] → sets RC + OUT
runGate() {
  local base="${1-$BASE}"
  OUT="$(cd "$TMP/repo" && BASE_SHA="$base" bash scripts/ci/lint-replay-plan.sh 2>&1)"
  RC=$?
}

# expect <name> <want-rc> [want-substring]
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [[ "$RC" -ne "$want_rc" ]]; then
    echo "FAIL: $name — rc=$RC want=$want_rc"
    echo "$OUT" | sed 's/^/    | /' | head -12
    fail=$((fail + 1))
    return
  fi
  if [[ -n "$want_sub" && "$OUT" != *"$want_sub"* ]]; then
    echo "FAIL: $name — output missing: $want_sub"
    echo "$OUT" | sed 's/^/    | /' | head -12
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

UNDECLARED="UNDECLARED REPLAY PLAN"
DECLARED="declared its replay plan"
NOTHING="nothing to declare"

# --- 1. the incident shape: allow-list widened, no plan ----------------
mkrepo 0
touch_commit internal/canonical/asset_fiat.go "canonical: widen fiat allow-list"
runGate
expect "fiat allow-list change without a plan fails" 1 "$UNDECLARED"
expect "the changed file is named" 1 "~ internal/canonical/asset_fiat.go"

# --- 2. decoder change, no plan --------------------------------------
mkrepo 0
touch_commit internal/sources/demo/decode.go "fix(demo): decode the new event variant"
runGate
expect "decoder change without a plan fails" 1 "~ internal/sources/demo/decode.go"

# --- 2b. a decoder sibling (decode_<x>.go) is watched too --------------
mkrepo 0
touch_commit internal/sources/demo/decode_rewards.go "fix(demo): decode reward events"
runGate
expect "decode_<sibling>.go change without a plan fails" 1 "~ internal/sources/demo/decode_rewards.go"

# --- 2c. a venue pair allow-list is the asset_fiat class ---------------
mkrepo 0
touch_commit internal/sources/demo/pairs.go "feat(demo): add three more pairs"
runGate
expect "venue pairs.go widening without a plan fails" 1 "~ internal/sources/demo/pairs.go"

# --- 3. declared plan passes ------------------------------------------
mkrepo 0
touch_commit internal/canonical/asset_fiat.go "$(printf 'canonical: widen fiat allow-list\n\nReplay-Plan: stellarindex-ops projector-replay -source reflector-fx -from 61602787 on r1 after deploy')"
runGate
expect "declared plan passes" 0 "$DECLARED"

# --- 4. `none — reason` passes ----------------------------------------
mkrepo 0
touch_commit internal/sources/demo/decode.go "$(printf 'refactor(demo): split decode helpers\n\nReplay-Plan: none — refactor only; golden output unchanged')"
runGate
expect "none-with-reason passes" 0 "$DECLARED"

# --- 5. empty trailer / bare none do not count ------------------------
mkrepo 0
touch_commit internal/sources/demo/decode.go "$(printf 'fix(demo): x\n\nReplay-Plan:   ')"
runGate
expect "empty trailer value rejected" 1 "$UNDECLARED"
mkrepo 0
touch_commit internal/sources/demo/decode.go "$(printf 'fix(demo): x\n\nReplay-Plan: none')"
runGate
expect "bare none rejected" 1 "$UNDECLARED"
mkrepo 0
touch_commit internal/sources/demo/decode.go "$(printf 'fix(demo): x\n\nReplay-Plan: none —')"
runGate
expect "none with dash but no reason rejected" 1 "$UNDECLARED"

# --- 6. the trailer may live on any commit in the range ----------------
mkrepo 0
touch_commit internal/sources/demo/decode.go "fix(demo): decode the new variant"
touch_commit internal/sources/demo/helper.go "$(printf 'docs(demo): state the replay\n\nReplay-Plan: replay demo from ledger 1 on r1 (ops ticket #1)')"
runGate
expect "plan on a later commit in the range counts" 0 "$DECLARED"

# --- 7. un-watched changes need no plan --------------------------------
mkrepo 0
touch_commit internal/sources/demo/helper.go "chore(demo): tidy a helper"
touch_commit internal/sources/demo/decode_test.go "test(demo): add a case"
runGate
expect "un-watched + *_test.go changes pass" 0 "$NOTHING"

# --- 8. no BASE_SHA / unknown BASE_SHA skips ---------------------------
mkrepo 0
touch_commit internal/canonical/asset_fiat.go "canonical: widen fiat allow-list"
runGate ""
expect "no BASE_SHA skips" 0 "skipping"
runGate "0000000000000000000000000000000000000000"
expect "zero BASE_SHA skips" 0 "skipping"
runGate "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
expect "unknown BASE_SHA skips" 0 "not in local history"

# --- 9. verdict must not depend on range size --------------------------
mkrepo 40
touch_commit internal/canonical/asset_fiat.go "$(printf 'canonical: widen, large range\n\nReplay-Plan: replay fx_quotes 2026-06-01.. on r1')"
runGate
expect "declared plan passes on a large commit range" 0 "$DECLARED"
mkrepo 40
touch_commit internal/canonical/asset_fiat.go "canonical: widen, large range, no plan"
runGate
expect "undeclared change still fails on a large commit range" 1 "$UNDECLARED"

# --- 10. wrong-command advisory (warn only, never a failure) -----------
WARN_DEMO="WARNING: the plan replays projected source 'demo'"
mkrepo 0
touch_commit internal/sources/demo/decode.go "$(printf 'fix(demo): widen the decoder\n\nReplay-Plan: stellarindex-ops ch-rebuild -sources demo -from 1 -to 2 -write on r1')"
runGate
expect "ch-rebuild on a projected source still PASSES (advisory, not a gate)" 0 "$DECLARED"
expect "ch-rebuild on a projected source warns" 0 "$WARN_DEMO"
expect "the advisory reports how many source names it resolved" 0 "cross-checked the declared plan against 1 projector source name"

mkrepo 0
touch_commit internal/sources/demo/decode.go "$(printf 'fix(demo): widen the decoder\n\nReplay-Plan: stellarindex-ops projector-replay -source demo -from 61602787 on r1')"
runGate
if [[ "$OUT" == *"$WARN_DEMO"* ]]; then
  echo "FAIL: projector-replay on a projected source must not warn"
  fail=$((fail + 1))
else
  echo "ok: projector-replay on a projected source does not warn"
  pass=$((pass + 1))
fi

mkrepo 0
touch_commit internal/sources/demo/decode.go "$(printf 'fix(demo): widen the decoder\n\nReplay-Plan: stellarindex-ops ch-rebuild -sdex -sources sdex -from 1 -to 2 -write')"
runGate
if [[ "$OUT" == *"WARNING: the plan replays projected source"* ]]; then
  echo "FAIL: a NON-projected source (sdex) must not warn about ch-rebuild"
  fail=$((fail + 1))
else
  echo "ok: ch-rebuild on a non-projected source does not warn"
  pass=$((pass + 1))
fi

echo
echo "lint-replay-plan-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
