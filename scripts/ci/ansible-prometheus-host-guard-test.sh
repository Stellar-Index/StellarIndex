#!/usr/bin/env bash
# ansible-prometheus-host-guard-test.sh — the prometheus role must never
# reach an archival node, not even through a tag-filtered run.
#
# roles/prometheus/tasks/06-firewall.yml renders the WHOLE /etc/nftables.conf
# with `flush ruleset`. playbooks/monitoring.yml targeted archival_nodes and
# the role's preflight was not tagged `always`, so
# `ansible-playbook -i inventory/r1.yml playbooks/monitoring.yml --tags firewall`
# skipped every guard and replaced r1's ruleset: Caddy 80/443 (the public
# API origin) gone and SSH narrowed to RFC1918.
#
# Uses ansible-playbook's own host and task selection (--list-hosts,
# --list-tasks); nothing connects to or changes any host.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1

if ! command -v ansible-playbook >/dev/null 2>&1; then
  if [ "${CI:-}" = "true" ]; then
    echo "ansible-prometheus-host-guard-test: FAIL — ansible-playbook missing (required in CI)" >&2
    exit 1
  fi
  echo "ansible-prometheus-host-guard-test: SKIP (no ansible-playbook locally; CI enforces)"
  exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# r1's shape: one host, in archival_nodes only.
cat >"$tmp/r1-shape.yml" <<'EOF'
all:
  children:
    archival_nodes:
      hosts:
        guard-r1: { ansible_host: 192.0.2.10 }
EOF
# An inventory that co-locates the prom pair with an archival node.
cat >"$tmp/overlap.yml" <<'EOF'
all:
  children:
    archival_nodes:
      hosts:
        guard-a1: { ansible_host: 192.0.2.11 }
    prometheus_pair:
      hosts:
        guard-a1: {}
        guard-p2: { ansible_host: 192.0.2.12 }
EOF

fails=0
pass() { echo "  ok — $1"; }
fail() {
  echo "  FAIL — $1" >&2
  fails=$((fails + 1))
}

run() { (cd configs/ansible && ansible-playbook "$@") >"$tmp/out" 2>&1 || {
  echo "  FAIL — ansible-playbook $* exited non-zero:" >&2
  cat "$tmp/out" >&2
  exit 1
}; }

run -i "$tmp/r1-shape.yml" playbooks/monitoring.yml --list-hosts
if grep -q 'guard-r1' "$tmp/out"; then
  fail "monitoring.yml selects an archival-only host (r1 shape)"
else
  pass "monitoring.yml selects no archival-only host"
fi

run -i "$tmp/overlap.yml" playbooks/monitoring.yml --tags firewall --list-tasks
for task in \
  "assert host is in the prometheus_pair inventory group" \
  "assert host is not an archival node"; do
  if grep -q "prometheus : $task" "$tmp/out"; then
    pass "--tags firewall still runs: $task"
  else
    fail "--tags firewall skips the guard: $task"
  fi
done
guard_at=$(grep -n -m1 'assert host is not an archival node' "$tmp/out" | cut -d: -f1 || true)
render_at=$(grep -n -m1 'render full /etc/nftables.conf' "$tmp/out" | cut -d: -f1 || true)
if [ -n "$guard_at" ] && [ -n "$render_at" ] && [ "$guard_at" -lt "$render_at" ]; then
  pass "the archival-node guard runs before the nftables render"
else
  fail "the archival-node guard does not precede the nftables render"
fi

if [ "$fails" -ne 0 ]; then
  echo "ansible-prometheus-host-guard-test: $fails failure(s)" >&2
  exit 1
fi
echo "ansible-prometheus-host-guard-test: PASS"
