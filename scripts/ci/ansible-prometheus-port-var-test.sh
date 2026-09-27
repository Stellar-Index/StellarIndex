#!/usr/bin/env bash
# ansible-prometheus-port-var-test.sh — pins SL19: prometheus.yml.j2
# (self-scrape + alertmanager targets), prometheus.service.j2
# (--web.external-url) and 06-firewall.yml (nftables dport set) each
# hardcoded literal 9090/9093 instead of referencing
# prometheus_port/alertmanager_port from defaults/main.yml. Operator
# overriding those vars (e.g. to dodge a port conflict) silently left
# the scrape config, the external URL and the firewall pointed at the
# OLD port while prometheus_listen/alertmanager_listen moved to the
# new one.
#
# Structural only (grep over the real files) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="configs/ansible/roles/prometheus"
DEFAULTS="$ROLE/defaults/main.yml"
PROM_YML="$ROLE/templates/prometheus.yml.j2"
PROM_SVC="$ROLE/templates/prometheus.service.j2"
FIREWALL="$ROLE/tasks/06-firewall.yml"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

for f in "$DEFAULTS" "$PROM_YML" "$PROM_SVC" "$FIREWALL"; do
  if [ ! -f "$f" ]; then
    echo "ansible-prometheus-port-var-test: FAIL — $f not found" >&2
    exit 1
  fi
done

if grep -qE '^prometheus_port:\s*9090\s*$' "$DEFAULTS" \
  && grep -qE '^alertmanager_port:\s*9093\s*$' "$DEFAULTS"; then
  ok "defaults: prometheus_port/alertmanager_port declared"
else
  bad "defaults: prometheus_port/alertmanager_port not declared as the single source of truth"
fi

if grep -qE 'prometheus_listen:.*\{\{ *prometheus_port *\}\}' "$DEFAULTS" \
  && grep -qE 'alertmanager_listen:.*\{\{ *alertmanager_port *\}\}' "$DEFAULTS"; then
  ok "defaults: prometheus_listen/alertmanager_listen derive from the port vars"
else
  bad "defaults: prometheus_listen/alertmanager_listen do not reference prometheus_port/alertmanager_port"
fi

# No bare literal :9090"/:9093" left in the scrape-target lines.
if grep -qE ':(9090|9093)"' "$PROM_YML"; then
  bad "$PROM_YML: literal :9090\"/:9093\" target — use {{ prometheus_port }}/{{ alertmanager_port }}"
else
  ok "$PROM_YML: no hardcoded scrape-target ports"
fi
if grep -q '{{ prometheus_port }}' "$PROM_YML" && grep -q '{{ alertmanager_port }}' "$PROM_YML"; then
  ok "$PROM_YML: scrape targets reference prometheus_port/alertmanager_port"
else
  bad "$PROM_YML: missing {{ prometheus_port }}/{{ alertmanager_port }} references"
fi

if grep -qE 'external-url=http://\{\{ inventory_hostname \}\}:9090/' "$PROM_SVC"; then
  bad "$PROM_SVC: --web.external-url hardcodes :9090"
elif grep -q '{{ prometheus_port }}' "$PROM_SVC"; then
  ok "$PROM_SVC: --web.external-url references prometheus_port"
else
  bad "$PROM_SVC: --web.external-url does not reference prometheus_port"
fi

if grep -qE 'dport \{ 9090, 9093,' "$FIREWALL"; then
  bad "$FIREWALL: nftables dport set hardcodes 9090, 9093"
elif grep -q '{{ prometheus_port }}' "$FIREWALL" && grep -q '{{ alertmanager_port }}' "$FIREWALL"; then
  ok "$FIREWALL: nftables dport set references prometheus_port/alertmanager_port"
else
  bad "$FIREWALL: nftables dport set does not reference prometheus_port/alertmanager_port"
fi

echo "ansible-prometheus-port-var-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
