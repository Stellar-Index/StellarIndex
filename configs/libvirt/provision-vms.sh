#!/bin/bash
# Provision the Testnet + Futurenet VMs on the libvirt/KVM host.
#
# Run ON the host (Debian 12, as root) AFTER installimage (see
# installimage-host.conf) and README.md. Idempotent-ish: re-running recreates
# the cloud-init seeds and re-imports; delete a VM first (virsh destroy +
# undefine + lvremove) to rebuild it clean.
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

echo "=== 1. install libvirt/KVM ==="
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients virtinst \
  bridge-utils cloud-image-utils genisoimage dnsmasq-base
systemctl enable --now libvirtd
virsh net-start default    2>/dev/null || true
virsh net-autostart default 2>/dev/null || true

echo "=== 2. Ubuntu 24.04 cloud image ==="
mkdir -p /var/lib/libvirt/images && cd /var/lib/libvirt/images
[ -f ubuntu-24.04-base.img ] || \
  wget -q https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img \
       -O ubuntu-24.04-base.img

create_vm() {
  local name=$1 ip=$2 ram=$3 vcpu=$4 lv=$5
  echo "=== create VM $name ($ip, ${ram}MB, ${vcpu} vCPU, /dev/vg0/$lv) ==="
  lvcreate -y -L 600G -n "$lv" vg0 2>/dev/null || true
  qemu-img convert -O raw ubuntu-24.04-base.img /dev/vg0/"$lv"
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
    echo "  - printf 'PermitRootLogin prohibit-password\\n' > /etc/ssh/sshd_config.d/00-root.conf"
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
    "/var/lib/libvirt/images/${name}-seed.iso" "/tmp/${name}-user-data"
  virt-install --name "$name" --memory "$ram" --vcpus "$vcpu" --cpu host-passthrough \
    --disk "path=/dev/vg0/${lv},format=raw,bus=virtio,cache=none" \
    --disk "path=/var/lib/libvirt/images/${name}-seed.iso,device=cdrom" \
    --os-variant ubuntu22.04 --network network=default,model=virtio \
    --graphics none --noautoconsole --import
  virsh autostart "$name"
}

#          name           ip              ram    vcpu  lv
create_vm  si-testnet     192.168.122.10  20480  4     testnet
create_vm  si-futurenet   192.168.122.20  20480  4     futurenet

echo "=== VMs ==="
virsh list --all
