#!/usr/bin/env bash
# lint-baseline-growth-test.sh — fixture tests for the anti-self-bypass
# tripwire.
#
# lint-baseline-growth.sh is the gate that stops a commit from growing a
# lint baseline in the same change that introduces the violation it hides.
# Its verdict has to be a function of the inputs and nothing else — so
# its behaviour is pinned here rather than assumed:
#
#   - undeclared growth fails;
#   - growth declared with a `Baseline-Growth: <file> — <reason>` trailer
#     that NAMES the grown file passes;
#   - a trailer with no reason after the colon does not count;
#   - a trailer that names a DIFFERENT watched file does NOT excuse growth
#     in an un-named one (per-file scoping);
#   - on direct-push, an undeclared growth cannot HEAL by being pushed
#     over: a BASE_SHA sitting on the failed growth tip is re-derived back
#     to the last clean/declared base and the growth is still caught
#     (anti-heal);
#   - no BASE_SHA (or a BASE_SHA outside this history) skips, rather
#     than failing a first push;
#   - and the verdict does not depend on how BIG the commit range is.
#
# The range-size case reproduces a CI failure mode: a trailer check of the
# form `git log --format=%B "$BASE..HEAD" | grep -qE ...` under
# `set -o pipefail`. `grep -q` exits at the first match and closes the
# pipe; once the log exceeded the 64KB pipe buffer, `git log` took SIGPIPE
# and reported 141, and pipefail surfaced that as the pipeline's status —
# so the `if` read "trailer found" as "no trailer" and failed a PR that had
# correctly declared its growth, passing locally and failing in CI on
# byte-identical inputs. The big-range case below reproduces it
# deterministically.
#
# Run: bash scripts/ci/lint-baseline-growth-test.sh
set -uo pipefail

# `git init` honours an INHERITED GIT_DIR ahead of its own `-C`, and a git
# hook exports GIT_DIR/GIT_INDEX_FILE. lint-changed dispatches test scripts,
# and the pre-commit hook runs lint-changed — so without this a fixture's
# init re-initialises the REAL repository: core.bare set on the live
# checkout, fixture commits on main, and git says only "warning: re-init".
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

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
    # mirroring how they exist on the real default branch. The
    # tripwire is a line-diff grep, not a TOML/fingerprint parser, so the
    # minimal shapes below are enough to exercise it.
    printf '# fingerprints\ncafe0000cafe0000cafe0000cafe0000cafe0000:x_test.go:generic-api-key:1\n' \
      > .gitleaksignore
    printf '[allowlist]\npaths = [\n  %s,\n]\n' "'''^docs/archive/'''" > .gitleaks.toml
    # Seed the five surfaces, mirroring their real
    # shape closely enough to exercise the growth detectors.
    printf '# accepted-risk vulns\nGO-2026-0001  # reviewed, localhost-only\n' \
      > scripts/ci/govulncheck-allow.txt
    mkdir -p deploy/systemd
    printf '# declared orphans\nexisting-orphan.service\n' > deploy/systemd/ORPHANS
    printf 'aaaa0000  0001_existing.up.sql\n' > scripts/ci/migration-immutability.sha256
    mkdir -p configs/ansible
    printf 'profile: moderate\nskip_list:\n  - yaml[truthy]\n' > configs/ansible/.ansible-lint
    printf 'linters:\n  exclusions:\n    generated: strict\n    rules:\n      - path: _test\\.go\n        linters:\n          - funlen\nissues:\n  max-same-issues: 0\n' \
      > .golangci.yml
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
# .gitleaksignore (the "silence a real leak in-diff" attack).
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
# ONE commit (the blanket-approval attack: two allowlists widened,
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

