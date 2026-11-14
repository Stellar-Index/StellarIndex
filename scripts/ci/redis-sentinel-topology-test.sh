#!/usr/bin/env bash
# redis-sentinel-topology-test.sh — a re-render of the redis-sentinel role must
# follow the live primary, not the inventory role, and keep the Sentinel id.
#
# Renders the role's real redis.conf.j2 and sentinel.conf.j2 and reads the two
# configure task files. No hosts, no ansible run.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="configs/ansible/roles/redis-sentinel"

PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import jinja2' >/dev/null 2>&1; then
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
    echo "redis-sentinel-topology-test: FAIL — no python3 with jinja2 (required in CI)" >&2
    exit 1
  fi
  echo "redis-sentinel-topology-test: SKIP (no python3 with jinja2 locally; CI enforces)"
  exit 0
fi

"$PY" - "$ROLE" <<'PYEOF'
import re
import sys

import jinja2

role = sys.argv[1]
env = jinja2.Environment(trim_blocks=True)
env.filters["bool"] = bool

base = dict(
    redis_bind_address="10.0.0.22", redis_port=6379, redis_password="fixture-password",
    redis_acl_lockdown=False, redis_replication_auth_user="replication",
    redis_appendonly="yes", redis_aof_appendfsync="everysec", redis_save_rules=["900 1"],
    redis_maxmemory="4gb", redis_maxmemory_policy="noeviction",
    redis_log_dir="/var/log/redis", redis_data_dir="/var/lib/redis",
    redis_sentinel_port=26379, redis_sentinel_master_name="cache", redis_sentinel_quorum=2,
    redis_sentinel_auth_user="sentinel", redis_sentinel_down_after_milliseconds=5000,
    redis_sentinel_failover_timeout=60000, redis_sentinel_parallel_syncs=1,
    redis_sentinel_data_dir="/var/lib/redis-sentinel",
)
failed = False


def check(cond, msg):
    global failed
    if cond:
        print("  ok —", msg)
    else:
        failed = True
        print("  FAIL —", msg, file=sys.stderr)


def render(name, **kw):
    return env.from_string(open(f"{role}/templates/{name}").read()).render(**{**base, **kw})


def directives(text, word):
    return [l for l in text.splitlines() if l.startswith(word)]


# After a failover cache-02 (inventory role: replica) is the primary.
live = "10.0.0.22"
on_new_primary = render("redis.conf.j2", ansible_host="10.0.0.22", redis_role="replica", redis_effective_primary_host=live)
check(directives(on_new_primary, "replicaof") == [], "the live primary gets no replicaof, whatever its inventory role")
on_old_primary = render("redis.conf.j2", ansible_host="10.0.0.21", redis_role="primary", redis_effective_primary_host=live)
check(directives(on_old_primary, "replicaof") == [f"replicaof {live} 6379"], "the inventory primary follows the live primary")

myid = "a" * 40
kept = render("sentinel.conf.j2", redis_effective_primary_host=live, redis_sentinel_myid={"stdout": myid + "\n"})
check(directives(kept, "sentinel myid") == [f"sentinel myid {myid}"], "a re-render keeps the Sentinel id the host already has")
fresh = render("sentinel.conf.j2", redis_effective_primary_host=live, redis_sentinel_myid={"stdout": ""})
check(directives(fresh, "sentinel myid") == [], "a first render writes no myid line")
check(directives(kept, "sentinel monitor") == [f"sentinel monitor cache {live} 6379 2"], "sentinel monitor names the live primary")

tasks = open(f"{role}/tasks/03-redis-configure.yml").read() + open(f"{role}/tasks/04-sentinel-configure.yml").read()
whens = re.findall(r"^\s+when:.*$|^\s+- .*redis_sentinel_state\.rc.*$", tasks, re.M)
check(not any("redis_sentinel_state.rc != 0" in w for w in whens),
      "no render is gated on the local Sentinel merely not answering")
check("refuse to re-render a configured host when no Sentinel names the primary" in tasks
      and "refuse to render when the live primary is not an inventory host" in tasks,
      "both refusals are present")

if failed:
    sys.exit(1)
print("redis-sentinel-topology-test: PASS")
PYEOF
