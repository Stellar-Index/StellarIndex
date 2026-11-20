#!/usr/bin/env bash
# prometheus-ha-render-test.sh — renders prometheus.yml.j2 for an HA pair and
# checks what Alertmanager, the peers and the unit flags depend on:
#   - the per-replica external label is dropped from outgoing alerts, and is
#     not named `instance` (which would clash with the target label);
#   - with basic-auth hashes set, the alertmanager and self-scrape jobs carry
#     a client basic_auth (password_file), and preflight refuses a hash that
#     has no plaintext;
#   - the Loki scrape job targets loki_listen_address:loki_http_port, and has
#     no target for a loopback-bound Loki;
#   - the unit template notifies a restarting handler, not SIGHUP, and the
#     restart handlers go one replica at a time, each waiting for /-/ready.
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

# python from ansible's own venv: it always has PyYAML and Jinja.
PY=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')

render() { # <prom_hash> <am_hash> <out> [extra ansible args...]
  local ph="$1" ah="$2" o="$3"
  shift 3
  ansible-playbook -i "$TMPD/inventory.yml" "$TMPD/play.yml" \
    -e "prom_hash='$ph' am_hash='$ah' out=$o" "$@" >"$TMPD/playbook.log" 2>&1 || {
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
render "" "" "$TMPD/loki-routable.yml" -e loki_listen_address=10.9.9.9 -e loki_http_port=3101
render "" "" "$TMPD/loki-wildcard.yml" -e loki_listen_address=0.0.0.0

for f in off on loki-routable loki-wildcard; do
  if promtool check config --syntax-only "$TMPD/$f.yml" >"$TMPD/promtool-$f.log" 2>&1; then
    ok "$f render passes promtool check config --syntax-only"
  else
    bad "$f render fails promtool"; cat "$TMPD/promtool-$f.log" >&2
  fi
done

# yq <file> <python expr over d> -> prints the value; used for structural checks.
yq() { "$PY" -I -c 'import sys, yaml; d = yaml.safe_load(open(sys.argv[1])); print(eval(sys.argv[2]))' "$1" "$2"; }

ext=$(yq "$TMPD/off.yml" "sorted(d['global']['external_labels'])")
if [ "$ext" = "['cluster', 'region', 'replica']" ]; then
  ok "replica is an external label and none is named instance"
else
  bad "external labels are $ext (want cluster, region, replica)"
fi
drop=$(yq "$TMPD/off.yml" "any(r.get('action') == 'labeldrop' and __import__('re').fullmatch(r['regex'], 'replica') is not None and __import__('re').fullmatch(r['regex'], 'region') is None for r in d['alerting']['alert_relabel_configs'])")
if [ "$drop" = "True" ]; then
  ok "alerting.alert_relabel_configs drops exactly the replica label"
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

loki_targets() { yq "$1" "[t for j in d['scrape_configs'] if j['job_name'] == 'loki' for s in j['static_configs'] for t in s['targets']]"; }
if [ "$(loki_targets "$TMPD/loki-routable.yml")" = "['10.9.9.9:3101']" ]; then
  ok "loki job targets loki_listen_address:loki_http_port"
else
  bad "loki job target is $(loki_targets "$TMPD/loki-routable.yml") (want 10.9.9.9:3101)"
fi
if [ "$(loki_targets "$TMPD/loki-wildcard.yml")" = "['10.0.2.5:3100']" ]; then
  ok "a wildcard-bound loki is scraped on ansible_host"
else
  bad "wildcard loki target is $(loki_targets "$TMPD/loki-wildcard.yml") (want 10.0.2.5:3100)"
fi
if [ "$(loki_targets "$TMPD/off.yml")" = "[]" ]; then
  ok "no loki job target for a loopback-bound loki"
else
  bad "loopback-bound loki rendered a target: $(loki_targets "$TMPD/off.yml")"
fi

if grep -q 'notify: "prometheus unit changed"' "$ROLE/tasks/05-systemd.yml"; then
  ok "prometheus unit change notifies the restart handler"
else
  bad "prometheus unit change is applied by SIGHUP only"
fi

# Restart handlers: one replica at a time, each waits for /-/ready (credential on
# stdin, not argv) before the next replica restarts.
if "$PY" -I - "$ROLE/handlers/main.yml" <<'PYEOF'
import jinja2, os, subprocess, sys, tempfile, yaml
hs = {h["name"]: h for h in yaml.safe_load(open(sys.argv[1]))}
bad = 0
for svc in ("prometheus", "alertmanager"):
    h = hs[f"restart {svc}"]
    v = {f"{svc}_listen": "0.0.0.0:9", f"{svc}_port": 9, f"{svc}_basic_auth_password_hash": "h",
         f"{svc}_basic_auth_username": "operator", "ansible_host": "10.0.0.5", "inventory_hostname": "prom1"}
    script = jinja2.Environment().from_string(h["shell"]).render(**v)
    # The credential is read from the client password file on the host, never rendered into the script.
    if "s3cret-fake" in script or f"/etc/prometheus/{svc}-client-password" not in script:
        bad += 1
    for ready_after, want_rc in ((2, 0), (10**6, 1)):
        with tempfile.TemporaryDirectory() as d:
            open(f"{d}/{svc}-client-password", "w").write("s3cret-fake")
            run = script.replace("/etc/prometheus/", f"{d}/")
            stubs = {"systemctl": f'echo "systemctl $*" >> {d}/calls\n',
                     "sleep": "",
                     "curl": f'echo "curl $*" >> {d}/calls; cat >> {d}/stdin; n=$(grep -c ^curl {d}/calls); [ "$n" -gt {ready_after} ]\n'}
            for n, body in stubs.items():
                open(f"{d}/{n}", "w").write("#!/bin/sh\n" + body)
                os.chmod(f"{d}/{n}", 0o755)
            env = {**os.environ, "PATH": f"{d}:{os.environ['PATH']}"}
            rc = subprocess.run(["bash", "-c", run], env=env, stdin=subprocess.DEVNULL, capture_output=True).returncode
            calls, stdin = open(f"{d}/calls").read(), open(f"{d}/stdin").read()
        good = (h.get("throttle") == 1 and rc == want_rc
                and f"systemctl restart {svc}" in calls and "s3cret-fake" not in calls
                and 'user = "operator:s3cret-fake"' in stdin and "http://127.0.0.1:9/-/ready" in stdin)
        label = "waits for /-/ready" if want_rc == 0 else "fails when never ready"
        print(("  ok — " if good else "  FAIL — ") + f"restart {svc} is throttled to one host and {label}")
        bad += not good
sys.exit(1 if bad else 0)
PYEOF
then :; else failed=1; fi

# Run the real preflight assert task, alone, against each hash/plaintext combination.
"$PY" -I -c '
import sys, yaml
t = next(t for t in yaml.safe_load(open(sys.argv[1])) if t.get("name", "").startswith("assert a basic-auth hash comes with"))
yaml.safe_dump([{"hosts": "localhost", "connection": "local", "gather_facts": False, "tasks": [t]}], open(sys.argv[2], "w"))
' "$ROLE/tasks/01-preflight.yml" "$TMPD/preflight.yml"
preflight() { # <expected rc 0|1> <label> <extra vars...>
  local want="$1" label="$2" rc=0
  shift 2
  ansible-playbook -i localhost, "$TMPD/preflight.yml" "$@" >"$TMPD/preflight.log" 2>&1 || rc=1
  if [ "$rc" = "$want" ]; then ok "preflight $label"; else bad "preflight $label (rc=$rc, want $want)"; fi
}
preflight 1 "refuses a hash without its plaintext" -e prometheus_basic_auth_password_hash=h -e alertmanager_basic_auth_password_hash=h -e prometheus_basic_auth_password=p
preflight 1 "refuses an alertmanager hash without its plaintext" -e alertmanager_basic_auth_password_hash=h
preflight 0 "accepts a hash with its plaintext" -e prometheus_basic_auth_password_hash=h -e alertmanager_basic_auth_password_hash=h -e prometheus_basic_auth_password=p -e alertmanager_basic_auth_password=p
preflight 0 "accepts unset hashes" -e prometheus_basic_auth_password_hash=

[ "$failed" -eq 0 ] || exit 1
echo "prometheus-ha-render-test: PASS"
