#!/usr/bin/env bash
# ansible-zfs-import-guard-test.sh — pins the 03-zfs.yml disaster-recovery
# guard.
#
# WHAT WENT WRONG. The only thing stopping `zpool create -f` from
# re-creating the `data` pool was `zpool list -H data` exiting 0 — which
# is true only while the pool is IMPORTED. A pool that exists on disk but
# is unimported (OS mirror reinstalled with the data drives left intact;
# zfs-import-cache failed at boot and an operator re-applies the role
# per infra.md#stellarindex_node_root_disk_warning) is invisible to `zpool list`, and `-f`
# overrides zpool's own exported/potentially-active refusal. There was no
# `zpool import` probe anywhere in the role.
#
# HOW IT TESTS. A real `ansible-playbook -c local` run of the "check pool
# exists / scan for importable pool / fail if collision" task range
# extracted verbatim from the role's OWN 03-zfs.yml (not a transcription),
# against a stub `zpool` binary on PATH whose `list` always reports
# not-imported (rc=1) and whose `import -d ...` output is the case's:
#
#   A. import scan reports the target pool importable, no ack  → play
#      FAILS before create (this is the arm that is RED before the fix —
#      on the unfixed file the range has no scan/assert task at all, so
#      the play trivially succeeds and never refuses);
#   B. same, with zfs_data_pool_recreate_ack=true                → succeeds;
#   C. import scan reports no pools available                    → succeeds;
#   D/E. `zdb -l` reads a label on a device or its first partition → fails;
#   F. the import scan exits non-zero                            → fails;
#   G. label found, ack given                                    → succeeds.
#
# Needs ansible-playbook. Run: bash scripts/ci/ansible-zfs-import-guard-test.sh

set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE_TASKS_FILE="${ROLE_TASKS_FILE:-$PWD/configs/ansible/roles/archival-node/tasks/03-zfs.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-zfs-import-guard-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi

# Extract the task range from "Check if pool" (inclusive) through the task
# immediately before "Create ZFS pool" (exclusive) — the pool-existence
# check plus whatever guards sit between it and the destructive create.
extract_range() {
  awk '
    /^- name: Check if pool/ { grab = 1 }
    /^- name: Create ZFS pool/ { grab = 0 }
    grab { print }
  ' "$1"
}

RANGE="$(extract_range "$ROLE_TASKS_FILE")"
if [ -z "$RANGE" ]; then
  bad "could not extract the pool-check task range from $ROLE_TASKS_FILE"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

# Stub zpool: `list -H <name>` always reports not-imported (rc=1, the
# pre-boot / post-reinstall shape this finding is about). `import -d ...`
# prints whatever this case put in $TMP/import-output.
cat > "$TMP/bin/zpool" <<'SH'
#!/bin/sh
case "$1" in
  list) exit 1 ;;
  import) cat "ZFS_TEST_TMP/import-output"; exit "$(cat ZFS_TEST_TMP/import-rc)" ;;
  *) exit 0 ;;
esac
SH
# Stub zdb: `-l <dev>` exits 0 (label read) for the device named in
# $TMP/labelled, 1 (could not open) for the one in $TMP/unreadable, else 2
# (no valid label).
cat > "$TMP/bin/zdb" <<'SH'
#!/bin/sh
[ "$2" = "$(cat ZFS_TEST_TMP/labelled)" ] && exit 0
[ "$2" = "$(cat ZFS_TEST_TMP/unreadable)" ] && exit 1
exit 2
SH
# Stub lsblk: `-nrpo NAME <dev>` prints the kernel name of the device and of
# each partition, as the real one does for a by-id link or a kernel path.
cat > "$TMP/bin/lsblk" <<'SH'
#!/bin/sh
[ "$3" = "$(cat ZFS_TEST_TMP/unlistable)" ] && exit 32
case "$3" in
  /dev/disk/by-id/a-part5) echo /dev/sda5 ;;
  /dev/disk/by-id/b) echo /dev/sdb; echo /dev/sdb1; echo /dev/sdb9 ;;
  /dev/nvme0n1) echo /dev/nvme0n1; echo /dev/nvme0n1p1; echo /dev/nvme0n1p9 ;;
  /dev/nvme1n1) echo /dev/nvme1n1; echo /dev/nvme1n1p1; echo /dev/nvme1n1p9 ;;
  *) exit 32 ;;
