#!/usr/bin/env bash
# lint-attribution-test.sh — proves lint-attribution.sh (F167/F172) catches
# an agent/vendor self-attribution marker landing in tracked doc content
# AND one landing in a commit message, and stays clean without either.
#
# The tree-content check scans `git ls-files` (tracked content — what a
# fresh CI checkout actually sees), so the doc fixture needs `git add -N`
# (intent-to-add) to become visible to it; `git reset` drops that index
# entry again without ever creating a commit. The commit-message case needs
# a REAL commit for `git log` to see it, so this creates exactly one
# throwaway commit and unwinds it with `git reset --soft` immediately after
# — it never touches any commit that existed before this test ran, and
# nothing here is pushed.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-attribution.sh"
FIX="docs/zz-lint-attribution-fixture.md"
OUT=$(mktemp)
PASS=0
FAIL=0

# shellcheck disable=SC2329  # invoked indirectly by the EXIT trap
cleanup() {
  git reset -q -- "$FIX" 2>/dev/null
  rm -f "$FIX" "$OUT"
}
trap cleanup EXIT

check() { # <name> <expected ok|red> [range] [must-contain-substring]
  local name="$1" want="$2" range="${3:-}" needle="${4:-}" rc
  bash "$GATE" "$range" >"$OUT" 2>&1
  rc=$?
  if [ "$want" = ok ]; then
    if [ "$rc" -eq 0 ]; then
      printf '  ok   %s\n' "$name"
      PASS=$((PASS + 1))
    else
      printf '  FAIL %s (rc=%s, wanted ok)\n' "$name" "$rc"
      FAIL=$((FAIL + 1))
      sed -n '1,40p' "$OUT"
    fi
  else
    if [ "$rc" -ne 0 ] && { [ -z "$needle" ] || grep -qF "$needle" "$OUT"; }; then
      printf '  ok   %s\n' "$name"
      PASS=$((PASS + 1))
    else
      printf '  FAIL %s (rc=%s, wanted red containing %q)\n' "$name" "$rc" "$needle"
      FAIL=$((FAIL + 1))
      sed -n '1,40p' "$OUT"
    fi
  fi
}

check "clean tree, no range: passes" ok

# gitleaks:allow — placeholder trailer text, not a real credential.
printf '# fixture\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n' >"$FIX"
git add -N "$FIX"
check "a Co-Authored-By/Claude trailer in tracked doc content is caught" red "" \
  "self-attribution marker"
git reset -q -- "$FIX"
rm -f "$FIX"
check "clean tree passes again after removing the doc fixture" ok

before=$(git rev-parse HEAD)
# gitleaks:allow — placeholder trailer text, not a real credential.
# A CI runner has no git identity; give the throwaway commit one.
git -c user.name=lint-attribution-test -c user.email=lint-attribution-test@invalid \
  commit --allow-empty -q -m "test: throwaway fixture commit

Co-Authored-By: Claude <noreply@anthropic.com>"
after=$(git rev-parse HEAD)
check "a Co-Authored-By/Claude trailer in a commit message is caught" red "${before}..${after}" \
  "self-attribution marker in its message"
git reset --soft "$before" -q
check "clean tree passes again after unwinding the fixture commit" ok

echo
echo "lint-attribution-test: $PASS passed, $FAIL failed"
exit "$FAIL"
