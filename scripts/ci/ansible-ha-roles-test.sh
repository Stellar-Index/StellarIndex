#!/usr/bin/env bash
# ansible-ha-roles-test.sh — safety properties of the patroni, redis-sentinel
# and loki roles that no host run covers yet (the HA hosts are not built).
# Reads and renders the real role files with PyYAML/Jinja; no hosts, no ansible run.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLES="${ROLES:-configs/ansible/roles}"

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
    echo "ansible-ha-roles-test: FAIL — no python3 with jinja2 and yaml (required in CI)" >&2
    exit 1
  fi
  echo "ansible-ha-roles-test: SKIP (no python3 with jinja2 and yaml locally; CI enforces)"
  exit 0
fi

"$PY" - "$ROLES" <<'PYEOF'
import re
import sys

import jinja2
import yaml

roles = sys.argv[1]
failed = False


def check(cond, msg):
    global failed
    if cond:
        print("  ok —", msg)
    else:
        failed = True
        print("  FAIL —", msg, file=sys.stderr)


def text(path):
    return open(f"{roles}/{path}").read()


def tasks(path):
    return yaml.safe_load(text(path))


def code(path):
    return "\n".join(l for l in text(path).splitlines() if not l.lstrip().startswith("#"))


def script_body(path, name):
    t = next(t for t in tasks(path) if t.get("name") == name)
    return t["copy"]["content"]


# loki: the pinned Loki 3.x rejects the 2.x storage keys.
check(yaml.safe_load(text("loki/defaults/main.yml"))["loki_version"].startswith("3."), "loki is pinned to 3.x")
cfg = code("loki/templates/loki-config.yaml.j2")
check("shared_store" not in cfg and "boltdb" not in cfg, "loki config has no 2.x shared_store / boltdb keys")
check("store: tsdb" in cfg and "schema: v13" in cfg and "tsdb_shipper:" in cfg, "loki config uses tsdb schema v13")
check("delete_request_store: s3" in cfg, "loki compactor names a delete_request_store")
check(any("uri" in t and "/ready" in t["uri"]["url"] for t in tasks("loki/tasks/server-04-systemd.yml")),
      "loki start is followed by a /ready wait")

# loki: a bad config or missing credentials must fail the play, not leave
# a green run over a crash-looping or non-flushing Loki.
check(any("loki-config.yaml" in str(t.get("template", {}).get("dest", "")) and
          "-verify-config" in str(t["template"].get("validate", ""))
          for t in tasks("loki/tasks/server-03-configure.yml")),
      "loki config is validated with -verify-config before it lands")
srv = tasks("loki/tasks/server-04-systemd.yml")
names = [t.get("name", "") for t in srv]
start = next(i for i, t in enumerate(srv) if t.get("name") == "enable + start loki")
ready = next(i for i, t in enumerate(srv) if "uri" in t and "/ready" in t["uri"]["url"])
check(any(t.get("meta") == "flush_handlers" for t in srv[start:ready]),
      "loki restart handlers run before the /ready wait")
check(any("/etc/default/loki" in str(t.get("shell", "")) and "AWS_SECRET_ACCESS_KEY" in str(t.get("shell", ""))
          for t in srv[:start]),
      "loki refuses to start without S3 credentials in /etc/default/loki")
check(any("archival_nodes" in str(t.get("assert", {})) for t in tasks("loki/tasks/server-01-preflight.yml")),
      "loki server preflight refuses archival nodes (the role replaces the whole firewall)")

# textfile scrapers: node_exporter is not root, mktemp files are 0600.
for path, name in (("patroni/tasks/11-monitoring.yml", "install patroni-textfile scraper"),
                   ("redis-sentinel/tasks/07-monitoring.yml", "install sentinel-state textfile scraper")):
    body = script_body(path, name)
    check(body.find('chmod 0644 "$TMP"') != -1 and body.find('chmod 0644 "$TMP"') < body.find('mv -f "$TMP"'),
          f"{path.split('/')[0]} scraper publishes a 0644 file")

sentinel = script_body("redis-sentinel/tasks/07-monitoring.yml", "install sentinel-state textfile scraper")
check(not re.search(r'redis-cli[^\n]*\n?[^\n]*-a\s+"?\$', sentinel) and "REDISCLI_AUTH=" in sentinel,
      "sentinel scraper passes the password by environment, not on argv")

# clustered restarts go one member at a time.
for h in yaml.safe_load(text("patroni/handlers/main.yml")):
    if h["name"] in ("restart etcd", "restart patroni"):
        check(h.get("throttle") == 1, f"patroni handler '{h['name']}' is throttled to one host")

# redis: credentials live here, so eviction and failover loss are bounded.
rd = yaml.safe_load(text("redis-sentinel/defaults/main.yml"))
check(rd["redis_maxmemory_policy"] == "noeviction", "redis never evicts by default")
check(rd.get("redis_min_replicas_to_write", 0) >= 1, "redis bounds acknowledged-write loss by default")
env = jinja2.Environment(trim_blocks=True)
env.filters["bool"] = bool
conf = env.from_string(text("redis-sentinel/templates/redis.conf.j2")).render(
    **{"redis_min_replicas_to_write": 0, "redis_min_replicas_max_lag": 10, **rd, "redis_bind_address": "10.0.0.22", "redis_password": "x", "redis_replication_auth_user": "r",
       "redis_log_dir": "/l", "redis_data_dir": "/d", "ansible_host": "10.0.0.22",
       "redis_effective_primary_host": "10.0.0.22"})
check("min-replicas-to-write 1" in conf, "redis.conf renders min-replicas-to-write")

# redis: a fresh or replaced host must hear the surviving Sentinels.
peer = next(t for t in tasks("redis-sentinel/tasks/03-redis-configure.yml") if t.get("name", "").startswith("ask the other Sentinels"))
check("redis_sentinel_managed" not in str(peer["when"]), "peer-Sentinel query is not skipped on a host without a rendered sentinel.conf")

# patroni: bootstrap is keyed on Patroni's own marker, never the distro's PG_VERSION.
boot = tasks("patroni/tasks/08-patroni-bootstrap.yml")
for blk in [t for t in boot if "block" in t]:
    for t in blk["block"]:
        if t["name"] in ("bootstrap via pgBackRest restore", "ensure restored data dir ownership"):
            check("_patroni_initialized.stat.exists" in str(t["when"]) and "_pg_data_present" not in str(t["when"]),
                  f"'{t['name']}' is gated on Patroni's marker, not PG_VERSION")
check(any("adopt" in str(t.get("when")) for t in boot), "bootstrap refuses a foreign cluster without an explicit opt-in")
check(yaml.safe_load(text("patroni/defaults/main.yml")).get("patroni_adopt_existing_data_dir", True) is False, "adopting a foreign data dir is off by default")
check(any("mask" in str(t.get("command", "")) for t in tasks("patroni/tasks/07-patroni-systemd.yml")), "the distro postgresql unit is masked")

if failed:
    sys.exit(1)
print("ansible-ha-roles-test: PASS")
PYEOF
