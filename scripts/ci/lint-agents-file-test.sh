#!/usr/bin/env bash
# lint-agents-file-test.sh — prove each assertion in lint-agents-file.sh
# can actually FAIL. A gate nobody has seen red is not a gate; this repo
# found 11 of 37 CI gates vacuous in an August audit, so every new one
# ships with the mutation that breaks it.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-agents-file.sh"
PASS=0; FAIL=0
check() { # <name> <expected: ok|red>
  local name="$1" want="$2" rc
  bash "$GATE" >/dev/null 2>&1; rc=$?
  if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = red ] && [ "$rc" -gt 0 ]; }; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$name" "$rc" "$want"; FAIL=$((FAIL+1))
  fi
}

BAK="$(mktemp)"; cp AGENTS.md "$BAK"
restore() { cp "$BAK" AGENTS.md; }
trap 'restore; rm -f "$BAK"' EXIT

check "unmodified AGENTS.md passes" ok

# 1. length ceiling
{ cat "$BAK"; for i in $(seq 1 200); do echo "padding line $i"; done; } > AGENTS.md
check "over-long file is rejected" red
restore

# 2. reference section
{ cat "$BAK"; printf '\n## Repo map\n\n- internal/ — things\n'; } > AGENTS.md
check "a reintroduced repo map is rejected" red
restore

# 3. emptied of rules — the vacuous-pass case
printf '# Stellar Index\n\nA project.\n' > AGENTS.md
check "a short file with no directives is rejected" red
restore

# 4. self-referential link
{ cat "$BAK"; printf '\nSee [AGENTS.md](AGENTS.md) for more.\n'; } > AGENTS.md
check "a self-referential link is rejected" red
restore

# 5. dangling relative link
{ cat "$BAK"; printf '\nSee [gone](docs/this-does-not-exist.md).\n'; } > AGENTS.md
check "a dangling relative link is rejected" red
restore

# The synonym / depth holes an earlier cut let through, all proven green
# before the fix.
for h in "## Directory structure" "## Repository map" "## Where things live" \
         "## Common task recipes" "#### Repo map"; do
  { cat "$BAK"; printf '\n%s\n\nstuff\n' "$h"; } > AGENTS.md
  check "reference section under '$h' is rejected" red
  restore
done

# The false positive that made a legitimate commands file red.
{ cat "$BAK"; printf '\n```sh\n# changelog entry goes under [Unreleased]\nmake verify\n```\n'; } > AGENTS.md
check "the word changelog inside a fenced block is NOT a heading" ok
restore

check "restored AGENTS.md passes again" ok

echo
echo "lint-agents-file-test: $PASS passed, $FAIL failed"
exit "$FAIL"
