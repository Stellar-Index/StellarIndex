#!/usr/bin/env bash
# ansible-zfs-arc-cap-test.sh — pins the archival-node ZFS ARC cap.
#   - 03-zfs.yml (the ZFS-host-gated task file) renders zfs-modprobe.conf.j2
#     to /etc/modprobe.d/zfs.conf, so a rebuilt host gets the cap.
#   - Rendered with the role defaults, the file is byte-identical to r1's
#     live copy; any drift there makes the first apply rewrite it.
#   - No task writes the live /sys/module/zfs parameter: shrinking ARC on a
#     running host is an operator decision, not a side effect of an apply.
#
# Renders the real template with a local ansible-playbook run; no hosts.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="${ROLE:-$PWD/configs/ansible/roles/archival-node}"
WANT="$(printf 'options zfs zfs_arc_max=34359738368\noptions zfs zfs_arc_min=8589934592\nX')"
WANT="${WANT%X}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-zfs-arc-cap-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi

task="$(awk '
  /^- name:/ { grab = ($0 ~ /^- name: Cap the ZFS ARC/) }
  grab && !/^[ \t]*#/ { print }' "$ROLE/tasks/03-zfs.yml")"
if grep -q 'src: zfs-modprobe.conf.j2' <<<"$task" && grep -q 'dest: /etc/modprobe.d/zfs.conf' <<<"$task"; then
  ok "03-zfs.yml renders zfs-modprobe.conf.j2 to /etc/modprobe.d/zfs.conf"
else
  bad "03-zfs.yml has no 'Cap the ZFS ARC' task rendering zfs-modprobe.conf.j2 to /etc/modprobe.d/zfs.conf"
fi

live="$(grep -hv '^[[:space:]]*#' "$ROLE"/tasks/*.yml)"
if grep -q '/sys/module/zfs/parameters' <<<"$live"; then
  bad "a task writes /sys/module/zfs/parameters — the cap must only take effect at module load"
else
  ok "no task writes the live ARC parameter"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/fixture.yml" <<YML
---
- hosts: localhost
  connection: local
  gather_facts: false
  vars_files:
    - "$ROLE/defaults/main.yml"
  tasks:
    - name: render the role template
      ansible.builtin.template:
        src: "$ROLE/templates/zfs-modprobe.conf.j2"
        dest: "$TMP/zfs.conf"
        mode: "0644"
YML
if ! out="$(cd "$TMP" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ansible-playbook -i localhost, fixture.yml 2>&1)"; then
  bad "rendering the template failed: $(tail -5 <<<"$out")"
elif [ "$(cat "$TMP/zfs.conf"; printf X)" = "${WANT}X" ]; then
  ok "rendered file is byte-identical to r1's /etc/modprobe.d/zfs.conf"
else
  bad "rendered file differs from r1's: $(od -c "$TMP/zfs.conf")"
fi

echo "ansible-zfs-arc-cap-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
