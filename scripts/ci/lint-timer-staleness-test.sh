#!/usr/bin/env bash
# lint-timer-staleness-test.sh — fixture tests for lint-timer-staleness.sh.
# Run: bash scripts/ci/lint-timer-staleness-test.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-timer-staleness.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0

mkdir -p "$TMP/roles/r/templates/systemd" "$TMP/rules"
touch "$TMP/roles/r/templates/systemd/named.timer.j2" "$TMP/roles/r/templates/systemd/escaped.timer.j2"
cat >"$TMP/rules/infra.yml" <<'EOF'
groups:
  - name: g
    rules:
      # commented.timer is mentioned only here
      - alert: a
        expr: absent(node_systemd_timer_last_trigger_seconds{name="named.timer"})
          or node_systemd_timer_last_trigger_seconds{name=~"escaped\\.timer"} > 1
EOF

run() { # $1 = case name, $2 = expected rc, $3 = baseline body
  printf '%s' "$3" >"$TMP/baseline"
  OUT="$(ROLES_DIR="$TMP/roles" RULES_DIR="$TMP/rules" BASELINE="$TMP/baseline" bash "$LINT" 2>&1)"
  rc=$?
  if [ "$rc" = "$2" ]; then
    echo "ok: $1"; pass=$((pass + 1))
  else
    echo "FAIL: $1 (rc=$rc, want $2)" >&2; printf '%s\n' "$OUT" >&2; fail=$((fail + 1))
  fi
}

run 'every timer named by a rule (plain and regex-escaped) → OK' 0 ''
touch "$TMP/roles/r/templates/systemd/commented.timer.j2"
run 'a timer named only in a comment → FAIL' 1 ''
run 'that timer baselined with a reason → OK' 0 'commented.timer  # covered elsewhere
'
run 'baselined without a reason → FAIL' 1 'commented.timer
'
run 'baseline entry a rule now names → FAIL (shrink only)' 1 'commented.timer  # x
named.timer  # stale entry
'
run 'baseline entry no role installs → FAIL' 1 'commented.timer  # x
gone.timer  # removed unit
'
rm "$TMP/roles/r/templates/systemd/"*.j2
run 'no timers at all → FAIL (never passes vacuously)' 1 ''

echo "---"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
