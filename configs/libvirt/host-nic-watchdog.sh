#!/usr/bin/env bash
#
# host-nic-watchdog.sh — run every minute by si-nic-watchdog.timer on the KVM
# host. The onboard e1000e NIC can wedge ("Detected Hardware Unit Hang") and
# take the box off the network while it keeps running. Escalation: bounce the
# interface (ifup re-applies the offload workaround) after FAIL_BOUNCE
# consecutive failed minutes; reboot after FAIL_REBOOT.
#
# DRY_RUN=1 logs the action instead of taking it.
set -uo pipefail

IFACE="${IFACE:-eno1}"
TARGET="${TARGET:-$(ip -4 route show default dev "$IFACE" | awk 'NR == 1 {print $3}')}"
STATE="${STATE:-/run/si-nic-watchdog.fails}"
REBOOT_MARK="${REBOOT_MARK:-/var/lib/si-nic-watchdog.last-reboot}"
FAIL_BOUNCE="${FAIL_BOUNCE:-3}"
FAIL_REBOOT="${FAIL_REBOOT:-10}"
# A box that is dark from boot would otherwise reboot every FAIL_REBOOT minutes.
REBOOT_MIN_GAP="${REBOOT_MIN_GAP:-3600}"

act() {
	echo "si-nic-watchdog: $*"
	[[ "${DRY_RUN:-0}" == 1 ]] || "$@"
}

if [[ -n "$TARGET" ]] && ping -c 3 -W 2 -I "$IFACE" "$TARGET" >/dev/null 2>&1; then
	rm -f "$STATE"
	exit 0
fi

fails=$(( $(cat "$STATE" 2>/dev/null || echo 0) + 1 ))
echo "$fails" > "$STATE"
echo "si-nic-watchdog: ${TARGET:-default gateway} unreachable via $IFACE for $fails consecutive check(s)"

if (( fails >= FAIL_REBOOT )); then
	now=$(date +%s)
	last=$(cat "$REBOOT_MARK" 2>/dev/null || echo 0)
	if (( now - last < REBOOT_MIN_GAP )); then
		echo "si-nic-watchdog: last watchdog reboot $(( now - last ))s ago (< ${REBOOT_MIN_GAP}s), not rebooting again"
		exit 1
	fi
	[[ "${DRY_RUN:-0}" == 1 ]] || echo "$now" > "$REBOOT_MARK"
	act systemctl reboot
elif (( fails % FAIL_BOUNCE == 0 )); then
	act ifdown --force "$IFACE"
	act ifup "$IFACE"
fi
exit 1
