#!/bin/bash
# Provision the Testnet + Futurenet VMs on the libvirt/KVM host.
#
# Run ON the host (Debian 12, as root) AFTER installimage (see
# installimage-host.conf) and README.md. Idempotent: re-running only
# refreshes the cloud-init seeds; it skips the base-image import for any
# logical volume that already exists, so it never overwrites a VM's live
# disk. Delete a VM first (virsh destroy + undefine + lvremove) to force a
# clean rebuild of its volume.
#
# The VMs run Ubuntu 24.04 (matching r1) so the archival-node ansible role
# deploys into them unchanged. Each gets a static IP on libvirt's default NAT
# network; the host reverse-proxies the public subdomains to them (see the
# host Caddyfile / README). Host owns the ZFS/RAID; the VM uses its virtual
# disk directly (no in-VM ZFS).
set -euo pipefail

# SSH pubkeys to authorize for root in BOTH VMs — one per line.
KEYS_FILE="${KEYS_FILE:-/root/vm_authorized_keys}"
[ -f "$KEYS_FILE" ] || { echo "ERROR: put the VM SSH pubkeys (one per line) in $KEYS_FILE"; exit 1; }

# Overridable for the self-test (provision-vms-test.sh); production callers
# should leave these at their libvirt/LVM defaults.
IMAGES_DIR="${IMAGES_DIR:-/var/lib/libvirt/images}"
VG_DEVICE_DIR="${VG_DEVICE_DIR:-/dev/vg0}"

echo "=== 1. install libvirt/KVM ==="
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients virtinst \
  bridge-utils cloud-image-utils genisoimage dnsmasq-base
systemctl enable --now libvirtd
virsh net-start default    2>/dev/null || true
virsh net-autostart default 2>/dev/null || true

echo "=== 2. Ubuntu 24.04 cloud image ==="
mkdir -p "$IMAGES_DIR" && cd "$IMAGES_DIR"
UBUNTU_IMG_NAME=ubuntu-24.04-server-cloudimg-amd64.img
UBUNTU_IMG_URL="https://cloud-images.ubuntu.com/releases/24.04/release/${UBUNTU_IMG_NAME}"
if [ ! -f ubuntu-24.04-base.img ]; then
  wget -q "$UBUNTU_IMG_URL" -O ubuntu-24.04-base.img
  wget -q "https://cloud-images.ubuntu.com/releases/24.04/release/SHA256SUMS" -O SHA256SUMS
  expected_sum=$(awk -v f="$UBUNTU_IMG_NAME" 'index($0, f) { print $1; exit }' SHA256SUMS)
  [ -n "$expected_sum" ] || { echo "ERROR: no checksum for $UBUNTU_IMG_NAME in SHA256SUMS"; rm -f ubuntu-24.04-base.img; exit 1; }
  actual_sum=$(sha256sum ubuntu-24.04-base.img | awk '{print $1}')
  if [ "$actual_sum" != "$expected_sum" ]; then
    echo "ERROR: checksum mismatch for ubuntu-24.04-base.img (got $actual_sum, want $expected_sum)"
    rm -f ubuntu-24.04-base.img
    exit 1
  fi
  rm -f SHA256SUMS
fi

create_vm() {
  local name=$1 ip=$2 ram=$3 vcpu=$4 lv=$5
  echo "=== create VM $name ($ip, ${ram}MB, ${vcpu} vCPU, ${VG_DEVICE_DIR}/$lv) ==="
  local lv_path="${VG_DEVICE_DIR}/${lv}"
  if [ -e "$lv_path" ]; then
    echo "  -> $lv_path already exists, skipping image import (destroy+undefine+lvremove to rebuild)"
  else
    lvcreate -y -L 600G -n "$lv" vg0
    qemu-img convert -O raw ubuntu-24.04-base.img "$lv_path"
  fi
  {
    echo "#cloud-config"
    echo "hostname: $name"
    echo "fqdn: $name.stellarindex.io"
    echo "manage_etc_hosts: true"
    echo "disable_root: false"
    echo "ssh_pwauth: false"
    echo "users:"
    echo "  - name: root"
    echo "    ssh_authorized_keys:"
    sed 's/^/      - /' "$KEYS_FILE"
    echo "runcmd:"
    printf '%s\n' "  - printf 'PermitRootLogin prohibit-password\\n' > /etc/ssh/sshd_config.d/00-root.conf"
    echo "  - systemctl restart ssh"
  } > "/tmp/${name}-user-data"
  cat > "/tmp/${name}-net.yaml" <<NET
version: 2
ethernets:
  id0:
    match: { name: "en*" }
    dhcp4: false
    addresses: [${ip}/24]
    routes: [{ to: default, via: 192.168.122.1 }]
    nameservers: { addresses: [1.1.1.1, 8.8.8.8] }
NET
  cloud-localds --network-config="/tmp/${name}-net.yaml" \
    "${IMAGES_DIR}/${name}-seed.iso" "/tmp/${name}-user-data"
  virt-install --name "$name" --memory "$ram" --vcpus "$vcpu" --cpu host-passthrough \
    --disk "path=${lv_path},format=raw,bus=virtio,cache=none" \
    --disk "path=${IMAGES_DIR}/${name}-seed.iso,device=cdrom" \
    --os-variant ubuntu22.04 --network network=default,model=virtio \
    --graphics none --noautoconsole --import
  virsh autostart "$name"
}

#          name           ip              ram    vcpu  lv
create_vm  si-testnet     192.168.122.10  20480  4     testnet
create_vm  si-futurenet   192.168.122.20  20480  4     futurenet

echo "=== VMs ==="
virsh list --all
