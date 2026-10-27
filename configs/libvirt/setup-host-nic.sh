#!/usr/bin/env bash
#
# setup-host-nic.sh — make the KVM host survive its e1000e NIC wedging.
# 1. Persist the standard workaround (TSO/GSO off) as a post-up in the
#    installimage-written /etc/network/interfaces.
# 2. Install host-nic-watchdog.sh on a 1-minute timer: bounce the NIC, then
#    reboot, when the default gateway stops answering.
#
# Idempotent. Run ON THE HOST (95.217.126.13) from this directory:
#   rsync -a configs/libvirt/ root@95.217.126.13:/root/libvirt/
#   ssh root@95.217.126.13 'cd /root/libvirt && ./setup-host-nic.sh'
set -euo pipefail

IFACE="${IFACE:-eno1}"
DIR="$(cd "$(dirname "$0")" && pwd)"
POST_UP="  post-up /usr/sbin/ethtool -K $IFACE tso off gso off"

if ! grep -qF "ethtool -K $IFACE tso off gso off" /etc/network/interfaces; then
	echo "== adding the offload post-up to the $IFACE inet stanza =="
	cp -a /etc/network/interfaces "/etc/network/interfaces.bak-$(date +%Y%m%d%H%M%S)"
	sed -i "/^iface $IFACE inet static/a\\$POST_UP" /etc/network/interfaces
	grep -qF "ethtool -K $IFACE tso off gso off" /etc/network/interfaces \
		|| { echo "no 'iface $IFACE inet static' stanza in /etc/network/interfaces" >&2; exit 1; }
fi
/usr/sbin/ethtool -K "$IFACE" tso off gso off

install -m 0755 "$DIR/host-nic-watchdog.sh" /usr/local/sbin/si-nic-watchdog

cat > /etc/systemd/system/si-nic-watchdog.service <<UNIT
[Unit]
Description=Bounce or reboot when $IFACE stops reaching the default gateway (e1000e hang)

[Service]
Type=oneshot
Environment=IFACE=$IFACE
ExecStart=/usr/local/sbin/si-nic-watchdog
UNIT

cat > /etc/systemd/system/si-nic-watchdog.timer <<'UNIT'
[Unit]
Description=Run si-nic-watchdog every minute

[Timer]
OnBootSec=5min
OnUnitActiveSec=1min
AccuracySec=10s

[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload
systemctl enable --now si-nic-watchdog.timer
echo "== offloads =="
/usr/sbin/ethtool -k "$IFACE" | grep -E '^(tcp|generic)-segmentation-offload:'
systemctl list-timers si-nic-watchdog.timer --no-pager
