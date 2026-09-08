#!/usr/bin/env bash
# lint-doc-links-test.sh — prove the link gate can fail, and prove each of
# its exclusions still holds. A gate nobody has seen red is not a gate.
#
# The fixture is deliberately UNTRACKED and the git index is never touched.
# An earlier cut used `git add -N` / `git rm --cached` because the gate read
# tracked files only; that raced with any concurrent `git add` during the
# ~15-minute backgrounded verify.sh run. The gate now reads untracked,
# non-ignored files too, so the fixture is visible to it as a plain file.
#
# SCOPE — most checks below run the gate SCOPED to just the fixture file
# (lint-doc-links.sh now takes a file list; see lint_doc_links.py's
# docstring). That is safe here for exactly the reason it is safe in
# production use: scoping narrows what gets SCANNED as a link source, never
# what a target resolves against, so a fixture link into ../README.md or
# ../CONTRIBUTING.md is still checked in full either way — proven by the
# checks below covering both an anchor hit and a gitignored-target miss.
# Full-tree (no file list) runs are kept for exactly the checks that are
# ABOUT the whole-tree discovery mechanism itself, so that mechanism stays
# under test: whether an untracked file is scanned at all, and two "is the
# tree actually clean" bookends. Before this split every one of the 11
# checks below ran a full 607-file scan, at ~21 s each — a self-test costing
# ten times the lint it covers.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-doc-links.sh"
FIX="docs/zz-lint-doc-links-fixture.md"
PASS=0; FAIL=0
# shellcheck disable=SC2329  # invoked indirectly by the EXIT trap
cleanup() { rm -f "$FIX"; }
trap cleanup EXIT

check() { # <name> <expected ok|red> [full] — scoped to $FIX unless "full"
          # is passed, in which case the gate runs its default whole-tree
          # discovery with no file list.
  local name="$1" want="$2" scope="${3:-scoped}" rc
  if [ "$scope" = full ]; then
    bash "$GATE" >/dev/null 2>&1; rc=$?
  else
    bash "$GATE" "$FIX" >/dev/null 2>&1; rc=$?
  fi
  if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = red ] && [ "$rc" -gt 0 ]; }; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$name" "$rc" "$want"; FAIL=$((FAIL+1))
  fi
}

# Wall-clock budget: the ok/red verdict on every check below is identical
# whether the gate scans one file or all 607 — a boolean pass/fail cannot
# tell scoped mode from a regression back to always-full-scan, only the
# clock can. t0 brackets the whole run; single_scan_s times the very first
# check's full-tree scan, so the budget below scales with THIS machine's
# speed rather than a hardcoded second count.
t0=$SECONDS
check "clean tree passes" ok full
single_scan_s=$((SECONDS - t0))
[ "$single_scan_s" -ge 1 ] || single_scan_s=1

printf '# fixture\n\n[gone](./does-not-exist-%s.md)\n' "$$" > "$FIX"
check "a broken relative link is caught" red

# The hole that made this gate blind to the very files a change creates.
# This one MUST run the default whole-tree discovery (not a file-list
# scope) — passing the fixture explicitly would trivially "discover" it
# and prove nothing about the untracked-file scan this check exists for.
check "an UNTRACKED file is scanned, not skipped" red full

printf '# fixture\n\n[bad anchor](../README.md#no-such-heading-%s)\n' "$$" > "$FIX"
check "a link to a non-existent heading anchor is caught" red

printf '# fixture\n\n[ok anchor](../CONTRIBUTING.md#no-orphan-work--the-contract)\n' > "$FIX"
check "a real anchor with a double hyphen resolves" ok

printf '# fixture\n\n```\n[gone](./nope-%s.md)\n' "$$" > "$FIX"
check "an odd number of fences is reported, not silently trusted" red

# shellcheck disable=SC2016  # the backticks are literal fixture content
printf '# fixture\n\nXDR notation: `Vec[Address](assets)` and `Vec[i128](amounts)`.\n' > "$FIX"
check "a link-shaped token inside backticks is ignored" ok

# shellcheck disable=SC2016  # the fence is literal fixture content
printf '# fixture\n\n```\n[gone](./nope-%s.md)\n```\n' "$$" > "$FIX"
check "a link inside a balanced fenced block is ignored" ok

# A target that exists on THIS machine but is gitignored does not exist for
# anyone else. Checking the filesystem alone made the gate green locally while
# CI failed on three such links. Scoped mode still catches this because
# _ignored() resolves the target against git, unrestricted to the source
# file list — exactly the subtlety this test exists to pin down.
mkdir -p docs/zz-ignored-fixture-dir
printf 'x\n' > docs/zz-ignored-fixture-dir/target.md
printf '/docs/zz-ignored-fixture-dir/\n' >> .gitignore
printf '# fixture\n\n[ignored target](zz-ignored-fixture-dir/target.md)\n' > "$FIX"
check "a link to a GITIGNORED target is caught" red
git checkout -- .gitignore 2>/dev/null || sed -i.bak '/zz-ignored-fixture-dir/d' .gitignore
rm -rf docs/zz-ignored-fixture-dir .gitignore.bak

rm -f "$FIX"
check "clean tree passes again" ok full

# This run does 3 full-tree scans (clean/untracked/clean-again) plus 8
# scoped, near-instant ones; a regression back to always-full-scan does 11
# full-tree scans. 6x a single scan sits well above the 3 this run needs and
# well below the 11 the defect this test exists to catch would cost.
total_s=$((SECONDS - t0))
budget_s=$((single_scan_s * 6))
if [ "$total_s" -le "$budget_s" ]; then
  printf '  ok   %s\n' "self-test cost stays near 3 full scans, not 10 (${total_s}s <= ${budget_s}s budget, single scan ~${single_scan_s}s)"
  PASS=$((PASS+1))
else
  printf '  FAIL %s\n' "self-test cost stays near 3 full scans, not 10 (${total_s}s > ${budget_s}s budget, single scan ~${single_scan_s}s — a check likely regressed to an unscoped full-tree call)"
  FAIL=$((FAIL+1))
fi

echo
echo "lint-doc-links-test: $PASS passed, $FAIL failed"
exit "$FAIL"
