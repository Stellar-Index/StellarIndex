#!/usr/bin/env bash
# lint-long-cadence-textfiles-test.sh — fixture tests for lint-long-cadence-textfiles.sh.
# Run: bash scripts/ci/lint-long-cadence-textfiles-test.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-long-cadence-textfiles.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0

SD="$TMP/roles/r/templates/systemd"
mkdir -p "$SD" "$TMP/rules"
printf 'OnCalendar=*-*-* 04:00:00 UTC\n' >"$SD/daily.timer.j2"
printf 'OnCalendar=Sat *-*-* 10:05:00 UTC\n' >"$SD/weekly.timer.j2"

# $1 = marker lines, $2 = regex alternation inside the catch-all, $3 = extra alert name
write_rules() {
  cat >"$TMP/rules/storage.yml" <<EOF
groups:
  - name: g
    rules:
$1
      - alert: stellarindex_textfile_producer_stale
        expr: |
          time() - node_textfile_mtime_seconds{file!~".*/($2)\\\\.prom\$"} > 86400
      - alert: other_alert
        expr: vector(1)
      - alert: $3
        expr: vector(1)
EOF
}

run() { # $1 = case name, $2 = expected rc
  OUT="$(ROLES_DIR="$TMP/roles" RULES_DIRS="$TMP/rules" bash "$LINT" 2>&1)"
  rc=$?
  if [ "$rc" = "$2" ]; then
    echo "ok: $1"; pass=$((pass + 1))
  else
    echo "FAIL: $1 (rc=$rc, want $2)" >&2; printf '%s\n' "$OUT" >&2; fail=$((fail + 1))
  fi
}

M='      # long-cadence weekly.timer: ops_job_weekly weekly_alert'
write_rules "$M" 'ops_job_weekly' weekly_alert
run 'weekly timer declared, excluded and owned → OK' 0

write_rules '      # nothing declared' 'ops_job_weekly' weekly_alert
run 'weekly timer with no declaration → FAIL' 1

write_rules "$M" 'ops_job_other' weekly_alert
run 'declared textfile missing from the catch-all regex → FAIL' 1

write_rules "$M" 'ops_job_weekly' unrelated_alert
run 'owning alert not defined → FAIL' 1

write_rules '      # long-cadence weekly.timer: none -' 'ops_job_other' unrelated_alert
run 'timer declared as writing no textfile → OK' 0

write_rules '      # long-cadence weekly.timer: ops_job_weekly weekly_alert
      # (commented) - alert: weekly_alert' 'ops_job_weekly' unrelated_alert
run 'a commented-out alert does not count as the owner → FAIL' 1

rm "$SD"/*.j2
write_rules '      # none' 'x' weekly_alert
run 'no timers at all → FAIL (never passes vacuously)' 1

echo "---"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
