#!/usr/bin/env bash
# redis-sentinel-acl-lockdown-test.sh — regression guard for SL02.
#
# Under redis_acl_lockdown, the legacy `default` Redis user is off
# (users.acl.j2: `user default off nopass nocommands`). Sentinel's
# username-less AUTH against that user was fixed by `sentinel
# auth-user` (SL03) — but a REPLICA's masterauth AUTH had the same
# failure mode and no `masteruser` directive to route around it, so
# under lockdown a replica could never sync from its master.
#
# This renders redis.conf.j2 and users.acl.j2 with jinja2 (the same
# templating engine ansible uses) under redis_acl_lockdown: true and
# pins that:
#   - redis.conf gets a `masteruser replication` line;
#   - users.acl defines a `replication` user with PSYNC/REPLCONF.
#
# Run: bash scripts/ci/redis-sentinel-acl-lockdown-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE_DIR="configs/ansible/roles/redis-sentinel"

pass=0
fail=0

record() {
  local desc="$1" ok="$2"
  if [ "$ok" = "1" ]; then
    echo "PASS: $desc"
    pass=$((pass + 1))
  else
    echo "FAIL: $desc"
    fail=$((fail + 1))
  fi
}

RENDERED_CONF="$(python3 - "$ROLE_DIR/templates/redis.conf.j2" <<'PY'
import sys
import jinja2

env = jinja2.Environment()
env.filters["bool"] = bool  # ansible's `| bool` filter; stub for this fixture

with open(sys.argv[1]) as f:
    tmpl = env.from_string(f.read())

print(tmpl.render(
    redis_bind_address="10.0.0.5",
    redis_port=6379,
    redis_password="fixture-password",
    redis_acl_lockdown=True,
    redis_replication_auth_user="replication",
    redis_appendonly="yes",
    redis_aof_appendfsync="everysec",
    redis_save_rules=["900 1"],
    redis_maxmemory="4gb",
    redis_maxmemory_policy="allkeys-lru",
    redis_role="replica",
    redis_effective_primary_host="10.0.0.4",
    redis_log_dir="/var/log/redis",
    redis_data_dir="/var/lib/redis",
))
PY
)"

RENDERED_ACL="$(python3 - "$ROLE_DIR/templates/users.acl.j2" <<'PY'
import sys
import jinja2

with open(sys.argv[1]) as f:
    tmpl = jinja2.Template(f.read())

print(tmpl.render(redis_password="fixture-password"))
PY
)"

conf_ok=0
[[ "$RENDERED_CONF" == *"masteruser replication"* ]] && conf_ok=1
record "redis.conf.j2 sets masteruser under lockdown" "$conf_ok"

acl_ok=0
if [[ "$RENDERED_ACL" == *"user replication on >fixture-password"* \
  && "$RENDERED_ACL" == *"+psync"* && "$RENDERED_ACL" == *"+replconf"* ]]; then
  acl_ok=1
fi
record "users.acl.j2 defines a replication ACL user with PSYNC/REPLCONF" "$acl_ok"

echo "redis-sentinel-acl-lockdown-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
