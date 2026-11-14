#!/usr/bin/env bash
# ansible-textfile-dir-mode-test.sh — pins the node_exporter textfile-collector
# dir's ownership and mode in every archival-node task that declares it. The
# .prom files there feed the SLA / archive-completeness / backup alerts.
#   - Two tasks declare the dir; if they disagree, alternate applies flip it.
#   - Group-write (1775) exists for the units that write as non-owners via
#     SupplementaryGroups=stellarindex (pgbackrest-backup as postgres; the
#     DynamicUser sla-probe, smoke and heartbeat@ healthchecks). Dropping
#     group-write silently loses their metrics; world-write lets anyone forge them.
#     The sticky bit stops one group writer deleting or renaming over another's file.
#   - The owner is root: root jobs write here, and a directory's owner can rename
#     any entry whatever the sticky bit says. The HA roles declare the same
#     path, so they must name root too and leave group and mode alone.
#
# Structural only (awk/grep over the real role files) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="${ROLE:-configs/ansible/roles/archival-node}"
DIR=/var/lib/node_exporter/textfile_collector
WANT="root:stellarindex:1775"

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

for f in configs/ansible/roles/{haproxy,patroni,redis-sentinel}/tasks/*monitoring.yml; do
  got="$(awk '/textfile_dir }}"/ { hit = 1; next }
    hit && /^- name:/ { exit }
    hit && /^[ \t]*(owner|group|mode):/ { gsub(/[ \t"]/, ""); printf "%s ", $0 }' "$f")"
  if [ "$got" = "owner:root " ]; then
    ok "$f: textfile dir is root's, group and mode left alone"
  else
    bad "$f: textfile dir declares '$got' — want only owner: root"
  fi
done

# No root unit may write a .prom through a tool that follows a link at the target.
if grep -rnE '\| *sponge ' configs/ansible/roles deploy/systemd; then
  bad "a collector pipes into sponge, which follows a symlink at its target — pipe into /usr/local/bin/textfile-publish"
else
  ok "no collector pipes into sponge"
fi

# The publisher replaces a link planted at the target instead of writing
# through it. BSD mv has no -T, so this arm runs where the hosts' mv does.
T="$(mktemp -d)"
if mv -T "$T" "$T.r" 2>/dev/null; then
  T="$T.r"
  echo victim > "$T/victim"
  ln -s "$T/victim" "$T/apt.prom"
  echo 'm 1' | TEXTFILE_DIR="$T" sh "$ROLE/files/textfile-publish.sh" apt
  if [ "$(cat "$T/victim")" = victim ] && [ ! -L "$T/apt.prom" ] && [ "$(cat "$T/apt.prom")" = 'm 1' ]; then
    ok "textfile-publish replaces a planted link and leaves its target alone"
  else
    bad "textfile-publish wrote through a link planted at the target"
  fi
else
  echo "  skip — mv has no -T here; the publisher arm runs on Linux"
fi
rm -rf "$T"

# Every unit that joins group stellarindex is a group writer the 0775 mode
# exists for. Under ProtectSystem=strict it also needs the dir in
# ReadWritePaths, or its write fails EROFS despite the group bit.
UNIT_DIRS="$ROLE/templates/systemd configs/healthchecks"
writers="$(grep -lE '^SupplementaryGroups=(.* )?stellarindex( |$)' \
  "$ROLE"/templates/systemd/*.service* configs/healthchecks/*.service 2>/dev/null)"
if [ -z "$writers" ]; then
  bad "no unit under $UNIT_DIRS joins group stellarindex — the 1775 mode has no group writer left; re-derive it in both declaring tasks together"
fi
strict_without_rw() {
  grep -qE '^ProtectSystem=strict$' "$1" &&
    ! grep -qE "^ReadWritePaths=(.* )?$DIR/?( |$)" "$1"
}
while IFS= read -r unit; do
  [ -n "$unit" ] || continue
  if strict_without_rw "$unit"; then
    bad "$unit: SupplementaryGroups=stellarindex under ProtectSystem=strict without ReadWritePaths=$DIR"
  else
    ok "group writer (needs 1775): $unit"
  fi
done <<<"$writers"

# An ops subcommand built on opsutil.JobHeartbeat writes its .prom into $DIR
# by default. Under ProtectSystem=strict the dir still stats, so the job
# believes it publishes while every write fails EROFS and the series is never born.
hb_jobs="$(grep -rhoE 'NewJobHeartbeat\("[a-z0-9-]+",' internal |
  sed -E 's/.*\("([a-z0-9-]+)",/\1/' | sort -u)"
if [ -z "$hb_jobs" ]; then
  bad "no literal opsutil.NewJobHeartbeat(\"<job>\", …) call under internal/ — re-derive the heartbeat publisher set"
fi
while IFS= read -r job; do
  [ -n "$job" ] || continue
  for unit in "$ROLE"/templates/systemd/*.service*; do
    grep -qE "^[^#]*stellarindex-ops $job( |\\\\|$)" "$unit" || continue
    if strict_without_rw "$unit"; then
      bad "$unit: runs heartbeat publisher '$job' under ProtectSystem=strict without ReadWritePaths=$DIR"
    else
      ok "heartbeat publisher '$job' can write $DIR: $unit"
    fi
  done
done <<<"$hb_jobs"

echo "ansible-textfile-dir-mode-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
