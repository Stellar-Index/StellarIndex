#!/usr/bin/env bash
# lint-doc-links-test.sh — prove the link gate can fail, and prove each of
# its exclusions still holds. A gate nobody has seen red is not a gate.
#
# The fixture is deliberately UNTRACKED and the git index is never touched.
# An earlier cut used `git add -N` / `git rm --cached` because the gate read
# tracked files only; that raced with any concurrent `git add` during the
# ~15-minute backgrounded verify.sh run. The gate now reads untracked,
# non-ignored files too, so the fixture is visible to it as a plain file.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-doc-links.sh"
FIX="docs/zz-lint-doc-links-fixture.md"
PASS=0; FAIL=0
# shellcheck disable=SC2329  # invoked indirectly by the EXIT trap
cleanup() { rm -f "$FIX"; }
trap cleanup EXIT

check() { # <name> <expected ok|red>
  local name="$1" want="$2" rc
  bash "$GATE" >/dev/null 2>&1; rc=$?
  if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = red ] && [ "$rc" -gt 0 ]; }; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$name" "$rc" "$want"; FAIL=$((FAIL+1))
  fi
}

check "clean tree passes" ok

printf '# fixture\n\n[gone](./does-not-exist-%s.md)\n' "$$" > "$FIX"
check "a broken relative link is caught" red

# The hole that made this gate blind to the very files a change creates.
check "an UNTRACKED file is scanned, not skipped" red

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
# CI failed on three such links.
mkdir -p docs/zz-ignored-fixture-dir
printf 'x\n' > docs/zz-ignored-fixture-dir/target.md
printf '/docs/zz-ignored-fixture-dir/\n' >> .gitignore
printf '# fixture\n\n[ignored target](zz-ignored-fixture-dir/target.md)\n' > "$FIX"
check "a link to a GITIGNORED target is caught" red
git checkout -- .gitignore 2>/dev/null || sed -i.bak '/zz-ignored-fixture-dir/d' .gitignore
rm -rf docs/zz-ignored-fixture-dir .gitignore.bak

rm -f "$FIX"
check "clean tree passes again" ok

echo
echo "lint-doc-links-test: $PASS passed, $FAIL failed"
exit "$FAIL"
