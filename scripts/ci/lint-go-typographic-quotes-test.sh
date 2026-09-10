#!/usr/bin/env bash
# lint-go-typographic-quotes-test.sh — fixtures for the typographic-quote
# gate (scripts/ci/lint-go-typographic-quotes.py).
#
# The gate exists because gofumpt's doc-comment reformatter rewrites a
# doubled apostrophe to U+201D, which silently changed what several
# ClickHouse/Timescale comments said the filter was. A gate for that has to
# be demonstrated failing on the real corrupted shape, not just passing on a
# clean tree — a gate never seen to go red is not known to work.
#
# What is pinned here:
#
#   1. THE KNOWN-BAD FILE. The byte-for-byte incident shape (a doc comment
#      carrying U+201D beside a raw string that still has the literal '')
#      is caught, at the right line, and the raw-string line is NOT flagged.
#   2. THE FMT-STABLE PROPERTY, run for real when gofumpt is installed:
#      `gofumpt -l` over that same file lists NOTHING while the gate fails.
#      This is the whole reason the gate is not "just run the formatter".
#   3. THE CLEAN FILE PASSES: '' in a raw string, and '' inside an indented
#      doc-comment code block — the remedy the failure text recommends —
#      are both fine.
#   4. ALL FOUR characters are found, one finding each. This is the case a
#      BRE `\|` alternation scan got wrong: POSIX basic regular expressions
#      have no alternation, so a grep without the GNU extension matches a
#      literal `|` and exits 1 — a clean bill of health that means nothing.
#      The gate does not shell out to grep at all, and case 9 pins that.
#   5. WAIVERS: `lint-quotes:ok: <reason>` on the line, or the line above,
#      waives and is COUNTED; the bare marker with no reason does not.
#   6. VACUITY: a tree with no Go file in scope exits 2 rather than
#      reporting a pass over nothing.
#   7. SCOPE: vendored and generated files are skipped and counted;
#      untracked files are not scanned at all (discovery is `git ls-files`).
#   8. A Go file that is not valid UTF-8 is a finding, not a crash.
#
# Run: bash scripts/ci/lint-go-typographic-quotes-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/scripts/ci/lint-go-typographic-quotes.py"
[ -f "$SRC" ] || { echo "lint-go-typographic-quotes-test: gate not found at $SRC" >&2; exit 2; }

# pwd -P: mktemp -d hands back /var/folders/... on macOS while git reports
# /private/var/folders/..., and the fixture-landed-here assertion below
# compares those two strings.
TMP="$(mktemp -d)" || exit 2
TMP="$(cd "$TMP" && pwd -P)" || exit 2
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }
expect_rc()   { if [ "$rc" -eq "$1" ]; then ok "$2 (rc=$rc)"; else bad "$2 (expected rc $1, got $rc)"; echo "$out" | sed -n '1,12s/^/      | /p'; fi; }
expect_hit()  { if grep -q -- "$1" <<<"$out"; then ok "$2"; else bad "$2 (missing: $1)"; echo "$out" | sed -n '1,12s/^/      | /p'; fi; }
expect_miss() { if grep -q -- "$1" <<<"$out"; then bad "$2 (unexpected: $1)"; echo "$out" | sed -n '1,12s/^/      | /p'; else ok "$2"; fi; }

