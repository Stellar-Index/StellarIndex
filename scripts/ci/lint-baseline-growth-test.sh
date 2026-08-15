#!/usr/bin/env bash
# lint-baseline-growth-test.sh — fixture tests for the anti-self-bypass
# tripwire.
#
# lint-baseline-growth.sh is the gate that stops a commit from growing a
# lint baseline in the same change that introduces the violation it hides
# (CS-098). Its verdict has to be a function of the inputs and nothing
# else — so its behaviour is pinned here rather than assumed:
#
#   - undeclared growth fails;
#   - growth declared with a `Baseline-Growth: <file> — <reason>` trailer
#     that NAMES the grown file passes;
#   - a trailer with no reason after the colon does not count;
#   - a trailer that names a DIFFERENT watched file does NOT excuse growth
#     in an un-named one (CID-1 per-file scoping — audit 2026-08-14);
#   - on direct-push, an undeclared growth cannot HEAL by being pushed
#     over: a BASE_SHA sitting on the failed growth tip is re-derived back
#     to the last clean/declared base and the growth is still caught
#     (CID-1 anti-heal — audit 2026-08-14);
#   - no BASE_SHA (or a BASE_SHA outside this history) skips, rather
#     than failing a first push;
#   - and the verdict does not depend on how BIG the commit range is.
#
# The range-size case reproduces a real CI failure: the trailer check used
# to be `git log --format=%B "$BASE..HEAD" | grep -qE ...` under
# `set -o pipefail`. `grep -q` exits at the first match and closes the
# pipe; once the log exceeded the 64KB pipe buffer, `git log` took SIGPIPE
# and reported 141, and pipefail surfaced that as the pipeline's status —
# so the `if` read "trailer found" as "no trailer" and failed a PR that had
# correctly declared its growth. PR #38 passed locally and failed in CI on
# byte-identical inputs. The big-range case below reproduces it
# deterministically.
#
# Run: bash scripts/ci/lint-baseline-growth-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/lint-baseline-growth.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# mkrepo <bulk-commits> — build a throwaway repo carrying a copy of the
# gate at the same relative path the real one lives at (the gate cds to
# `dirname $0/../..`, so the layout is what points it at the fixture
# rather than at this repo). Leaves BASE set to the pre-growth commit.
mkrepo() {
  local bulk="${1:-0}"
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo/scripts/ci"
  cp "$GATE" "$TMP/repo/scripts/ci/lint-baseline-growth.sh"
  (
    cd "$TMP/repo" || exit 1
    git init -q .
    git config user.email t@t.invalid
    git config user.name t
    git config commit.gpgsign false
    printf 'existing-entry\n' > scripts/ci/demo.baseline
    # Seed the two gitleaks allowlists so a later add reads as GROWTH,
    # mirroring how they exist on the real default branch (W5-ci-2). The
    # tripwire is a line-diff grep, not a TOML/fingerprint parser, so the
    # minimal shapes below are enough to exercise it.
    printf '# fingerprints\ncafe0000cafe0000cafe0000cafe0000cafe0000:x_test.go:generic-api-key:1\n' \
      > .gitleaksignore
    printf '[allowlist]\npaths = [\n  %s,\n]\n' "'''^docs/archive/'''" > .gitleaks.toml
    git add -A
    git commit -qm "base"
    BASE_OUT="$(git rev-parse HEAD)"
    echo "$BASE_OUT" > "$TMP/base"
    # Bulk filler commits, each with a large message. These sit BELOW the
    # growth commit in `git log` (newest-first) output, so they are the
    # bytes the old implementation was still writing when `grep -q` quit.
    local i
    for ((i = 0; i < bulk; i++)); do
      printf 'filler %d\n' "$i" > "filler$i.txt"
      git add -A
      {
        printf 'chore: filler %d\n\n' "$i"
        # ~8KB of body per commit.
        for ((j = 0; j < 100; j++)); do
          printf 'padding line %d %d aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' "$i" "$j"
        done
      } > "$TMP/msg"
      git commit -q -F "$TMP/msg"
    done
  )
  BASE="$(cat "$TMP/base")"
}

