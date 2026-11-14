#!/usr/bin/env bash
# loki-push-target-test.sh — every Promtail agent must be given a Loki address
# that Loki listens on, or the play must refuse.
#
# Evaluates the loki role's real promtail_target_url default and the agent
# refusal with Jinja. No hosts, no ansible run.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="configs/ansible/roles/loki"

PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then PY="$cand"; fi
fi
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "loki-push-target-test: FAIL — no python3 with jinja2 and yaml (required in CI)" >&2
    exit 1
  fi
  echo "loki-push-target-test: SKIP (no python3 with jinja2 and yaml locally; CI enforces)"
  exit 0
fi

"$PY" - "$ROLE" <<'PYEOF'
import re
import sys

import jinja2
import yaml

role = sys.argv[1]
env = jinja2.Environment()
env.tests["match"] = lambda value, pattern: re.match(pattern, value) is not None

url_tmpl = yaml.safe_load(open(f"{role}/defaults/main.yml"))["promtail_target_url"]
tasks = yaml.safe_load(open(f"{role}/tasks/agent-02-configure.yml"))
refusal = next(t for t in tasks if t["name"] == "refuse to ship to a Loki that only listens on loopback")
cond = refusal["assert"]["that"][0]
failed = False


def check(cond_ok, msg):
    global failed
    if cond_ok:
        print("  ok —", msg)
    else:
        failed = True
        print("  FAIL —", msg, file=sys.stderr)


def ctx(host, listen, override=None):
    c = dict(
        groups={"log_aggregator": ["mon-01"], "log_shippers": ["mon-01", "app-01"]},
        hostvars={"mon-01": {"ansible_host": "10.0.0.9", "loki_listen_address": listen}, "app-01": {"ansible_host": "10.0.0.5"}},
        inventory_hostname=host, loki_listen_address=listen, loki_http_port=3100, loki_agg="mon-01", loki_agg_listen=listen,
    )
    c["promtail_target_url"] = override or env.from_string(url_tmpl).render(**c)
    return c


def allowed(c):
    return env.from_string("{{ " + cond + " }}").render(**c) == "True"


push = "/loki/api/v1/push"
check(ctx("mon-01", "127.0.0.1")["promtail_target_url"] == "http://127.0.0.1:3100" + push,
      "the aggregator's own agent ships to the loopback address Loki binds")
check(ctx("mon-01", "::1")["promtail_target_url"] == "http://[::1]:3100" + push, "an IPv6 bind address is bracketed")
check(ctx("mon-01", "0.0.0.0")["promtail_target_url"] == "http://10.0.0.9:3100" + push,
      "a wildcard bind falls back to the aggregator's ansible_host")
check(ctx("app-01", "10.0.0.9")["promtail_target_url"] == "http://10.0.0.9:3100" + push,
      "a remote agent ships to the aggregator's ansible_host")
check(not allowed(ctx("app-01", "127.0.0.1")), "a remote agent is refused while Loki listens on loopback only")
check(allowed(ctx("app-01", "10.0.0.9")), "a remote agent is allowed once Loki listens on the internal address")
check(allowed(ctx("app-01", "127.0.0.1", "http://lb.internal:3100" + push)), "an overridden front is not refused")
check("inventory_hostname != groups['log_aggregator'][0]" in str(refusal.get("when")), "the refusal skips the aggregator itself")

if failed:
    sys.exit(1)
print("loki-push-target-test: PASS")
PYEOF
