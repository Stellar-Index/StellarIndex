#!/usr/bin/env bash
# Self-test for provision-vms.sh's idempotency guard and image-checksum
# verification (Q272, T476).
#
# Before the fix, create_vm() ran `lvcreate ... 2>/dev/null || true`
# (swallowing an already-exists error) followed by an unconditional
# `qemu-img convert` onto /dev/vg0/$lv — re-running the script against an
# already-provisioned VM clobbered its block device, live or not. The
# script also wget'd the base image with no checksum/signature check at
# all. Both cases are proven here by pointing the script at fake
# IMAGES_DIR/VG_DEVICE_DIR trees and a stub PATH, never real libvirt/LVM.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PROVISION="${SCRIPT_DIR}/provision-vms.sh"
PASS=0
FAIL=0

ok()  { printf '  ok   — %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  FAIL — %s\n' "$1"; FAIL=$((FAIL + 1)); }

# A minimal PATH of stub binaries standing in for apt/libvirt/LVM tooling
# that isn't available (or safe to run for real) in a test environment.
# lvcreate/qemu-img/virt-install log every invocation so the guard's
# skip-when-present behaviour can be asserted on.
make_stub_bin() {
  local bin="$1" call_log="$2" wget_mode="$3"
  mkdir -p "$bin"

  for noop in apt-get systemctl virsh cloud-localds virt-install; do
    cat > "$bin/$noop" <<'EOS'
#!/bin/sh
exit 0
EOS
    chmod +x "$bin/$noop"
  done

  cat > "$bin/lvcreate" <<EOS
#!/bin/sh
echo "lvcreate \$*" >> "$call_log"
exit 0
EOS
  chmod +x "$bin/lvcreate"

  # convert's destination is the last argument; write recognisable
  # content so a test can tell the volume WAS (over)written.
  cat > "$bin/qemu-img" <<EOS
#!/usr/bin/env bash
echo "qemu-img \$*" >> "$call_log"
if [ "\$1" = "convert" ]; then
  dst="\${@: -1}"
  echo "IMPORTED-FROM-BASE-IMAGE" > "\$dst"
fi
exit 0
EOS
  chmod +x "$bin/qemu-img"

  # Serves a fixed image body + matching SHA256SUMS by default; in
  # "corrupt" mode the image body no longer matches the advertised sum.
  # SHA256SUMS always advertises the checksum of the genuine 32-byte
  # "a" body; "corrupt" mode serves a different image body under that
  # same filename, so the fixed sum no longer matches (a real
  # tampered-in-transit/MITM scenario), not merely an injected fake sum.
  cat > "$bin/wget" <<EOS
#!/bin/sh
# args: -q URL -O dest
url="\$2"; dest="\$4"
case "\$url" in
  *SHA256SUMS)
    echo "3ba3f5f43b92602683c19aee62a20342b084dd5971ddd33808d81a328879a547  ubuntu-24.04-server-cloudimg-amd64.img" > "\$dest"
    ;;
  *)
    if [ "$wget_mode" = "corrupt" ]; then
      echo "TAMPERED-IMAGE-BODY" > "\$dest"
    else
      printf 'a%.0s' \$(seq 1 32) > "\$dest"
    fi
    ;;
esac
exit 0
EOS
  chmod +x "$bin/wget"
}

echo "provision-vms self-test:"

# ---------------------------------------------------------------------
# 1. Idempotency: an already-provisioned volume is never touched again.
# ---------------------------------------------------------------------
work="$(mktemp -d)"
bin="$work/bin"; call_log="$work/calls.log"; touch "$call_log"
make_stub_bin "$bin" "$call_log" "normal"

images_dir="$work/images"; vg_dir="$work/vg0"
mkdir -p "$images_dir" "$vg_dir"
keys_file="$work/keys"; echo "ssh-ed25519 AAAAtest test@example.com" > "$keys_file"

# Pre-seed a matching image so the checksum step (commit 1) doesn't
# interfere with this test's focus — use the same fixed body the stub
# wget would have produced, with a checksum computed for real so the
# verification step (which runs regardless) passes.
printf 'a%.0s' $(seq 1 32) > "$images_dir/ubuntu-24.04-base.img"

# The LV for si-testnet already exists — simulating a running VM's disk.
echo "LIVE-VM-DATA-DO-NOT-TOUCH" > "$vg_dir/testnet"

out="$(PATH="$bin:$PATH" IMAGES_DIR="$images_dir" VG_DEVICE_DIR="$vg_dir" \
       KEYS_FILE="$keys_file" bash "$PROVISION" 2>&1)"
rc=$?

if [ "$rc" -eq 0 ]; then
  ok "script exits 0 with a pre-existing volume present"
else
  bad "script should still succeed; rc=$rc out=${out: -400}"
fi

if [ "$(cat "$vg_dir/testnet")" = "LIVE-VM-DATA-DO-NOT-TOUCH" ]; then
  ok "an already-provisioned volume's contents are left untouched"
else
  bad "existing volume was overwritten: $(cat "$vg_dir/testnet")"
fi

if grep -q 'testnet' "$call_log"; then
  bad "lvcreate/qemu-img were invoked against the already-existing volume"
else
  ok "lvcreate/qemu-img are never invoked for an already-existing volume"
fi

if [ -f "$vg_dir/futurenet" ] && [ "$(cat "$vg_dir/futurenet")" = "IMPORTED-FROM-BASE-IMAGE" ]; then
  ok "a genuinely new volume is still created and imported"
else
  bad "new-volume creation path regressed: $([ -f "$vg_dir/futurenet" ] && cat "$vg_dir/futurenet" || echo missing)"
fi

# ---------------------------------------------------------------------
# 2. Checksum verification: a tampered download is refused, not imported.
# ---------------------------------------------------------------------
work2="$(mktemp -d)"
bin2="$work2/bin"; call_log2="$work2/calls.log"; touch "$call_log2"
make_stub_bin "$bin2" "$call_log2" "corrupt"

images_dir2="$work2/images"; vg_dir2="$work2/vg0"
mkdir -p "$images_dir2" "$vg_dir2"
keys_file2="$work2/keys"; echo "ssh-ed25519 AAAAtest test@example.com" > "$keys_file2"

out2="$(PATH="$bin2:$PATH" IMAGES_DIR="$images_dir2" VG_DEVICE_DIR="$vg_dir2" \
        KEYS_FILE="$keys_file2" bash "$PROVISION" 2>&1)"
rc2=$?

if [ "$rc2" -ne 0 ] && [[ "$out2" == *"checksum mismatch"* ]]; then
  ok "a tampered download is refused with a checksum-mismatch error"
else
  bad "tampered download should be refused; rc=$rc2 out=${out2: -400}"
fi

if [ -f "$images_dir2/ubuntu-24.04-base.img" ]; then
  bad "the unverified image was left on disk after a checksum failure"
else
  ok "the unverified image is removed after a checksum failure"
fi

if [ -s "$call_log2" ]; then
  bad "create_vm ran despite the checksum failure: $(cat "$call_log2")"
else
  ok "create_vm never runs when the base image fails verification"
fi

echo "provision-vms self-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
