#!/usr/bin/env bash
# redis-sentinel-firewall-ipv6-test.sh — regression test for SL26.
#
# 06-firewall.yml built its internal_v4/ssh_allowed_v4 nftables sets with
# `{% for cidr in ... if ':' not in cidr %}`, which filters OUT any IPv6
# CIDR — silently. An operator adding an IPv6 entry to
# redis_internal_cidrs or redis_allowed_ssh_cidrs got no error and no
# rule: the entry just vanished from the rendered ruleset, so IPv6-only
# peers had no path in even though the inventory said they should. This
# renders the SAME Jinja content nftables.conf is built from and asserts
# an IPv6 CIDR lands in a v6 set AND an accept rule.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="configs/ansible/roles/redis-sentinel/tasks/06-firewall.yml"

if [ ! -f "$TASKS" ]; then
  echo "redis-sentinel-firewall-ipv6-test: FAIL — missing $TASKS" >&2
  exit 1
fi

# Same interpreter discovery as check-alertmanager-parity.sh: a plain
# python3 rarely has jinja2, but ansible-playbook ships its own.
PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 &&
    "$cand" -c 'import jinja2' >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2' >/dev/null 2>&1; then PY="$cand"; fi
fi
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "redis-sentinel-firewall-ipv6-test: FAIL — no python3 with jinja2 (required in CI)" >&2
    exit 1
  fi
  echo "redis-sentinel-firewall-ipv6-test: SKIP (no python3 with jinja2 locally; CI enforces)"
  exit 0
fi

"$PY" - "$TASKS" <<'PYEOF'
import re
import sys

import jinja2

tasks_path = sys.argv[1]
src = open(tasks_path).read()

m = re.search(r"content: \|\n((?:(?:      .*)?\n)+)", src)
if not m:
    print("redis-sentinel-firewall-ipv6-test: FAIL — could not locate the nft `content:` block", file=sys.stderr)
    sys.exit(1)
lines = m.group(1).splitlines()
nft_src = "\n".join(l[6:] if l.startswith("      ") else l for l in lines)

rendered = jinja2.Template(nft_src).render(
    redis_internal_cidrs=["10.0.0.0/8", "2001:db8:1::/64"],
    redis_allowed_ssh_cidrs=["10.0.0.0/8", "2001:db8:2::/64"],
    redis_port=6379,
    redis_sentinel_port=26379,
    redis_exporter_listen_port=9121,
)

failed = False


def check(cond, msg):
    global failed
    if cond:
        print("  ok —", msg)
    else:
        failed = True
        print("  FAIL —", msg, file=sys.stderr)


check("2001:db8:1::/64" in rendered, "internal IPv6 CIDR appears somewhere in the rendered ruleset")
check(
    re.search(r"set internal_v6 \{[^}]*2001:db8:1::/64", rendered, re.S) is not None,
    "internal IPv6 CIDR lands inside an internal_v6 set (not just dropped text)",
)
check("2001:db8:2::/64" in rendered, "ssh-allowed IPv6 CIDR appears somewhere in the rendered ruleset")
check(
    re.search(r"set ssh_allowed_v6 \{[^}]*2001:db8:2::/64", rendered, re.S) is not None,
    "ssh-allowed IPv6 CIDR lands inside an ssh_allowed_v6 set",
)
check("ip6 saddr @internal_v6" in rendered, "an accept rule matches on @internal_v6")
check("ip6 saddr @ssh_allowed_v6" in rendered, "an accept rule matches on @ssh_allowed_v6")

if failed:
    sys.exit(1)
print("redis-sentinel-firewall-ipv6-test: PASS")
PYEOF
