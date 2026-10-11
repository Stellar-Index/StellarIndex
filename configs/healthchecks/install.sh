#!/usr/bin/env bash
# Install per-binary Healthchecks.io heartbeats on R1.
#
# Bare-bones manual installer for ad-hoc bring-up before Ansible inventory
# exists. Production should use the idempotent, drift-tracking role task
# `configs/ansible/roles/archival-node/tasks/17-stellarindex-healthchecks.yml`
# (manual re-runs let deployed wrappers/units lag the repo).
#
# Idempotent: re-running re-syncs the script + units. The
# /etc/default/stellarindex-healthchecks env file is created with placeholders
# on first run (operator fills in URLs) and preserved afterwards.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." &>/dev/null && pwd)"

INSTALL_DIR="/opt/stellarindex/healthchecks"
SYSTEMD_DIR="/etc/systemd/system"
ENV_FILE="/etc/default/stellarindex-healthchecks"

mkdir -p "$INSTALL_DIR"
# Sourced by all three wrappers, so installed first.
install -m 0644 "$SCRIPT_DIR/hc-ping.sh" "$INSTALL_DIR/hc-ping.sh"
install -m 0755 "$SCRIPT_DIR/heartbeat.sh" "$INSTALL_DIR/heartbeat.sh"
install -m 0755 "$SCRIPT_DIR/smoke.sh" "$INSTALL_DIR/smoke.sh"
install -m 0755 "$SCRIPT_DIR/sla-probe.sh" "$INSTALL_DIR/sla-probe.sh"
# Copied alongside so the smoke unit needs no checkout on R1.
install -m 0755 "$REPO_ROOT/scripts/dev/r1-smoke.sh" "$INSTALL_DIR/r1-smoke.sh"
install -m 0644 "$SCRIPT_DIR/stellarindex-heartbeat@.service" "$SYSTEMD_DIR/"
install -m 0644 "$SCRIPT_DIR/stellarindex-heartbeat@.timer" "$SYSTEMD_DIR/"
install -m 0644 "$SCRIPT_DIR/stellarindex-smoke.service" "$SYSTEMD_DIR/"
install -m 0644 "$SCRIPT_DIR/stellarindex-smoke.timer" "$SYSTEMD_DIR/"
install -m 0644 "$SCRIPT_DIR/stellarindex-sla-probe.service" "$SYSTEMD_DIR/"
install -m 0644 "$SCRIPT_DIR/stellarindex-sla-probe.timer" "$SYSTEMD_DIR/"

# Placeholder env file if missing. Operator pastes the five Healthchecks.io URLs
# (3 heartbeats + smoke + SLA probe), then restarts the timers (see end of script).
if [ ! -f "$ENV_FILE" ]; then
  cat > "$ENV_FILE" <<'EOF'
# Healthchecks.io URLs.
#
# Each is a separate "Check" on healthchecks.io. Empty URL silently
# skips the ping (the underlying probe still runs and logs failures
# via journalctl, so the timer is useful even before URLs are wired).
#
# Per-binary heartbeats (60 s cadence, suggested grace 120 s):
HEALTHCHECKS_URL_INDEXER=
HEALTHCHECKS_URL_AGGREGATOR=
HEALTHCHECKS_URL_API=
# API surface smoke test (5 min cadence, suggested grace 10 min):
HEALTHCHECKS_URL_SMOKE=
# SLA probe (latency + freshness; 15 min cadence, grace 30 min):
HEALTHCHECKS_URL_SLA_PROBE=
# SLA probe tuning (defaults match the binary's flag defaults).
# SLA_PROBE_BASE_URL=http://localhost:3000/v1
# SLA_PROBE_DURATION=30s
# SLA_PROBE_CONCURRENCY=1
# SLA_PROBE_PAIR=native,fiat:USD
EOF
  chmod 0600 "$ENV_FILE"
  chown root:root "$ENV_FILE"
  echo "install: created placeholder $ENV_FILE — operator to populate"
fi

systemctl daemon-reload
systemctl enable --now stellarindex-heartbeat@indexer.timer
systemctl enable --now stellarindex-heartbeat@aggregator.timer
systemctl enable --now stellarindex-heartbeat@api.timer
systemctl enable --now stellarindex-smoke.timer
systemctl enable --now stellarindex-sla-probe.timer

echo "install: done"
echo
echo "Next: populate $ENV_FILE with real URLs from healthchecks.io,"
echo "then 'systemctl restart stellarindex-heartbeat@*.timer stellarindex-smoke.timer stellarindex-sla-probe.timer'"