# grow_toml_lines <commit-message> <line>... — append the given lines to
# .gitleaks.toml (inline-array and new-block spellings of an allowlist).
grow_toml_lines() {
  (
    cd "$TMP/repo" || exit 1
    msg="$1"
    shift
    printf '%s\n' "$@" >> .gitleaks.toml
    git add -A
    printf '%s\n' "$msg" > "$TMP/gmsg"
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

# qualified_toml_then_drop <commit-message> <line> — commit a .gitleaks.toml carrying the
# qualifiers whose removal widens it (base recorded in $TMP/qbase), then a commit deleting <line>.
qualified_toml_then_drop() {
  (
    cd "$TMP/repo" || exit 1
    printf '%s\n' '[extend]' 'useDefault = true' '' '# why these fixtures are not secrets' \
      '[[allowlists]]' 'condition = "AND"' "paths = ['''_test\\.go\$''']" "regexes = ['''GEXAMPLE''']" \
      > .gitleaks.toml
    git add -A
    # Declared, so the gate's anti-heal walk keeps this commit as the base.
    git commit -qm "chore: qualified allowlist" -m "Baseline-Growth: .gitleaks.toml — fixture seed"
    git rev-parse HEAD > "$TMP/qbase"
    grep -vxF "$2" .gitleaks.toml > "$TMP/toml" || true
    cp "$TMP/toml" .gitleaks.toml
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# grow_govulncheck <commit-message> — add an accepted-risk vuln id
# (scripts/ci/govulncheck-allow.txt is a .txt, missed by the
# *.baseline glob).
grow_govulncheck() {
  (
    cd "$TMP/repo" || exit 1
    printf 'GO-2026-9999  # newly accepted, not actually reviewed\n' \
      >> scripts/ci/govulncheck-allow.txt
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# grow_orphans <commit-message> — declare a new orphan systemd unit
# (deploy/systemd/ORPHANS silences lint-deploy-systemd-authority.sh).
grow_orphans() {
  (
    cd "$TMP/repo" || exit 1
    printf 'newly-orphaned.service\n' >> deploy/systemd/ORPHANS
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# grow_ansible_lint <commit-message> — widen the ansible-lint skip_list
# (configs/ansible/.ansible-lint, auto-discovered rather than named
# in any CI yaml).
grow_ansible_lint() {
  (
    cd "$TMP/repo" || exit 1
    printf '  - name[casing]\n' >> configs/ansible/.ansible-lint
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# grow_golangci_exclusion <commit-message> — add a new path/linter
# exemption to .golangci.yml's exclusions.rules block.
grow_golangci_exclusion() {
  (
    cd "$TMP/repo" || exit 1
    # Insert a new rule INSIDE the exclusions.rules block (after its one
    # seeded entry) — appending at EOF would land past the block's end
    # (the "issues:" key) and prove nothing.
    sed -i.bak '/^          - funlen$/a\
      - path: cmd/.*/main\\.go\
        linters:\
          - errcheck
' .golangci.yml
    rm -f .golangci.yml.bak
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# golangci_unrelated <commit-message> — a .golangci.yml edit OUTSIDE the
# exclusions block: must NOT be read as growth.
golangci_unrelated() {
  (
    cd "$TMP/repo" || exit 1
    printf 'output:\n  formats:\n    text:\n      colors: true\n' >> .golangci.yml
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# migration_append <commit-message> — add a checksum for a brand-new
# migration file (the normal, append-only case): must NOT be flagged.
migration_append() {
  (
    cd "$TMP/repo" || exit 1
    printf 'bbbb1111  0002_new.up.sql\n' >> scripts/ci/migration-immutability.sha256
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
}

# migration_mutate <commit-message> — rewrite the hash recorded for an
# EXISTING basename (the shipped-migration-edit-hiding shape).
migration_mutate() {
  (
    cd "$TMP/repo" || exit 1
    printf 'cccc2222  0001_existing.up.sql\n' > scripts/ci/migration-immutability.sha256
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
    echo "$OUT" | sed -n '1,12s/^/    | /p'
    fail=$((fail + 1))
    return
  fi
  if [[ -n "$want_sub" && "$OUT" != *"$want_sub"* ]]; then
    echo "FAIL: $name — output missing: $want_sub"
    echo "$OUT" | sed -n '1,12s/^/    | /p'
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
    echo "$OUT" | sed -n '1,12s/^/    | /p'
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

# --- 6. .gitleaksignore fingerprint growth is caught ----------------
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

mkrepo 0
grow_toml_lines "chore: allowlist one value" "[[allowlists]]" "regexes = ['''SBEXAMPLEVALUE''']"
runGate
expect "gitleaks.toml inline regexes in a new block fails undeclared" 1 "UNDECLARED GROWTH: .gitleaks.toml"

mkrepo 0
grow_toml_lines "chore: exempt a commit" "commits = ['''0123456789abcdef0123456789abcdef01234567''']"
runGate
expect "gitleaks.toml inline commits allowlist fails undeclared" 1 "UNDECLARED GROWTH: .gitleaks.toml"

mkrepo 0
grow_toml_lines "chore: per-rule stopword" "[[rules]]" "id = \"generic-api-key\"" "[[rules.allowlists]]" "stopwords = [\"leak\"]"
runGate
expect "gitleaks.toml per-rule inline stopwords fails undeclared" 1 "UNDECLARED GROWTH: .gitleaks.toml"

# --- 9. a non-widening .gitleaks.toml edit (comment) passes ---------
mkrepo 0
comment_toml "docs: clarify an allowlist rationale"
runGate
expect "gitleaks.toml comment-only edit passes" 0 "no undeclared"

mkrepo 0
grow_toml_lines "docs: describe the allowlist" "description = \"why the docs archive is skipped\""
runGate
expect "gitleaks.toml description-only edit passes" 0 "no undeclared"

# --- 9b. removing a qualifier widens .gitleaks.toml ----------------
mkrepo 0
qualified_toml_then_drop "chore: simplify the allowlist" 'condition = "AND"'
runGate "$(cat "$TMP/qbase")"
expect "gitleaks.toml removed condition = AND fails undeclared" 1 "UNDECLARED GROWTH: .gitleaks.toml"

mkrepo 0
qualified_toml_then_drop "chore: trim the extend block" 'useDefault = true'
runGate "$(cat "$TMP/qbase")"
expect "gitleaks.toml removed useDefault fails undeclared" 1 "UNDECLARED GROWTH: .gitleaks.toml"

mkrepo 0
qualified_toml_then_drop "docs: drop a stale comment" '# why these fixtures are not secrets'
runGate "$(cat "$TMP/qbase")"
expect "gitleaks.toml removed comment passes" 0 "no undeclared"

# --- 10. PER-FILE SCOPING --------------------------------------------
# Grow TWO allowlists in one commit but declare only ONE of them. The
# named file is excused; the un-named one must still fail. The pre-fix
# gate cleared the fail flag for ALL files on the first trailer match, so
# this passed (both allowlists silently widened). Proven red.
mkrepo 0
grow_both "$(printf 'chore: widen two allowlists\n\nBaseline-Growth: .gitleaksignore — reviewed public fixture')"
runGate
expect "per-file: named .gitleaksignore is excused, .gitleaks.toml still fails" 1 "UNDECLARED GROWTH: .gitleaks.toml"
expect_missing "per-file: the declared .gitleaksignore is NOT reported" "UNDECLARED GROWTH: .gitleaksignore"

# --- 11. DIRECT-PUSH SELF-HEAL ---------------------------------------
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

# --- 13. govulncheck-allow.txt growth is caught -----------
mkrepo 0
grow_govulncheck "chore: accept a new vuln"
runGate
expect "govulncheck-allow.txt growth fails undeclared" 1 "UNDECLARED GROWTH: scripts/ci/govulncheck-allow.txt"

# --- 14. deploy/systemd/ORPHANS growth is caught ----------------
mkrepo 0
grow_orphans "chore: declare an orphan unit"
runGate
expect "ORPHANS growth fails undeclared" 1 "UNDECLARED GROWTH: deploy/systemd/ORPHANS"

# --- 14b. a waiver added to a Go lint's in-source map is caught ---
mkrepo 0
(
  cd "$TMP/repo" || exit 1
  mkdir -p scripts/ci/lint-pk-discriminators
  printf 'package main\n\nvar allow = map[string]string{\n\t"trades": "OK: fanned op_index",\n}\n' \
    > scripts/ci/lint-pk-discriminators/main.go
  git add -A && git commit -qm "chore: add the lint"
  git rev-parse HEAD > "$TMP/base"
  printf 'package main\n\nvar allow = map[string]string{\n\t"trades": "OK: fanned op_index",\n\t"new_events": "OK: trust me",\n}\n' \
    > scripts/ci/lint-pk-discriminators/main.go
  git add -A && git commit -qm "chore: waive a table"
)
runGate
expect "a pk-discriminators waiver fails undeclared" 1 "UNDECLARED GROWTH: scripts/ci/lint-pk-discriminators/main.go"

# --- 14c. an entry added to an inline baseline in lint-migrations.sh is caught ---
mkrepo 0
(
  cd "$TMP/repo" || exit 1
  mkdir -p scripts/ci
  printf "followup_baseline='0002_a.up.sql\n0003_b.up.sql'\necho ok\n" > scripts/ci/lint-migrations.sh
  git add -A && git commit -qm "chore: add the lint"
  git rev-parse HEAD > "$TMP/base"
  printf "followup_baseline='0002_a.up.sql\n0003_b.up.sql\n0004_c.up.sql:c_idx'\necho ok, still\n" > scripts/ci/lint-migrations.sh
  git add -A && git commit -qm "chore: grandfather a migration"
)
runGate
expect "a lint-migrations.sh inline baseline entry fails undeclared" 1 "UNDECLARED GROWTH: scripts/ci/lint-migrations.sh"

# --- 15. configs/ansible/.ansible-lint skip_list growth is caught
mkrepo 0
grow_ansible_lint "chore: grandfather another ansible-lint rule"
runGate
expect "ansible-lint skip_list growth fails undeclared" 1 "UNDECLARED GROWTH: configs/ansible/.ansible-lint"

# --- 16. .golangci.yml exclusions.rules growth is caught --------
mkrepo 0
grow_golangci_exclusion "chore: exempt another path from lint"
runGate
expect ".golangci.yml exclusions growth fails undeclared" 1 "UNDECLARED GROWTH: .golangci.yml"

# --- 17. an unrelated .golangci.yml edit (outside exclusions) passes --
mkrepo 0
golangci_unrelated "chore: tweak output formatting"
runGate
expect ".golangci.yml edit outside exclusions passes" 0 "no undeclared"

# --- 18. migration-immutability.sha256 — a normal append (new
# migration checksum) must NOT be flagged; append-only is the documented,
# always-allowed case.
mkrepo 0
migration_append "feat: add migration 0002"
runGate
expect "new migration checksum append passes" 0 "no undeclared"

# --- 19. migration-immutability.sha256 — a MUTATED hash on an
# EXISTING basename (the shipped-migration-edit-hiding shape) is caught.
mkrepo 0
migration_mutate "fix: quietly rehash an existing migration"
runGate
expect "mutated migration checksum fails undeclared" 1 "UNDECLARED GROWTH: scripts/ci/migration-immutability.sha256"

# --- 20. declaring the new surfaces with a scoped trailer passes ------
mkrepo 0
grow_govulncheck "$(printf 'chore: accept a new vuln\n\nBaseline-Growth: scripts/ci/govulncheck-allow.txt — reviewed, localhost-only')"
runGate
expect "declared govulncheck-allow.txt growth passes" 0 "no undeclared"

# --- 21. an inline escape marker added to Go or SQL needs a trailer naming that file
inline_marker() {
  (
    cd "$TMP/repo" || exit 1
    mkdir -p internal/x migrations
    printf 'package x\n\nvar f = 1.0\n' > internal/x/a.go
    printf 'ALTER TABLE t ADD COLUMN c int;\n' > migrations/0002_a.up.sql
    git add -A && git commit -qm "chore: add files"
    git rev-parse HEAD > "$TMP/base"
    printf 'package x\n\nvar f = 1.0 //i128:ok display only\n' > internal/x/a.go
    printf 'ALTER TABLE t DROP COLUMN c; -- migration-compat:ok two-release dance\n' > migrations/0002_a.up.sql
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
  BASE="$(cat "$TMP/base")"
}
mkrepo 0
inline_marker "chore: waive inline"
runGate
expect "an added //i128:ok fails undeclared" 1 "UNDECLARED GROWTH: internal/x/a.go"
expect "an added migration-compat:ok fails undeclared" 1 "UNDECLARED GROWTH: migrations/0002_a.up.sql"

mkrepo 0
inline_marker "$(printf 'chore: waive inline\n\nBaseline-Growth: internal/x/a.go — display only\nBaseline-Growth: migrations/0002_a.up.sql — reviewed')"
runGate
expect "declared inline markers pass" 0 "no undeclared"

# --- 21b. a secret-scan allow marker in ANY file silences gitleaks for that line
leak_marker() {
  (
    cd "$TMP/repo" || exit 1
    mkdir -p configs
    printf 'key = "placeholder"\n' > configs/x.toml
    git add -A && git commit -qm "chore: add config"
    git rev-parse HEAD > "$TMP/base"
    printf 'key = "placeholder" # gitleaks:%s\n' allow > configs/x.toml
    git add -A
    printf '%s\n' "$1" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
  BASE="$(cat "$TMP/base")"
}
mkrepo 0
leak_marker "chore: waive secret scan"
runGate
expect "an added gitleaks allow marker in a .toml fails undeclared" 1 "UNDECLARED GROWTH: configs/x.toml"

mkrepo 0
leak_marker "$(printf 'chore: waive secret scan\n\nBaseline-Growth: configs/x.toml — fixture value, not a credential')"
runGate
expect "a declared gitleaks allow marker passes" 0 "no undeclared"

# --- 22. the other escape markers the lints accept, in any letter case
inline_marker_more() {
  (
    cd "$TMP/repo" || exit 1
    mkdir -p internal/x migrations
    printf 'package x\n\nvar f = 1\n' > internal/x/a.go
    printf 'ALTER TABLE t ADD COLUMN c int;\n' > migrations/0002_a.up.sql
    git add -A && git commit -qm "chore: add files"
    git rev-parse HEAD > "$TMP/base"
    printf 'package x\n\nvar f float64 //floatmoney:ok display only\n' > internal/x/a.go
    printf 'ALTER TABLE t ADD COLUMN fee_usd DOUBLE PRECISION; -- LINT-MONEY:OK display only\n' > migrations/0002_a.up.sql
    printf 'CREATE VIEW v AS SELECT 1; -- lint-priceable:ok legacy\n' > migrations/0003_b.up.sql
    git add -A
    printf 'chore: waive inline\n' > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
  BASE="$(cat "$TMP/base")"
}
mkrepo 0
inline_marker_more
runGate
expect "an added //floatmoney:ok fails undeclared" 1 "UNDECLARED GROWTH: internal/x/a.go"
expect "an added upper-case LINT-MONEY:OK fails undeclared" 1 "UNDECLARED GROWTH: migrations/0002_a.up.sql"
expect "an added lint-priceable:ok fails undeclared" 1 "UNDECLARED GROWTH: migrations/0003_b.up.sql"

# change_commit <message> <path> <before> <after> [<path> <before> <after> ...] — commit each
# <before> (empty = absent), record BASE, then commit each <after> with <message>.
change_commit() {
  local msg="$1"
  shift
  (
    cd "$TMP/repo" || exit 1
    local args=("$@") i seed="chore: seed"$'\n'
    for ((i = 0; i < ${#args[@]}; i += 3)); do
      mkdir -p "$(dirname "${args[i]}")"
      [[ -n "${args[i + 1]}" ]] && printf '%s\n' "${args[i + 1]}" > "${args[i]}"
      seed="$seed"$'\n'"Baseline-Growth: ${args[i]} — fixture seed"
    done
    git add -A && git commit -qm "$seed" --allow-empty
    git rev-parse HEAD > "$TMP/base"
    for ((i = 0; i < ${#args[@]}; i += 3)); do
      printf '%s\n' "${args[i + 2]}" > "${args[i]}"
    done
    git add -A
    printf '%s\n' "$msg" > "$TMP/gmsg"
    git commit -q -F "$TMP/gmsg"
  )
  BASE="$(cat "$TMP/base")"
}

# --- 23. every escape marker a lint honours, in whatever file type that lint scans
mkrepo 0
change_commit "chore: waive inline" \
  internal/m/sum.go 'var q = 1' 'var q = 1 // i128sum:ok bounded' \
  internal/m/dec.go 'var d = 1' 'var d = 7 // decimals-7: classic' \
  internal/m/cap.go 'var c = 1' 'var c = 1 // cap-ok: fine' \
  internal/m/ident.go 'var i = 1' 'var i = 1 // asset-identity: single-form ok (x)' \
  internal/m/latest.go 'var l = 1' 'var l = 1 // unbounded-latest-ok: x' \
  internal/m/apikey.go 'var a = 1' 'var a = 1 // apikey-scan-ok: x' \
  internal/m/http.go 'var h = 1' 'var h = 1 // http-timeout-ok: x' \
  configs/ansible/t.yml '- name: x' '- name: x  # si-swallow-ok: probe'
runGate
for f in sum dec cap ident latest apikey http; do
  expect "an added marker in internal/m/$f.go fails undeclared" 1 "UNDECLARED GROWTH: internal/m/$f.go"
done
expect "an added si-swallow-ok: in a playbook fails undeclared" 1 "UNDECLARED GROWTH: configs/ansible/t.yml"

mkrepo 0
# shellcheck disable=SC2016  # the backticks are markdown, not a substitution
change_commit "chore: teach a lint a marker" \
  scripts/ci/lint-foo.sh '# lint' '# lint; honours // cap-ok: <reason>' \
  scripts/ci/lint-foo-test.sh '# t' 'printf "x // cap-ok: y"' \
  docs/x.md 'doc' 'Mark the line `i128sum:ok <reason>`.'
runGate
expect "a marker in a lint, its self-test or a doc is not growth" 0 "no undeclared"

# --- 24. a trailer excuses only the file in its path slot, never one named in a reason
mkrepo 0
change_commit "$(printf 'chore: two\n\nBaseline-Growth: scripts/ci/demo.baseline — until internal/x/a.go moves')" \
  scripts/ci/demo.baseline 'existing-entry' "$(printf 'existing-entry\nnew-entry')" \
  internal/x/a.go 'var v = 1' 'var v = 1 //i128:ok display only'
runGate
expect "a file named only in another trailer's reason is undeclared" 1 "UNDECLARED GROWTH: internal/x/a.go"

mkrepo 0
change_commit "$(printf 'chore: one\n\nBaseline-Growth: internal/x/a.go.orig — not this file')" \
  internal/x/a.go 'var v = 1' 'var v = 1 //i128:ok display only'
runGate
expect "a trailer path that merely contains the file is undeclared" 1 "UNDECLARED GROWTH: internal/x/a.go"

# --- 25. count-format baselines: a lowered count is a shrink, a raised or new key is growth
count_seed="$(printf 'a.yml|unannotated-swallow|5\nb.yml|unannotated-swallow|6\nc.go:#interp  3 reads: why')"
mkrepo 0
change_commit "chore: fix swallows" \
  scripts/ci/demo-count.baseline "$count_seed" \
  "$(printf 'a.yml|unannotated-swallow|4\nb.yml|unannotated-swallow|6\nc.go:#interp  2 reads: why, now fewer')"
runGate
expect "lowering counts needs no trailer" 0 "no undeclared"

mkrepo 0
change_commit "chore: raise one" \
  scripts/ci/demo-count.baseline "$count_seed" \
  "$(printf 'a.yml|unannotated-swallow|4\nb.yml|unannotated-swallow|7\nc.go:#interp  3 reads: why')"
runGate
expect "raising a count is undeclared growth" 1 "+ b.yml|unannotated-swallow|7"

mkrepo 0
change_commit "chore: new key" \
  scripts/ci/demo-count.baseline "$count_seed" \
  "$(printf '%s\nd.yml|unannotated-swallow|1' "$count_seed")"
runGate
expect "a new counted key is undeclared growth" 1 "+ d.yml|unannotated-swallow|1"

# --- 25b. a metric waiver entry carrying its reason comment is still growth
mkrepo 0
change_commit "chore: waive a metric" \
  scripts/ci/lint-metric-refs.sh 'X=(' "$(printf 'X=(\n  stellarindex_x_dropped_total  # dashboard only\n)')"
runGate
expect "a commented metric waiver fails undeclared" 1 "UNDECLARED GROWTH: scripts/ci/lint-metric-refs.sh"

# --- 25c. a fingerprinted entry: a new fingerprint at the same or a lower count swaps the excused reads
fp_seed='c.go:#interp  6 reads: #111 spliced from a closed set'
mkrepo 0
change_commit "chore: swap a read" scripts/ci/demo-count.baseline "$fp_seed" \
  'c.go:#interp  6 reads: #222 spliced from a closed set'
runGate
expect "a new fingerprint at the same count is undeclared growth" 1 "+ c.go:#interp  6 reads: #222"

mkrepo 0
change_commit "chore: swap a read" scripts/ci/demo-count.baseline "$fp_seed" \
  'c.go:#interp  5 reads: #333 spliced from a closed set'
runGate
expect "a new fingerprint at a lower count is undeclared growth" 1 "+ c.go:#interp  5 reads: #333"

mkrepo 0
change_commit "chore: fewer" scripts/ci/demo-count.baseline "$fp_seed" \
  'c.go:#interp  5 reads: #111 spliced from a closed set'
runGate
expect "a lower count keeping its fingerprint needs no trailer" 0 "no undeclared"

# --- 25d. a Go guard test's waiver map: a new key is growth, a reworded reason is not
map_go() { printf 'package p\n\nvar closedVWAPSeamExempt = map[string]string{\n%s}\n' "$1"; }
mkrepo 0
change_commit "chore: exempt a reader" internal/p/gate_test.go \
  "$(map_go $'\t"a:b.C": "probe",\n')" "$(map_go $'\t"a:b.C": "probe",\n\t"x:y.Z": "sneak",\n')"
runGate
expect "a new Go waiver-map key fails undeclared" 1 "UNDECLARED GROWTH: internal/p/gate_test.go"

mkrepo 0
change_commit "chore: reword" internal/p/gate_test.go \
  "$(map_go $'\t"a:b.C": "probe",\n')" "$(map_go $'\t"a:b.C": "existence probe, returns a bool",\n')"
runGate
expect "a reworded Go waiver reason needs no trailer" 0 "no undeclared"

# --- 25e. golangci: a linter dropped from enable, or a new settings exclude, is growth
golangci_seed="$(printf 'linters:\n  enable:\n    - gosec\n    - exhaustive\n  settings:\n    gosec:\n      excludes:\n        - G104\n')"
mkrepo 0
change_commit "chore: quieter" .golangci.yml "$golangci_seed" \
  "$(printf 'linters:\n  enable:\n    - exhaustive\n  settings:\n    gosec:\n      excludes:\n        - G104\n')"
runGate
expect "a linter removed from linters.enable fails undeclared" 1 "removed from linters.enable: gosec"

mkrepo 0
change_commit "chore: quieter" .golangci.yml "$golangci_seed" "$(printf '%s\n        - G115\n' "$golangci_seed")"
runGate
expect "a new gosec exclude fails undeclared" 1 "+ linters: - G115"

mkrepo 0
change_commit "chore: stricter" .golangci.yml "$golangci_seed" \
  "$(printf 'linters:\n  enable:\n    - gosec\n    - exhaustive\n    - nilerr\n  settings:\n    gosec:\n      excludes:\n        - G104\n')"
runGate
expect "a linter added to linters.enable needs no trailer" 0 "no undeclared"

# --- 25f. third-party and prose waivers: exhaustive:ignore, DO NOT RUN
mkrepo 0
change_commit "chore: waive" \
  internal/m/ex.go 'var e = 1' "$(printf '//exhaustive:ignore\nvar e = 1')" \
  ops/r.sh 'echo' "psql -c \"CALL refresh_continuous_aggregate('t', NULL, now())\" # DO NOT RUN twice"
runGate
for f in internal/m/ex.go ops/r.sh; do
  expect "an added waiver in $f fails undeclared" 1 "UNDECLARED GROWTH: $f"
done

mkrepo 0
change_commit "chore: prose" internal/m/p.go 'var p = 1' "$(printf '// changes here do not run the shards\nvar p = 2')"
runGate
expect "lowercase do-not-run prose is not a marker" 0 "no undeclared"

# --- 25g. a marker on the line above governs the line below: rewriting that line is growth
mkrepo 0
change_commit "chore: refactor" internal/api/l.go \
  "$(printf 'type R struct {\n\t// floatmoney:ok rate, not an amount\n\tSupplyAPR *float64\n}')" \
  "$(printf 'type R struct {\n\t// floatmoney:ok rate, not an amount\n\tSuppliedUSD *float64\n}')"
runGate
expect "a rewritten line under a standalone marker fails undeclared" 1 "+ SuppliedUSD *float64"

# --- 25h. zizmor: an inline ignore in a workflow, a config waiver, or a raised floor is growth
mkrepo 0
# shellcheck disable=SC2016  # the ${{ }} is workflow syntax, not a shell expansion
change_commit "chore: quiet zizmor" \
  .github/workflows/x.yml 'run: echo' 'run: echo "${{ github.event.issue.title }}" # zizmor: ignore[template-injection]' \
  .github/workflows/y.yml 'with: {}' "$(printf 'with:\n  min-confidence: high')" \
  .github/zizmor.yml 'rules: {}' "$(printf 'rules:\n  template-injection:\n    disable: true')"
runGate
for f in .github/workflows/x.yml .github/workflows/y.yml .github/zizmor.yml; do
  expect "a zizmor waiver in $f fails undeclared" 1 "UNDECLARED GROWTH: $f"
done

# --- 25i. a .gitattributes `-diff` cannot blind the tripwire, and is itself growth
mkrepo 0
change_commit "chore: tidy diffs" \
  scripts/ci/.gitattributes '# attrs' "$(printf '# attrs\n*.baseline -diff')" \
  scripts/ci/demo.baseline 'existing-entry' "$(printf 'existing-entry\nhidden-entry')"
runGate
expect "a baseline entry behind a -diff attribute fails undeclared" 1 "+ hidden-entry"
expect "an added -diff attribute fails undeclared" 1 "UNDECLARED GROWTH: scripts/ci/.gitattributes"

mkrepo 0
change_commit "chore: tidy diffs" \
  internal/a/.gitattributes '# attrs' "$(printf '# attrs\n*.go diff=foo')" \
  internal/b/.gitattributes '# attrs' "$(printf '# attrs\n*.sql merge=union')" \
  internal/c/.gitattributes '# attrs' "$(printf '# attrs\n*.pb.go linguist-generated')"
runGate
for f in a b c; do
  expect "a diff=/merge=/linguist-generated attribute in internal/$f fails undeclared" 1 "UNDECLARED GROWTH: internal/$f/.gitattributes"
done

# --- 25j. Go waiver maps: a := local, a single-line map, an identifier key, a raised count
mkrepo 0
change_commit "chore: exempt more" \
  internal/p/a_test.go "$(printf 'package p\n\nfunc f() {\n\topenBucketExempt := map[string]string{\n\t\t"a": "r",\n\t}\n\t_ = openBucketExempt\n}')" \
  "$(printf 'package p\n\nfunc f() {\n\topenBucketExempt := map[string]string{\n\t\t"a": "r",\n\t\t"sneak": "r",\n\t}\n\t_ = openBucketExempt\n}')" \
  internal/p/b_test.go 'var streamAllowed = map[string]bool{"a.go": true}' \
  'var streamAllowed = map[string]bool{"a.go": true, "trades_writer.go": true}' \
  internal/p/c_test.go "$(printf 'var reviewedAllowlist = map[string]int{\n\t"supply_reader.go": 1,\n}')" \
  "$(printf 'var reviewedAllowlist = map[string]int{\n\t"supply_reader.go": 2,\n}')" \
  internal/p/d_test.go "$(printf 'var kindExempt = map[Kind]string{\n\tKindA: "r",\n}')" \
  "$(printf 'var kindExempt = map[Kind]string{\n\tKindA: "r",\n\tKindB: "r",\n}')" \
  internal/p/e_test.go 'var fooExempt = map[string]interface{}{"a": nil}' \
  'var fooExempt = map[string]interface{}{"a": nil, "b": nil}' \
  internal/p/f_test.go 'var barAllowlist = map[string]struct{ why string }{"c": {why: "x"}}' \
  'var barAllowlist = map[string]struct{ why string }{"c": {why: "x"}, "d": {why: "y"}}' \
  internal/p/g_test.go "$(printf 'var bazWaiver = map[string]struct{ m struct{ x int } }{\n\t"e": {m: struct{ x int }{1}},\n}')" \
  "$(printf 'var bazWaiver = map[string]struct{ m struct{ x int } }{\n\t"e": {m: struct{ x int }{1}},\n\t"f": {m: struct{ x int }{2}},\n}')" \
  internal/p/h_test.go "$(printf 'var quoteExempt = map[string]bool{\n\t"a\\"b": true,\n\t"c\\td": true,\n}')" \
  "$(printf 'var quoteExempt = map[string]bool{\n\t"a\\"b": true,\n\t"c\\td": true,\n\t"e": true,\n}')"
runGate
expect "a widened waiver map is reported by key" 1 '+ "f"'
for f in a b c d e f g h; do
  expect "a widened Go waiver map in internal/p/${f}_test.go fails undeclared" 1 "UNDECLARED GROWTH: internal/p/${f}_test.go"
done

# --- 25k. waiver entries in their other spellings: quoted, several per line, indented ids
mkrepo 0
change_commit "chore: waive" \
  scripts/ci/lint-metric-refs.sh 'X=(' "$(printf 'X=(\n  "stellarindex_a_failures_total"  # later\n  stellarindex_b_total stellarindex_c_total\n)')" \
  scripts/ci/govulncheck-allow.txt '# ids' "$(printf '# ids\n  GO-2026-9999  # indented')"
runGate
expect "a quoted metric waiver fails undeclared" 1 '+   "stellarindex_a_failures_total"'
expect "two metric waivers on one line fail undeclared" 1 "+   stellarindex_b_total stellarindex_c_total"
expect "an indented govulncheck id fails undeclared" 1 "UNDECLARED GROWTH: scripts/ci/govulncheck-allow.txt"

# --- 25l. a DO NOT RUN added to a runbook is growth (lint-migration-commands honours it in docs)
mkrepo 0
change_commit "chore: runbook" docs/x.md 'doc' "$(printf 'doc\n-- DO NOT RUN without reading the header\nDELETE FROM trades;')"
runGate
expect "a DO NOT RUN in a doc fails undeclared" 1 "UNDECLARED GROWTH: docs/x.md"

# --- 25m. a tool config under another name, or the watched config deleted, is growth
mkrepo 0
change_commit "chore: configs" \
  .golangci.yaml '' "$(printf 'version: "2"\nlinters:\n  default: none')" \
  zizmor.yml '' 'rules: {}'
(cd "$TMP/repo" && git rm -q .golangci.yml && git commit -qm "chore: drop config")
runGate
for f in .golangci.yaml zizmor.yml .golangci.yml; do
  expect "an alternate or deleted tool config $f fails undeclared" 1 "UNDECLARED GROWTH: $f"
done

# --- 25n. a marker trailing a code line governs the next line (cap-ok:, si-swallow-ok:)
mkrepo 0
change_commit "chore: another read" internal/s/q.go \
  "$(printf 'var a = 1 // cap-ok: paged\nvar b = 2')" "$(printf 'var a = 1 // cap-ok: paged\nvar n = 3')"
runGate
expect "a line rewritten under a trailing marker fails undeclared" 1 "+ var n = 3"

mkrepo 0
change_commit "chore: tidy" internal/s/q.go \
  "$(printf 'func g() int {\n\ta := 1 // cap-ok: paged\n\treturn a\n}')" \
  "$(printf 'func g() int {\n\ta := 1 // cap-ok: paged\n\n\treturn a\n\t// done\n}')"
runGate
expect "lines added near an unchanged trailing marker are not growth" 0 "no undeclared"

mkrepo 0
change_commit "chore: refactor" internal/api/l.go \
  "$(printf 'type R struct {\n\t// floatmoney:ok rate, not an amount\n\tSupplyAPR *float64\n}')" \
  "$(printf 'type R struct {\n\t// floatmoney:ok rate, not an amount\n\tSuppliedUSD *float64\n\tSupplyAPR *float64\n}')"
runGate
expect "a line inserted under an unchanged standalone marker fails undeclared" 1 "+ SuppliedUSD *float64"

# --- 26. every marker a lint honours is registered as watched or unwatched
reg() { sed -n "s/^$1='\\(.*\\)'\$/\\1/p" "$GATE" | tr '|' '\n'; }
registered="$(reg WATCHED_MARKERS; reg UNWATCHED_MARKERS)"
unregistered=""
while IFS= read -r m; do
  [[ -n "$m" ]] || continue
  grep -Fxq -- "$m" <<<"$registered" || unregistered="$unregistered $m"
done <<<"$(grep -hoE '[a-z0-9][a-z0-9-]*(:ok|-ok:)' scripts/ci/lint-*.sh scripts/ci/lint-*.py | sort -u)"
if [[ -n "$registered" && -z "$unregistered" ]]; then
  echo "ok: every lint escape marker is registered"
  pass=$((pass + 1))
else
  echo "FAIL: escape markers missing from WATCHED_MARKERS/UNWATCHED_MARKERS:${unregistered:- (registry empty)}"
  fail=$((fail + 1))
fi

echo
echo "lint-baseline-growth-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
