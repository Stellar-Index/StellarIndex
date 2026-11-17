#!/usr/bin/env bash
# prometheus-ha-render-test.sh — renders prometheus.yml.j2 for an HA pair and
# checks what Alertmanager, the peers and the unit flags depend on:
#   - the per-replica external label is dropped from outgoing alerts, and is
#     not named `instance` (which would clash with the target label);
#   - with basic-auth hashes set, the alertmanager and self-scrape jobs carry
#     a client basic_auth (password_file), and preflight refuses a hash that
#     has no plaintext;
#   - a Loki scrape job exists for log_aggregator hosts;
#   - the unit template notifies a restarting handler, not SIGHUP.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="configs/ansible/roles/prometheus"

if ! command -v ansible-playbook >/dev/null 2>&1; then
  if [ "${CI:-}" = "true" ]; then
    echo "prometheus-ha-render-test: FAIL — ansible-playbook not found (required in CI)" >&2
    exit 1
  fi
  echo "prometheus-ha-render-test: SKIP (no ansible-playbook locally; CI enforces)"
  exit 0
fi

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

cat >"$TMPD/inventory.yml" <<'EOF'
all:
  children:
    prometheus_pair:
      hosts:
        prom1: {ansible_host: 10.0.0.5, ansible_connection: local}
        prom2: {ansible_host: 10.0.0.6, ansible_connection: local}
    stellarindex_api:
      hosts:
        api1: {ansible_host: 10.0.1.5, ansible_connection: local}
    log_aggregator:
      hosts:
        log1: {ansible_host: 10.0.2.5, ansible_connection: local}
EOF

cat >"$TMPD/play.yml" <<EOF
- hosts: prom1
  gather_facts: no
  vars:
    prometheus_default_scrape_interval: 15s
    prometheus_default_evaluation_interval: 15s
    prometheus_aggregator_scrape_interval: 30s
    prometheus_indexer_scrape_interval: 30s
    prometheus_node_scrape_interval: 30s
    prometheus_external_labels: {region: r1, cluster: stellarindex}
    prometheus_port: 9090
    alertmanager_port: 9093
    prometheus_basic_auth_username: operator
    alertmanager_basic_auth_username: operator
    prometheus_basic_auth_password_hash: "{{ prom_hash }}"
    alertmanager_basic_auth_password_hash: "{{ am_hash }}"
  tasks:
    - template:
        src: $PWD/$ROLE/templates/prometheus.yml.j2
        dest: "{{ out }}"
EOF

render() { # <prom_hash> <am_hash> <out>
  ansible-playbook -i "$TMPD/inventory.yml" "$TMPD/play.yml" \
    -e "prom_hash='$1' am_hash='$2' out=$3" >"$TMPD/playbook.log" 2>&1 || {
    echo "prometheus-ha-render-test: FAIL — render failed" >&2
    cat "$TMPD/playbook.log" >&2
    exit 1
  }
}

failed=0
ok() { echo "  ok — $1"; }
bad() { echo "  FAIL — $1" >&2; failed=1; }

# gitleaks: fake bcrypt-shaped fixture value, not a real credential.
# shellcheck disable=SC2016
HASH='$2y$10$fixturefixturefixturefixture' # gitleaks:allow

render "" "" "$TMPD/off.yml"
render "$HASH" "$HASH" "$TMPD/on.yml"

for f in off on; do
  if promtool check config --syntax-only "$TMPD/$f.yml" >"$TMPD/promtool-$f.log" 2>&1; then
    ok "$f render passes promtool check config --syntax-only"
  else
    bad "$f render fails promtool"; cat "$TMPD/promtool-$f.log" >&2
  fi
done

if grep -qE '^    instance:' "$TMPD/off.yml"; then
  bad "external label is named instance (clashes with the target label)"
else
  ok "external label is not named instance"
fi
relabel=$(grep -A3 'alert_relabel_configs' "$TMPD/off.yml" || true)
if grep -q '^    replica: prom1$' "$TMPD/off.yml" \
  && [[ "$relabel" == *'action: labeldrop'* && "$relabel" == *'regex: replica'* ]]; then
  ok "replica external label is dropped from outgoing alerts"
else
  bad "the per-replica external label is not dropped by alert_relabel_configs"
fi

if grep -q 'basic_auth' "$TMPD/off.yml"; then
  bad "basic_auth rendered with the hashes unset"
else
  ok "no client basic_auth when the hashes are unset"
fi
if [ "$(grep -c 'password_file: /etc/prometheus/' "$TMPD/on.yml")" -eq 3 ]; then
  ok "alerting, prometheus_pair and alertmanager_pair carry a client password_file"
else
  bad "expected 3 client password_file entries with both hashes set"
fi

if grep -q 'job_name: "loki"' "$TMPD/on.yml" && grep -q '10.0.2.5:3100' "$TMPD/on.yml"; then
  ok "loki scrape job targets the log_aggregator host"
else
  bad "no loki scrape job for log_aggregator"
fi

handler=$(grep -A5 'name: restart prometheus' "$ROLE/handlers/main.yml" || true)
if grep -q 'notify: "prometheus unit changed"' "$ROLE/tasks/05-systemd.yml" \
  && [[ "$handler" == *'state: restarted'* ]]; then
  ok "prometheus unit change restarts the service"
else
  bad "prometheus unit change is applied by SIGHUP only"
fi

if grep -q 'prometheus_basic_auth_password ' "$ROLE/tasks/01-preflight.yml"; then
  ok "preflight requires the plaintext whenever a hash is set"
else
  bad "preflight does not tie the hash to a plaintext client password"
fi

[ "$failed" -eq 0 ] || exit 1
echo "prometheus-ha-render-test: PASS"
