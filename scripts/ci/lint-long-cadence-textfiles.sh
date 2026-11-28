#!/usr/bin/env bash
# lint-long-cadence-textfiles.sh — a role timer that fires less often than daily
# must be declared next to stellarindex_textfile_producer_stale (24h), or the
# catch-all fires on its textfile for most of the interval.
#
# A timer is long-cadence when its OnCalendar starts with a weekday or a fixed
# day of the month. The timer-to-textfile mapping lives in Go flags and unit
# files, so it is declared, not derived, in a comment beside the rule:
#   # long-cadence <timer>: <textfile stem | none> <owning alert | ->
# For a textfile the stem must appear in the rule's file regex (excluded from the
# catch-all) and the owning alert must exist in the same tree.
#
# Env overrides (used by the self-test): ROLES_DIR, RULES_DIRS (space-separated).
set -euo pipefail
cd "$(dirname "$0")/../.."
ROLES_DIR="${ROLES_DIR:-configs/ansible/roles}"
RULES_DIRS="${RULES_DIRS:-deploy/monitoring/rules configs/prometheus/rules.r1}"
[ -d "$ROLES_DIR" ] || { echo "lint-long-cadence-textfiles: FAIL — not found: $ROLES_DIR" >&2; exit 1; }

timers="$(find "$ROLES_DIR" -name '*.timer.j2' -type f | LC_ALL=C sort)"
[ -n "$timers" ] || { echo "lint-long-cadence-textfiles: FAIL — no timer units under $ROLES_DIR (pattern drift?)" >&2; exit 1; }

long=""
for f in $timers; do
  cal="$(sed -n 's/^OnCalendar=//p' "$f")"
  cal="${cal%%$'\n'*}"
  first="${cal%% *}"
  if [[ "$first" =~ ^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) ]] || [[ "$first" =~ ^[*0-9]+-[*0-9]+-[0-9] ]]; then
    long="$long $(basename "$f" .j2)"
  fi
done

fail=0
for d in $RULES_DIRS; do
  rf="$d/storage.yml"
  [ -f "$rf" ] || { echo "lint-long-cadence-textfiles: FAIL — not found: $rf" >&2; exit 1; }
  stale="$(awk '/- alert: stellarindex_textfile_producer_stale/{on=1; next} on && /- alert:/{exit} on && !/^[[:space:]]*#/' "$rf")"
  [ -n "$stale" ] || { echo "lint-long-cadence-textfiles: FAIL — no stellarindex_textfile_producer_stale rule in $rf" >&2; exit 1; }
  for t in $long; do
    line="$(grep -E "^[[:space:]]*# long-cadence ${t//./\\.}:" "$rf" || true)"
    if [ -z "$line" ]; then
      echo "lint-long-cadence-textfiles: FAIL — $t fires less often than daily but $rf has no '# long-cadence $t: <textfile|none> <alert|->' line" >&2
      fail=1
      continue
    fi
    read -r stem alert <<<"$(sed -E 's/^[^:]*:[[:space:]]*//' <<<"$line")"
    [ "$stem" = none ] && continue
    if ! grep -qF "$stem" <<<"$stale"; then
      echo "lint-long-cadence-textfiles: FAIL — $t writes $stem but the catch-all in $rf does not exclude it" >&2
      fail=1
    fi
    if [ "${alert:--}" = "-" ] || ! grep -qE "^[[:space:]]*- alert: ${alert}[[:space:]]*$" "$d"/*.yml; then
      echo "lint-long-cadence-textfiles: FAIL — $t: owning alert '${alert:--}' is not defined under $d" >&2
      fail=1
    fi
  done
done

read -ra long_arr <<<"$long"
read -ra tree_arr <<<"$RULES_DIRS"
[ "$fail" = 0 ] && echo "lint-long-cadence-textfiles: OK — ${#long_arr[@]} long-cadence timer(s) declared in ${#tree_arr[@]} tree(s)"
exit "$fail"