esac
SH
for b in zpool zdb lsblk; do
  sed -i.bak "s#ZFS_TEST_TMP#$TMP#" "$TMP/bin/$b" && rm -f "$TMP/bin/$b.bak"
  chmod +x "$TMP/bin/$b"
done

echo "$RANGE" > "$TMP/range.yml"
cat > "$TMP/fixture.yml" <<YML
---
- hosts: localhost
  connection: local
  gather_facts: false
  vars:
    zfs_data_pool_name: data
    zfs_data_devices: [/dev/disk/by-id/a-part5, /dev/disk/by-id/b]
  tasks:
    - name: run extracted range
      ansible.builtin.include_tasks: "$TMP/range.yml"
YML

run_case() {  # <desc> <import-output> <expect: succeeded|refused> [extra ansible args…]
  local desc="$1" import_output="$2" expect="$3"; shift 3
  printf '%s\n' "$import_output" > "$TMP/import-output"
  printf '%s\n' "${IMPORT_RC:-0}" > "$TMP/import-rc"
  printf '%s\n' "${LABELLED:-none}" > "$TMP/labelled"
  printf '%s\n' "${UNREADABLE:-none}" > "$TMP/unreadable"
  printf '%s\n' "${UNLISTABLE:-none}" > "$TMP/unlistable"
  out="$(cd "$TMP" && PATH="$TMP/bin:$PATH" ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
         ansible-playbook -i localhost, fixture.yml "$@" 2>&1)"
  rc=$?
  got=succeeded
  [ "$rc" -ne 0 ] && got=refused
  if [ "$got" = "$expect" ]; then ok "$desc → $got (expected $expect)"
  else bad "$desc → $got (expected $expect): $(tail -5 <<<"$out")"; fi
}

# A. collision, no ack → must refuse before the destructive create runs
run_case "importable 'data' pool found, no ack" "  pool: data
    id: 1234567890
   state: ONLINE" refused

# B. collision, acknowledged → proceeds
run_case "importable 'data' pool found, ack given" "  pool: data
    id: 1234567890
   state: ONLINE" succeeded -e zfs_data_pool_recreate_ack=true

# C. nothing importable → proceeds
run_case "no importable pools" "no pools available to import" succeeded

# D. a pool under another name: the scan does not list the target, but a
# member device carries a label → must refuse
LABELLED=/dev/sda5 run_case "foreign-named pool, label on a partition vdev" "  pool: tank-old
    id: 99
   state: ONLINE" refused

# E. whole-disk vdev: the label is on its first partition
LABELLED=/dev/sdb1 run_case "label on a whole-disk vdev's first partition" \
  "no pools available to import" refused

# F. the scan itself failed → not evidence of empty disks
IMPORT_RC=1 run_case "import scan errored" "cannot discover pools: permission denied" refused

# G. labelled, acknowledged → proceeds
LABELLED=/dev/sdb1 run_case "label found, ack given" "no pools available to import" \
  succeeded -e zfs_data_pool_recreate_ack=true

KERNEL_DEVICES='{"zfs_data_devices": ["/dev/nvme0n1", "/dev/nvme1n1"]}'

# H. whole disks named by kernel path: the label partition is nvme0n1p1,
# which no `-part1` suffix reaches
LABELLED=/dev/nvme1n1p1 run_case "label on a kernel-named whole disk's partition" \
  "no pools available to import" refused -e "$KERNEL_DEVICES"

# I. kernel-named and clean → proceeds
run_case "kernel-named whole disks, no label" "no pools available to import" \
  succeeded -e "$KERNEL_DEVICES"

# J. zdb could not open a device → not evidence of an empty disk
UNREADABLE=/dev/sdb1 run_case "zdb cannot read a partition" "no pools available to import" refused

# K. a device the kernel cannot list → nothing was probed on it
UNLISTABLE=/dev/disk/by-id/b run_case "lsblk cannot list a device" "no pools available to import" refused

echo
echo "ansible-zfs-import-guard-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
