#!/usr/bin/env bash
# prometheus-basic-auth-wiring-test.sh — regression test for T643.
#
# prometheus_listen/alertmanager_listen bind 0.0.0.0 by design (peer
# Prometheuses scrape/deliver cross-host — see defaults/main.yml), and
# the ONLY control on 9090/9093 was the firewall: no basic_auth_users,
# no htpasswd, no reverse proxy anywhere in the role. This renders the
# prometheus/alertmanager systemd unit templates with
# {prometheus,alertmanager}_basic_auth_password_hash SET and asserts
# --web.config.file is wired in (and UNSET, asserting it stays absent —
# no regression on the documented degrade-to-firewall-only default).
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE_DIR="configs/ansible/roles/prometheus"
PROM_SVC="$ROLE_DIR/templates/prometheus.service.j2"
AM_SVC="$ROLE_DIR/templates/alertmanager.service.j2"

for f in "$PROM_SVC" "$AM_SVC"; do
  if [ ! -f "$f" ]; then
    echo "prometheus-basic-auth-wiring-test: FAIL — missing $f" >&2
    exit 1
  fi
done

if ! command -v ansible-playbook >/dev/null 2>&1; then
  if [ "${CI:-}" = "true" ]; then
    echo "prometheus-basic-auth-wiring-test: FAIL — ansible-playbook not found (required in CI)" >&2
    exit 1
  fi
  echo "prometheus-basic-auth-wiring-test: SKIP (no ansible-playbook locally; CI enforces)"
  exit 0
fi

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

cat >"$TMPD/inventory.yml" <<'EOF'
all:
  children:
    prometheus_pair:
      hosts:
        prom1:
          ansible_host: 10.0.0.5
          ansible_connection: local
        prom2:
          ansible_host: 10.0.0.6
          ansible_connection: local
EOF

# gitleaks: fake bcrypt-shaped fixture value, not a real credential.
# shellcheck disable=SC2016 # single-quoted on purpose: no expansion wanted
HASH='$2y$10$fixturefixturefixturefixture'  # gitleaks:allow

cat >"$TMPD/play.yml" <<EOF
- hosts: prom1
  gather_facts: no
  vars:
    prometheus_user: prometheus
    prometheus_group: prometheus
    prometheus_data_dir: /var/lib/prometheus
    prometheus_retention_days: 30
    prometheus_retention_size: "0"
    prometheus_listen: "0.0.0.0:9090"
    alertmanager_user: alertmanager
    alertmanager_group: alertmanager
    alertmanager_listen: "0.0.0.0:9093"
    alertmanager_cluster_listen_port: 9094
    prometheus_basic_auth_password_hash: "{{ prom_hash }}"
    alertmanager_basic_auth_password_hash: "{{ am_hash }}"
  tasks:
    - template:
        src: $PWD/$PROM_SVC
        dest: "{{ prom_out }}"
    - template:
        src: $PWD/$AM_SVC
        dest: "{{ am_out }}"
EOF

run() { # <prom_hash> <am_hash> <prom_out> <am_out>
  ansible-playbook -i "$TMPD/inventory.yml" "$TMPD/play.yml" \
    -e "prom_hash='$1' am_hash='$2' prom_out=$3 am_out=$4" \
    >"$TMPD/playbook.log" 2>&1
}

failed=0
ok() { echo "  ok — $1"; }
bad() {
  echo "  FAIL — $1" >&2
  failed=1
}

if ! run "$HASH" "$HASH" "$TMPD/prom-on.service" "$TMPD/am-on.service"; then
  echo "prometheus-basic-auth-wiring-test: FAIL — render (auth set) failed" >&2
  cat "$TMPD/playbook.log" >&2
  exit 1
fi
if ! run "" "" "$TMPD/prom-off.service" "$TMPD/am-off.service"; then
  echo "prometheus-basic-auth-wiring-test: FAIL — render (auth unset) failed" >&2
  cat "$TMPD/playbook.log" >&2
  exit 1
fi

if grep -q -- '--web.config.file=/etc/prometheus/web-config.yml' "$TMPD/prom-on.service"; then
  ok "prometheus unit wires --web.config.file when the hash is set"
else
  bad "prometheus unit missing --web.config.file when the hash is set"
fi
if grep -q -- '--web.config.file' "$TMPD/prom-off.service"; then
  bad "prometheus unit still has --web.config.file when the hash is EMPTY (should degrade to firewall-only)"
else
  ok "prometheus unit has no --web.config.file when the hash is empty (unchanged default)"
fi

if grep -q -- '--web.config.file=/etc/alertmanager/web-config.yml' "$TMPD/am-on.service"; then
  ok "alertmanager unit wires --web.config.file when the hash is set"
else
  bad "alertmanager unit missing --web.config.file when the hash is set"
fi
if grep -q -- '--web.config.file' "$TMPD/am-off.service"; then
  bad "alertmanager unit still has --web.config.file when the hash is EMPTY"
else
  ok "alertmanager unit has no --web.config.file when the hash is empty (unchanged default)"
fi

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "prometheus-basic-auth-wiring-test: PASS"