# new_repo <abs-dir> — a throwaway tracked tree. The gate discovers its
# subject with `git ls-files`, so the fixture has to be a real repository.
#
# A git hook runs with GIT_DIR / GIT_INDEX_FILE and friends EXPORTED, and
# `git init` honours an inherited GIT_DIR AHEAD of its own -C — so a fixture
# built from the pre-commit path (which is where scripts/dev/lint-changed.sh
# runs this file) would re-initialise the REAL repository instead. Clear them
# first, then ASSERT the fixture landed where it was asked to: missing one
# variable fails the same silent way, and git says only "warning: re-init".
# Enforced by scripts/ci/lint-git-fixture-isolation.sh.
new_repo() {
  local d="$1"
  rm -rf "$d"
  mkdir -p "$d"
  unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
        GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR
  git -C "$d" init -q -b main || return 1
  case "$(git -C "$d" rev-parse --absolute-git-dir)" in
    "$d"/*) ;;
    *) echo "fixture escaped its directory — refusing to continue" >&2; exit 1 ;;
  esac
}

# run_gate <root> [args...] — sets $out and $rc.
run_gate() {
  local root="$1"
  shift
  out="$(GO_QUOTES_LINT_ROOT="$root" python3 "$SRC" "$@" 2>&1)"
  rc=$?
}

# The typographic characters are written as UTF-8 byte escapes so this file
# stays plain ASCII and the codepoint under test is stated, not guessed:
#   \xe2\x80\x98 U+2018   \xe2\x80\x99 U+2019
#   \xe2\x80\x9c U+201C   \xe2\x80\x9d U+201D
# \x27 is a plain apostrophe, so \x27\x27 is the doubled form gofumpt eats.

# ── 1. the known-bad file ──────────────────────────────────────────────────
echo "lint-go-typographic-quotes-test: the corrupted doc comment"

R="$TMP/bad"; new_repo "$R" || exit 2
mkdir -p "$R/internal/storage/clickhouse"
BADGO="$R/internal/storage/clickhouse/lake.go"
# shellcheck disable=SC2016  # the backticks are Go raw-string/comment syntax in the fixture, not command substitution
printf 'package clickhouse\n\n// Doc says the filter is `entry_xdr != \xe2\x80\x9d` applied to the row FINAL kept.\nconst Q = `SELECT 1 WHERE entry_xdr != \x27\x27 AND a = \x27\x27`\n' > "$BADGO"
git -C "$R" add -A

run_gate "$R"
expect_rc 1 "the known-bad file FAILS the gate"
expect_hit "internal/storage/clickhouse/lake.go:3:" "the finding names the file and the comment line"
expect_hit "U+201D" "the finding names the codepoint it found"
expect_hit "in a comment" "the finding says the character is in a comment"
expect_miss "lake.go:4:" "the raw string on line 4 is NOT flagged"
expect_hit "1 typographic quote(s) across 1 Go file(s)" "the summary counts findings AND files checked"

# The failure text has to teach the non-obvious part: the cause is the
# formatter, and restoring '' is not the fix because it gets re-mangled.
expect_hit "gofumpt" "the failure names gofumpt as the cause"
expect_hit "DOC-COMMENT reformatter" "the failure names the doc-comment reformatter specifically"
expect_hit "DO NOT JUST RETYPE" "the failure warns against restoring the doubled apostrophe"
expect_hit "FIXED POINT" "the failure explains why re-running the formatter proves nothing"
expect_hit "indented code block" "the failure gives the reword/code-block remedy"

# ── 2. fmt-stable: the formatter sees nothing wrong with that same file ───
echo "lint-go-typographic-quotes-test: the corruption is fmt-STABLE"

gofumpt_bin=""
if command -v gofumpt >/dev/null 2>&1; then
  gofumpt_bin="$(command -v gofumpt)"
elif [ -x "$(go env GOPATH 2>/dev/null)/bin/gofumpt" ]; then
  gofumpt_bin="$(go env GOPATH)/bin/gofumpt"
fi
if [ -n "$gofumpt_bin" ]; then
  fmt_out="$("$gofumpt_bin" -l "$BADGO" 2>&1)"
  if [ -z "$fmt_out" ]; then
    ok "gofumpt -l lists NOTHING for the corrupted file — 'the tree was clean after make fmt' cannot detect this"
  else
    bad "gofumpt -l unexpectedly reported the corrupted file: $fmt_out"
  fi
else
  echo "  skip — gofumpt not installed; the fmt-stable property is not exercised locally"
fi

# ── 3. the clean file passes ───────────────────────────────────────────────
echo "lint-go-typographic-quotes-test: the repaired shapes pass"

R="$TMP/clean"; new_repo "$R" || exit 2
mkdir -p "$R/internal/storage/clickhouse"
{
  printf 'package clickhouse\n\n'
  printf '// The row FINAL keeps is the one matching:\n//\n'
  printf '//\tentry_xdr != \x27\x27\n//\n'
  printf '// so the predicate is applied before dedup.\n'
  # shellcheck disable=SC2016  # Go raw-string backticks in the fixture, not command substitution
  printf 'const Q = `SELECT 1 WHERE entry_xdr != \x27\x27`\n'
} > "$R/internal/storage/clickhouse/lake.go"
git -C "$R" add -A
run_gate "$R"
expect_rc 0 "a doc-comment code block and a raw string, both holding '', PASS"
expect_hit "OK — 1 Go file(s)" "the pass line says how many files were checked"

# ── 4. all four characters, one finding each ───────────────────────────────
# The alternation case. A BRE `\|` scan reported the tree clean here.
echo "lint-go-typographic-quotes-test: all four codepoints"

R="$TMP/four"; new_repo "$R" || exit 2
printf 'package p\n\n// a \xe2\x80\x98 one\n// b \xe2\x80\x99 two\n// c \xe2\x80\x9c three\n// d \xe2\x80\x9d four\nfunc F() {}\n' > "$R/p.go"
git -C "$R" add -A
run_gate "$R"
expect_rc 1 "a file carrying all four characters FAILS"
expect_hit "FAIL — 4 typographic quote(s)" "each of the four is its own finding"
expect_hit "U+2018" "U+2018 is found"
expect_hit "U+2019" "U+2019 is found"
expect_hit "U+201C" "U+201C is found"
expect_hit "U+201D" "U+201D is found"

# ── 5. waivers ─────────────────────────────────────────────────────────────
echo "lint-go-typographic-quotes-test: the inline waiver"

R="$TMP/waived"; new_repo "$R" || exit 2
printf 'package p\n\n// asserts the mangling \xe2\x80\x9d lint-quotes:ok: fixture for the gofumpt rewrite\nfunc F() {}\n' > "$R/p.go"
git -C "$R" add -A
run_gate "$R"
expect_rc 0 "a waiver with a reason, on the offending line, waives"
expect_hit "1 waived" "the waiver is COUNTED in the summary, not hidden"

R="$TMP/waived-above"; new_repo "$R" || exit 2
printf 'package p\n\n// lint-quotes:ok: the next line asserts the gofumpt rewrite\nvar S = "\xe2\x80\x9d"\nfunc F() {}\n' > "$R/p.go"
git -C "$R" add -A
run_gate "$R"
expect_rc 0 "a waiver on the line directly above waives"

R="$TMP/waived-noreason"; new_repo "$R" || exit 2
printf 'package p\n\n// mangled \xe2\x80\x9d lint-quotes:ok\nfunc F() {}\n' > "$R/p.go"
git -C "$R" add -A
run_gate "$R"
expect_rc 1 "the bare marker with NO reason does not waive"

# ── 6. vacuity ─────────────────────────────────────────────────────────────
echo "lint-go-typographic-quotes-test: refuses to pass vacuously"

R="$TMP/empty"; new_repo "$R" || exit 2
printf 'not go\n' > "$R/README.md"
git -C "$R" add -A
run_gate "$R"
expect_rc 2 "a tree with no Go file in scope exits 2"
expect_hit "refusing to pass vacuously" "and says so rather than printing OK"

# ── 7. scope: vendored, generated, untracked ───────────────────────────────
echo "lint-go-typographic-quotes-test: scope"

R="$TMP/scope"; new_repo "$R" || exit 2
mkdir -p "$R/vendor/example.com/lib"
printf 'package lib\n\n// vendored \xe2\x80\x9d prose\nfunc F() {}\n' > "$R/vendor/example.com/lib/lib.go"
printf '// Code generated by stringer. DO NOT EDIT.\n\npackage p\n\n// generated \xe2\x80\x9d prose\nfunc G() {}\n' > "$R/gen.go"
printf 'package p\n\n// clean prose\nfunc H() {}\n' > "$R/ok.go"
git -C "$R" add -A
run_gate "$R"
expect_rc 0 "a vendored and a generated file are skipped, the clean one is scanned"
expect_hit "OK — 1 Go file(s)" "only the one in-scope file is counted as scanned"
expect_hit "2 skipped" "the two skipped files are accounted for, not silently dropped"

R="$TMP/untracked"; new_repo "$R" || exit 2
printf 'package p\n\n// clean\nfunc F() {}\n' > "$R/ok.go"
git -C "$R" add -A
printf 'package p\n\n// mangled \xe2\x80\x9d prose\nfunc G() {}\n' > "$R/untracked.go"
run_gate "$R"
expect_rc 0 "an UNTRACKED bad file is out of scope (discovery is git ls-files)"
git -C "$R" add -A
run_gate "$R"
expect_rc 1 "the same file, once tracked, is caught"

# An explicit file list is also accepted, for a caller that wants to scope it.
run_gate "$R" "$R/untracked.go"
expect_rc 1 "an explicit path is scanned"

# ── 8. a Go file that is not valid UTF-8 ───────────────────────────────────
echo "lint-go-typographic-quotes-test: undecodable source"

R="$TMP/binary"; new_repo "$R" || exit 2
printf 'package p\n\n// \xff\xfe not utf-8\nfunc F() {}\n' > "$R/p.go"
git -C "$R" add -A
run_gate "$R"
expect_rc 1 "a Go file that is not valid UTF-8 is a finding, not a crash"
expect_hit "not valid UTF-8" "and the finding says why"

# ── 9. the gate must not scan with grep ────────────────────────────────────
# Not style policing: the first sweep for these characters used a basic
# regular expression with `\|` alternation and came back clean, because
# POSIX BRE has no alternation and a grep without the GNU extension matches
# a literal `|` instead — exit 1, no output, indistinguishable from a pass.
# `grep -P` is not the escape either: /usr/bin/grep on macOS is BSD grep and
# rejects -P outright. Developers here work on macOS and CI runs Linux, so
# the scan has to behave identically on both; Python decodes the file and
# has no dialect. If someone rewrites the gate around grep, this says why
# not before the false negative does.
echo "lint-go-typographic-quotes-test: no grep in the scan path"

if grep -nE '(subprocess|os\.system|os\.popen|check_output)[^#]*grep' "$SRC" > /dev/null 2>&1; then
  bad "the gate shells out to grep — BRE \\| alternation and -P are not portable across BSD and GNU grep"
else
  ok "the gate does not shell out to grep (it decodes the file in Python instead)"
fi

echo
echo "lint-go-typographic-quotes-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