# grow <commit-message> — add an entry to the baseline and commit it.
# Records the growth commit's sha in $TMP/grow_sha.
grow() {
  (
    cd "$TMP/repo" || exit 1
    printf 'existing-entry\nnewly-added-entry\n' > scripts/ci/demo.baseline
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
    git rev-parse HEAD > "$TMP/grow_sha"
  )
  GROW_SHA="$(cat "$TMP/grow_sha")"
}

# unrelated <commit-message> — a commit that touches NO watched allowlist
# (models the innocuous push that lands on top of a failed growth).
unrelated() {
  (
    cd "$TMP/repo" || exit 1
    printf 'noise %s\n' "$RANDOM" > unrelated.txt
    git add -A
    printf '%s\n' "$1" > "$TMP/umsg"
    git commit -q -F "$TMP/umsg"
  )
}

# grow_ignore <commit-message> — add a secret-scan fingerprint to
# .gitleaksignore (W5-ci-2: the "silence a real leak in-diff" attack).
grow_ignore() {
  (
    cd "$TMP/repo" || exit 1
    printf '%s\n' 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef:internal/leak_test.go:generic-api-key:12' \
      >> .gitleaksignore
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# grow_toml <commit-message> — add a broad path allowlist entry to
# .gitleaks.toml (a quote-prefixed array element that widens what the
# scanner ignores).
grow_toml() {
  (
    cd "$TMP/repo" || exit 1
    printf '%s\n' "  '''internal/'''," >> .gitleaks.toml
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# grow_both <commit-message> — grow .gitleaksignore AND .gitleaks.toml in
# ONE commit (the CID-1 blanket-approval attack: two allowlists widened,
# one benign trailer).
grow_both() {
  (
    cd "$TMP/repo" || exit 1
    printf '%s\n' 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef:internal/leak_test.go:generic-api-key:12' \
      >> .gitleaksignore
    printf '%s\n' "  '''internal/'''," >> .gitleaks.toml
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# comment_toml <commit-message> — a NON-widening .gitleaks.toml edit (a
# comment line): must NOT be flagged as allowlist growth.
comment_toml() {
  (
    cd "$TMP/repo" || exit 1
    printf '%s\n' '# clarify why an existing path is allowlisted' >> .gitleaks.toml
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# runGate [base] → sets RC + OUT
runGate() {
  local base="${1-$BASE}"
  OUT="$(cd "$TMP/repo" && BASE_SHA="$base" bash scripts/ci/lint-baseline-growth.sh 2>&1)"
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

# expect_missing <name> <want-substring-absent>
expect_missing() {
  local name="$1" nosub="$2"
  if [[ "$OUT" == *"$nosub"* ]]; then
    echo "FAIL: $name — output unexpectedly contains: $nosub"
    echo "$OUT" | sed 's/^/    | /' | head -12
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# --- 1. undeclared growth fails -------------------------------------
mkrepo 0
grow "feat: sneak an entry in"
runGate
expect "undeclared growth fails" 1 "UNDECLARED GROWTH: scripts/ci/demo.baseline"

# --- 2. declared growth passes (trailer names the file) -------------
mkrepo 0
grow "$(printf 'feat: legitimate\n\nBaseline-Growth: scripts/ci/demo.baseline — grandfathering a known set')"
runGate
expect "declared growth passes" 0 "no undeclared"

# --- 3. a trailer with no reason does not count ----------------------
mkrepo 0
grow "$(printf 'feat: empty declaration\n\nBaseline-Growth:   ')"
runGate
expect "empty trailer value rejected" 1 "UNDECLARED GROWTH: scripts/ci/demo.baseline"

# --- 4. no BASE_SHA / unknown BASE_SHA skips ------------------------
mkrepo 0
grow "feat: sneak an entry in"
runGate ""
expect "no BASE_SHA skips" 0 "skipping"
runGate "0000000000000000000000000000000000000000"
expect "zero BASE_SHA skips" 0 "skipping"
runGate "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
expect "unknown BASE_SHA skips" 0 "not in local history"

# --- 5. THE SIGPIPE REGRESSION: verdict must not depend on range size
# 40 filler commits x ~8KB ≈ 320KB of log body, far past the 64KB pipe
# buffer, with the declaring commit NEWEST so the match lands on the
# first line and the writer is still streaming when grep quits. Against
# the piped `grep -q` implementation this fails; it must pass.
mkrepo 40
grow "$(printf 'feat: legitimate, large range\n\nBaseline-Growth: scripts/ci/demo.baseline — grandfathering a known set')"
runGate
expect "declared growth passes on a large commit range" 0 "no undeclared"

# --- 6. .gitleaksignore fingerprint growth is caught (W5-ci-2) -------
mkrepo 0
grow_ignore "fix: quiet a scary-looking test string"
runGate
expect "gitleaksignore growth fails undeclared" 1 "UNDECLARED GROWTH: .gitleaksignore"

# --- 7. declared .gitleaksignore growth passes ----------------------
mkrepo 0
grow_ignore "$(printf 'fix: allowlist a public test fixture\n\nBaseline-Growth: .gitleaksignore — public account id in a unit test, not a credential')"
runGate
expect "gitleaksignore growth passes when declared" 0 "no undeclared"

# --- 8. .gitleaks.toml allowlist widening is caught -----------------
mkrepo 0
grow_toml "chore: broaden the secret-scan allowlist"
runGate
expect "gitleaks.toml allowlist growth fails undeclared" 1 "UNDECLARED GROWTH: .gitleaks.toml"

# --- 9. a non-widening .gitleaks.toml edit (comment) passes ---------
mkrepo 0
comment_toml "docs: clarify an allowlist rationale"
runGate
expect "gitleaks.toml comment-only edit passes" 0 "no undeclared"

# --- 10. CID-1 PER-FILE SCOPING -------------------------------------
# Grow TWO allowlists in one commit but declare only ONE of them. The
# named file is excused; the un-named one must still fail. The pre-fix
# gate cleared the fail flag for ALL files on the first trailer match, so
# this passed (both allowlists silently widened). Proven-red for CID-1.
mkrepo 0
grow_both "$(printf 'chore: widen two allowlists\n\nBaseline-Growth: .gitleaksignore — reviewed public fixture')"
runGate
expect "per-file: named .gitleaksignore is excused, .gitleaks.toml still fails" 1 "UNDECLARED GROWTH: .gitleaks.toml"
expect_missing "per-file: the declared .gitleaksignore is NOT reported" "UNDECLARED GROWTH: .gitleaksignore"

# --- 11. CID-1 DIRECT-PUSH SELF-HEAL --------------------------------
# Undeclared growth commit G lands (its own run reds); an innocuous commit
# is pushed on top; the healing push carries BASE_SHA=G (event.before=the
# failed tip). The pre-fix gate diffed G...HEAD, saw no growth in its own
# window, and went green — the growth healed with no trailer consulted.
# The base-walk must re-derive back past G and catch it. Proven-red.
mkrepo 0
grow "feat: sneak growth then push over it"
unrelated "chore: an innocuous follow-up push"
runGate "$GROW_SHA"
expect "self-heal: undeclared growth behind a moving base is still caught" 1 "UNDECLARED GROWTH: scripts/ci/demo.baseline"
expect "self-heal: base was re-derived (anti-heal note printed)" 1 "CID-1 anti-heal"

# --- 12. a properly-declared growth tip does NOT trigger the walk ----
# If the base tip DECLARED its own growth (names the file in its own
# message), it is a valid clean base — the walk must stop there and not
# re-litigate the already-approved growth.
mkrepo 0
grow "$(printf 'feat: declared at the tip\n\nBaseline-Growth: scripts/ci/demo.baseline — reviewed')"
unrelated "chore: follow-up"
runGate "$GROW_SHA"
expect "declared tip is a clean base — no re-flag on follow-up" 0 "no undeclared"

echo
echo "lint-baseline-growth-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
