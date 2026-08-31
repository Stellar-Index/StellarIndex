#!/usr/bin/env bash
# lint-unit-failed-baseline-test.sh — keeps the catch-all failed-unit
# alert's exclusion list honest.
#
# stellarindex_systemd_unit_failed covers EVERY systemd unit except those
# named in scripts/ci/unit-failed-dedicated.baseline. That inversion is
# the whole point: naming units individually is how ~20 oneshot timers
# — including directory-sync, sole writer of the table the scam-pricing
# gate reads — ended up in no alert at all (wave-D LID-6).
#
# An exclusion list is one edit away from becoming a suppression list.
# This asserts:
#   1. every excluded unit is genuinely named in a rule file, so an
#      entry cannot silence a unit nothing else watches;
#   2. the catch-all's own regex matches the baseline exactly, so an
#      entry added to one and not the other is caught;
#   3. the baseline is non-empty and the rule exists — a check with an
#      empty subject set passes forever.
#
# Run: bash scripts/ci/lint-unit-failed-baseline-test.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

BASELINE=scripts/ci/unit-failed-dedicated.baseline
RULES=configs/prometheus/rules.r1
pass=0; fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail+1)); }

echo "lint-unit-failed-baseline-test: catch-all exclusion list"

[ -f "$BASELINE" ] || { echo "  FAIL baseline missing: $BASELINE"; exit 1; }

units=$(grep -vE '^\s*#|^\s*$' "$BASELINE" | sed -E 's/\s*#.*//; s/\s+$//')
if [ -z "$units" ]; then
  bad "baseline is empty — the catch-all would cover everything and this test would assert nothing"
else
  ok "baseline is non-empty"
fi

# 1. Each excluded unit must be named in some rule file.
while IFS= read -r u; do
  [ -z "$u" ] && continue
  if grep -rqF "name=\"$u\"" "$RULES"/*.yml 2>/dev/null; then
    ok "$u is named by a dedicated alert"
  else
    bad "$u is excluded from the catch-all but named in NO rule file — that is a silenced unit, not a covered one"
  fi
done <<< "$units"

# 2. The rule's negative-match regex must list exactly the baseline.
expr_line=$(grep -h 'node_systemd_unit_state{state="failed"' "$RULES"/infra.yml 2>/dev/null | head -1)
if [ -z "$expr_line" ]; then
  bad "stellarindex_systemd_unit_failed not found in $RULES/infra.yml"
else
  ok "catch-all rule is present"
  missing=0
  while IFS= read -r u; do
    [ -z "$u" ] && continue
    # PromQL string literals unescape `\\` to `\`, so a regex `\.` is
    # written `\\.` in the rule file. Match that form — an earlier
    # version of this test expected a single backslash and reported the
    # CORRECT rule as broken.
    esc=$(printf '%s' "$u" | sed 's/\./\\\\./g')
    case "$expr_line" in *"$esc"*) ;; *) bad "baseline entry '$u' is absent from the rule's exclusion regex"; missing=1 ;; esac
  done <<< "$units"
  [ "$missing" -eq 0 ] && ok "every baseline entry appears in the rule's exclusion regex"

  # 3. THE OTHER DIRECTION. Check 2 only walks baseline -> regex, while
  # this file's own header claimed "the catch-all's own regex matches the
  # baseline exactly, so an entry added to one and not the other is
  # caught". Half of that was true. The DANGEROUS half was unchecked: a
  # unit added to the EXCLUSION REGEX with no baseline entry is silently
  # exempted from the catch-all and has no dedicated alert either — it
  # simply stops being watched, which is the failure the catch-all exists
  # to prevent. directory-sync.service (sole writer of account_directory,
  # the table the scam-pricing gate reads) is exactly the kind of unit
  # that could vanish that way.
  extra=0
  # Pull every unit name out of the regex alternation, undo PromQL's
  # `\\.` -> `\.` escaping, and require a baseline line for each.
  regex_units=$(printf '%s' "$expr_line" \
    | grep -oE '[a-zA-Z0-9@_.-]+\\\\\.(service|timer|socket|mount|scope)' \
    | sed 's/\\\\\./\./g' | sort -u)
  while IFS= read -r u; do
    [ -z "$u" ] && continue
    case "$units" in *"$u"*) ;; *) bad "unit '$u' is excluded by the rule's regex but has NO baseline entry — it is exempt from the catch-all and unwatched"; extra=1 ;; esac
  done <<< "$regex_units"
  [ "$extra" -eq 0 ] && ok "every unit in the exclusion regex has a baseline entry"
fi

echo "----"
echo "lint-unit-failed-baseline-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
