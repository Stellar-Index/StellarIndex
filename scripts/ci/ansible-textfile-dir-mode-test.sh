#!/usr/bin/env bash
# ansible-textfile-dir-mode-test.sh — pins the node_exporter textfile-collector
# dir's ownership and mode in every archival-node task that declares it. The
# .prom files there feed the SLA / archive-completeness / backup alerts.
#   - Two tasks declare the dir; if they disagree, alternate applies flip it.
#   - Group-write (0775) exists for the units that write as non-owners via
#     SupplementaryGroups=stellarindex (pgbackrest-backup as postgres; the
#     DynamicUser sla-probe, smoke and heartbeat@ healthchecks). Dropping
#     group-write silently loses their metrics; world-write lets anyone forge them.
#
# Structural only (awk/grep over the real role files) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="${ROLE:-configs/ansible/roles/archival-node}"
DIR=/var/lib/node_exporter/textfile_collector
WANT="stellarindex:stellarindex:0775"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

# One "<file>|<owner>:<group>:<mode>" line per uncommented task that
# declares $DIR as a directory.
decls="$(awk -v dir="$DIR" '
  function flush() {
    if (hit && isdir) print file "|" owner ":" group ":" mode
    hit = 0; isdir = 0; owner = ""; group = ""; mode = ""
  }
  FNR == 1 { flush(); file = FILENAME }
  /^- name:/ { flush() }
  /^[ \t]*#/ { next }
  { t = $0; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t); gsub(/"/, "", t) }
  t == "path: " dir { hit = 1 }
  t == "state: directory" { isdir = 1 }
  t ~ /^owner: / { owner = substr(t, 8) }
  t ~ /^group: / { group = substr(t, 8) }
  t ~ /^mode: /  { mode = substr(t, 7) }
  END { flush() }' "$ROLE"/tasks/*.yml)"

if [ -z "$decls" ]; then
  echo "ansible-textfile-dir-mode-test: FAIL — no task declares $DIR under $ROLE/tasks" >&2
  exit 1
fi

while IFS='|' read -r file got; do
  if [ "$got" = "$WANT" ]; then
    ok "$file: $got"
  else
    bad "$file: $got — want $WANT (every declaring task must agree)"
  fi
done <<<"$decls"

# Every unit that joins group stellarindex is a group writer the 0775 mode
# exists for. Under ProtectSystem=strict it also needs the dir in
# ReadWritePaths, or its write fails EROFS despite the group bit.
UNIT_DIRS="$ROLE/templates/systemd configs/healthchecks"
writers="$(grep -lE '^SupplementaryGroups=(.* )?stellarindex( |$)' \
  "$ROLE"/templates/systemd/*.service* configs/healthchecks/*.service 2>/dev/null)"
if [ -z "$writers" ]; then
  bad "no unit under $UNIT_DIRS joins group stellarindex — the 0775 mode has no group writer left; re-derive it in both declaring tasks together"
fi
while IFS= read -r unit; do
  [ -n "$unit" ] || continue
  if grep -qE '^ProtectSystem=strict$' "$unit" &&
    ! grep -qE "^ReadWritePaths=(.* )?$DIR/?( |$)" "$unit"; then
    bad "$unit: SupplementaryGroups=stellarindex under ProtectSystem=strict without ReadWritePaths=$DIR"
  else
    ok "group writer (needs 0775): $unit"
  fi
done <<<"$writers"

echo "ansible-textfile-dir-mode-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
