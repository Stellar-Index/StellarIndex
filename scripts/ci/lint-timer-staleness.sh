#!/usr/bin/env bash
# lint-timer-staleness.sh — every systemd timer an ansible role installs must be
# named by an alert rule, or listed in timer-staleness.baseline with a reason.
#
# A timer that stops firing (disabled, unloaded, a bad OnCalendar) leaves no
# failed unit, so stellarindex_systemd_unit_failed never sees it and the table
# the job writes freezes in silence. A rule that names the timer
# (stellarindex_oneshot_timer_stale and its kin) is what notices. Comment lines
# in rule files do not count.
#
# The baseline can only shrink: an entry whose timer is now named by a rule, or
# no longer exists, fails too.
#
# Env overrides (used by the self-test): ROLES_DIR, RULES_DIR, BASELINE.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROLES_DIR="${ROLES_DIR:-configs/ansible/roles}"
RULES_DIR="${RULES_DIR:-configs/prometheus/rules.r1}"
BASELINE="${BASELINE:-scripts/ci/timer-staleness.baseline}"
for p in "$ROLES_DIR" "$RULES_DIR" "$BASELINE"; do
  [ -e "$p" ] || { echo "lint-timer-staleness: FAIL — not found: $p" >&2; exit 1; }
done

timers="$(find "$ROLES_DIR" \( -name '*.timer' -o -name '*.timer.j2' \) -type f \
  | sed -E 's#.*/##; s/\.j2$//' | sort -u)"
[ -n "$timers" ] || { echo "lint-timer-staleness: FAIL — no timer units under $ROLES_DIR (pattern drift?)" >&2; exit 1; }

# Rule text with comment lines removed, and `\\.` (a regex-escaped dot in YAML) read as `.`.
rules="$(cat "$RULES_DIR"/*.yml | grep -vE '^[[:space:]]*#' | sed 's/\\\\\././g' || true)"

fail=0
total=0
covered=0
baselined=0
for t in $timers; do
  total=$((total + 1))
  named=0
  if grep -qF "$t" <<<"$rules"; then named=1; fi
  entry="$(grep -E "^${t//./\\.}[[:space:]]" "$BASELINE" || true)"
  if [ "$named" = 1 ]; then
    covered=$((covered + 1))
    if [ -n "$entry" ]; then
      echo "lint-timer-staleness: FAIL — $t is now named by a rule; remove it from $BASELINE" >&2
      fail=1
    fi
  elif [ -n "$entry" ]; then
    baselined=$((baselined + 1))
    if ! grep -qE '#[[:space:]]*[^[:space:]]' <<<"$entry"; then
      echo "lint-timer-staleness: FAIL — $BASELINE entry for $t carries no reason" >&2
      fail=1
    fi
  else
    echo "lint-timer-staleness: FAIL — $t is installed by a role but no alert rule names it." >&2
    echo "  Add it to stellarindex_oneshot_timer_stale (configs/prometheus/rules.r1/infra.yml and" >&2
    echo "  deploy/monitoring/rules/infra.yml), or list it in $BASELINE with what covers it." >&2
    fail=1
  fi
done

while IFS= read -r line; do
  name="$(printf '%s\n' "$line" | sed -E 's/#.*//; s/[[:space:]]+//g')"
  [ -n "$name" ] || continue
  if ! grep -qxF "$name" <<<"$timers"; then
    echo "lint-timer-staleness: FAIL — $BASELINE lists $name, which no role installs; remove it" >&2
    fail=1
  fi
done <"$BASELINE"

if [ "$fail" = 0 ]; then
  echo "lint-timer-staleness: OK — $total timer(s): $covered named by a rule, $baselined baselined"
fi
exit "$fail"
