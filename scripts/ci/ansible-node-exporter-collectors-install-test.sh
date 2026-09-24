#!/usr/bin/env bash
# ansible-node-exporter-collectors-install-test.sh — pins T630: the
# archival-node role's apt/nvme systemd drop-ins repoint
# ExecStart=/usr/share/prometheus-node-exporter-collectors/{apt_info.py,
# nvme_metrics.py}, but nothing in the role ever apt-installed the
# `prometheus-node-exporter-collectors` package that ships those scripts.
# On a clean/DR rebuild the drop-ins point at binaries that don't exist,
# so both timers fail silently and the drive-wear/media-error series
# (nvme_percentage_used_ratio, nvme_media_errors_total, ...) never scrape.
#
# Structural only (grep over the real task file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/archival-node/tasks/10-observability.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-node-exporter-collectors-install-test: FAIL — $TASKS not found" >&2
  exit 1
fi

if ! grep -q "prometheus-node-exporter-collectors" "$TASKS"; then
  bad "role no longer references prometheus-node-exporter-collectors — test out of date"
else
  ok "role references prometheus-node-exporter-collectors"
fi

# The drop-ins that assume the package's scripts exist.
if ! grep -qE '/usr/share/prometheus-node-exporter-collectors/(apt_info|nvme_metrics)\.py' "$TASKS"; then
  bad "no drop-in ExecStart references the package's scripts — test out of date"
else
  ok "drop-ins reference the package's apt_info.py/nvme_metrics.py"
fi

# The install task must exist, must name the exact package, and must
# come BEFORE the drop-in tasks that assume its scripts are on disk.
install_hits="$(grep -n '^\s*name:\s*prometheus-node-exporter-collectors\s*$' "$TASKS" || true)"
dropin_hits="$(grep -n '/usr/share/prometheus-node-exporter-collectors/apt_info\.py' "$TASKS" || true)"
install_line="${install_hits%%$'\n'*}"; install_line="${install_line%%:*}"
dropin_line="${dropin_hits%%$'\n'*}"; dropin_line="${dropin_line%%:*}"

if [ -z "$install_line" ]; then
  bad "no apt task installs the prometheus-node-exporter-collectors package"
elif [ -z "$dropin_line" ]; then
  bad "could not locate the apt drop-in line to order against"
elif [ "$install_line" -lt "$dropin_line" ]; then
  ok "prometheus-node-exporter-collectors is apt-installed before the drop-ins that need it"
else
  bad "prometheus-node-exporter-collectors apt task (line $install_line) runs AFTER the drop-in that needs it (line $dropin_line)"
fi

echo "ansible-node-exporter-collectors-install-test: $pass ok, $fail failed"
[ "$fail" -eq 0 ]
