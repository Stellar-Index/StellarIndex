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
import itertools
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile

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

# textfile scrapers: node_exporter is not root, mktemp files are 0600.
for path, name in (("patroni/tasks/11-monitoring.yml", "install patroni-textfile scraper"),
                   ("redis-sentinel/tasks/07-monitoring.yml", "install sentinel-state textfile scraper"),
                   ("haproxy/tasks/07-monitoring.yml", "install keepalived-state textfile scraper")):
    body = script_body(path, name)
    check(body.find('chmod 0644 "$TMP"') != -1 and body.find('chmod 0644 "$TMP"') < body.find('mv -f "$TMP"'),
          f"{path.split('/')[0]} scraper publishes a 0644 file")

sentinel = script_body("redis-sentinel/tasks/07-monitoring.yml", "install sentinel-state textfile scraper")
check(not re.search(r'redis-cli[^\n]*\n?[^\n]*-a\s+"?\$', sentinel) and "REDISCLI_AUTH=" in sentinel,
      "sentinel scraper passes the password by environment, not on argv")

# clustered restarts go one member at a time.
handlers = {h["name"]: h for h in yaml.safe_load(text("patroni/handlers/main.yml"))}
for name in ("restart etcd", "restart patroni"):
    check(handlers[name].get("throttle") == 1, f"patroni handler '{name}' is throttled to one host")


def run_patroni_restart(cluster_json):
    """Run the rendered restart handler with curl/systemctl stubbed; return (rc, systemctl log)."""
    script = jinja2.Environment().from_string(handlers["restart patroni"]["shell"]).render(
        patroni_rest_connect_address="10.0.0.11:8008", inventory_hostname="db-01",
        groups={"postgres_cluster": ["db-01", "db-02", "db-03"]})
    with tempfile.TemporaryDirectory() as d:
        stub = {"curl": f'case "$*" in *"/cluster"*) cat {d}/cluster.json ;; esac\n',
                "systemctl": f'echo "$*" >> {d}/systemctl.log\n', "sleep": ""}
        for name, body in stub.items():
            with open(f"{d}/{name}", "w") as f:
                f.write("#!/bin/sh\n" + body)
            os.chmod(f"{d}/{name}", 0o755)
        with open(f"{d}/cluster.json", "w") as f:
            f.write(json.dumps(cluster_json))
        env = {**os.environ, "PATH": f"{d}:{os.environ['PATH']}"}
        rc = subprocess.run(["bash", "-c", script], env=env, stdin=subprocess.DEVNULL,
                            capture_output=True).returncode
        log = open(f"{d}/systemctl.log").read() if os.path.exists(f"{d}/systemctl.log") else ""
    return rc, log


if shutil.which("jq") is None and os.environ.get("CI") != "true":
    print("  skip — patroni restart handler run (no jq locally; CI enforces)")
else:
    healthy = {"members": [{"role": "leader", "state": "running"},
                           {"role": "sync_standby", "state": "streaming"},
                           {"role": "replica", "state": "streaming"}]}
    rc, log = run_patroni_restart(healthy)
    check(rc == 0 and "restart patroni" in log, "patroni restart handler restarts a healthy cluster's member")
    degraded = {"members": healthy["members"][:2]}
    rc, log = run_patroni_restart(degraded)
    check(rc != 0 and "restart" not in log, "patroni restart handler refuses while a member is missing")
    leaderless = {"members": [{"role": "replica", "state": "streaming"}] * 3}
    rc, log = run_patroni_restart(leaderless)
    check(rc != 0 and "restart" not in log, "patroni restart handler refuses while there is no leader")

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

# firewalls: nft rejects `elements = { }`, which the defaults' IPv4-only CIDR lists used to render.
for role, prefix in (("haproxy", "haproxy"), ("redis-sentinel", "redis")):
    fw_task = next(t for t in tasks(f"{role}/tasks/06-firewall.yml") if "content" in t.get("copy", {}))
    fw = jinja2.Environment(undefined=jinja2.ChainableUndefined).from_string(fw_task["copy"]["content"])
    dflt = yaml.safe_load(text(f"{role}/defaults/main.yml"))
    for label, cidrs in (("defaults", None), ("IPv6 only", ["fd00::/8"]), ("mixed", ["10.0.0.0/8", "fd00::/8", "fe80::/10"])):
        v = dict(dflt)
        if cidrs:
            v[f"{prefix}_internal_cidrs"] = v[f"{prefix}_allowed_ssh_cidrs"] = cidrs
        out = fw.render(**v)
        sets = {m[1]: m[2] for m in re.findall(r"^( *)set (\w+_v[46]) \{\n(.*?)^\1\}$", out, re.S | re.M)}
        check(len(sets) >= 4 and not re.search(r"elements = \{\s*\}", out),
              f"{role} firewall ({label}) renders no empty nft element set")
        for n in ("internal_v4", "internal_v6"):
            want = [c for c in v[f"{prefix}_internal_cidrs"] if (":" in c) == n.endswith("v6")]
            got = re.findall(r"[0-9a-f.:]+/\d+", sets.get(n, ""))
            check(got == want, f"{role} firewall ({label}) {n} holds {want}")

# patroni: bootstrap is keyed on Patroni's own marker, never the distro's PG_VERSION.
boot = tasks("patroni/tasks/08-patroni-bootstrap.yml")
for blk in [t for t in boot if "block" in t]:
    for t in blk["block"]:
        if t["name"] in ("bootstrap via pgBackRest restore", "ensure restored data dir ownership"):
            check("_patroni_initialized.stat.exists" in str(t["when"]) and "_pg_data_present" not in str(t["when"]),
                  f"'{t['name']}' is gated on Patroni's marker, not PG_VERSION")
check(yaml.safe_load(text("patroni/defaults/main.yml")).get("patroni_adopt_existing_data_dir", True) is False, "adopting a foreign data dir is off by default")
check(any("mask" in str(t.get("command", "")) for t in tasks("patroni/tasks/07-patroni-systemd.yml")), "the distro postgresql unit is masked")

# patroni: the data-dir gate runs on every host, untagged-proof, before 07 masks anything.
order = [t.get("import_tasks") for t in tasks("patroni/tasks/main.yml")]
gate_entry = next(t for t in tasks("patroni/tasks/main.yml") if t.get("import_tasks") == "data-dir-gate.yml")
check(order.index("data-dir-gate.yml") < order.index("07-patroni-systemd.yml") and "when" not in gate_entry
      and "always" in gate_entry.get("tags", []), "data-dir gate runs on every host before the distro unit is masked")

ansible_env = jinja2.Environment()
ansible_env.filters["bool"] = lambda v: v if isinstance(v, bool) else str(v).lower() in ("1", "true", "yes")


def fires(conds, **v):
    conds = conds if isinstance(conds, list) else [conds]
    return all(ansible_env.from_string("{{ (%s) | bool }}" % c).render(**v) == "True" for c in conds)


gate = {t["name"]: t for t in tasks("patroni/tasks/data-dir-gate.yml")}
refuse = gate["refuse to start Patroni on a cluster it did not create"]["when"]
adopt_ok = gate["refuse adopting a foreign data dir outside a pgBackRest restore"]["assert"]["that"]
for host, marker, pgv, method, adopt in itertools.product(["db-01", "db-02"], [True, False], [True, False],
                                                          ["initdb", "pgbackrest"], [True, False]):
    v = dict(inventory_hostname=host, groups={"postgres_cluster": ["db-01", "db-02", "db-03"]},
             _patroni_initialized={"stat": {"exists": marker}}, _pg_data_present={"stat": {"exists": pgv}},
             patroni_bootstrap_method=method, patroni_adopt_existing_data_dir=adopt)
    want = pgv and not marker and not (host == "db-01" and method == "pgbackrest" and adopt)
    check(fires(refuse, **v) == want, f"data-dir gate on {host} marker={marker} PG_VERSION={pgv} {method} adopt={adopt} -> refuse={want}")
    check(fires(adopt_ok, **v) == (not adopt or method == "pgbackrest"), f"adopt={adopt} accepted only with pgbackrest ({method})")

# patroni: --target-action is valid only with a recovery target (pgBackRest error 031).
restore = next(t for blk in boot if "block" in blk for t in blk["block"] if t["name"] == "bootstrap via pgBackRest restore")
ansible_env.filters["quote"] = shlex.quote
for target, want in (("latest", False), ("immediate", True), ("time:2026-04-30 14:00:00", True)):
    cmd = ansible_env.from_string(restore["command"]).render(patroni_pgbackrest_stanza="s", patroni_pgbackrest_restore_target=target)
    check(("--target-action=promote" in cmd) == want, f"pgbackrest restore for {target} has target-action={want}")

# patroni: a replica join is confirmed by the member's own /replica check, retried through the base backup.
confirm = next(t for t in tasks("patroni/tasks/09-patroni-join.yml") if t["name"] == "confirm we joined as a replica")
check(confirm.get("uri", {}).get("url", "").endswith("/replica") and "_replica_check" in str(confirm.get("until")),
      "replica join asserts /replica answers 200")

# patroni/etcd: "new" on an empty data dir is refused beside a live quorum or on a partial play.
check('ETCD_INITIAL_CLUSTER_STATE="{{ etcd_initial_cluster_state }}"' in text("patroni/templates/etcd.conf.j2")
      and yaml.safe_load(text("patroni/defaults/main.yml"))["etcd_initial_cluster_state"] == "new",
      "etcd initial cluster state is a variable defaulting to new")
check(re.search(r'^ETCD_AUTO_COMPACTION_RETENTION="(?!0")[^"]+"', code("patroni/templates/etcd.conf.j2"), re.M) is not None,
      "etcd auto-compaction is enabled")
etcd_refuse = next(t for t in tasks("patroni/tasks/03-etcd-configure.yml")
                   if t.get("name") == "refuse to bootstrap a new etcd cluster beside a live one")["when"]
group = ["db-01", "db-02", "db-03"]
for member, state, quorum, play, want in ((False, "new", 1, group, False), (False, "new", 0, group, True),
                                          (False, "new", 1, ["db-02"], True), (True, "new", 0, ["db-02"], False),
                                          (False, "existing", 0, ["db-02"], False)):
    v = dict(_etcd_member_dir={"stat": {"exists": member}}, etcd_initial_cluster_state=state,
             _etcd_peer_quorum={"rc": quorum}, groups={"postgres_cluster": group}, ansible_play_hosts_all=play)
    ansible_env.filters["difference"] = lambda a, b: [x for x in a if x not in b]
    check(fires(etcd_refuse, **v) == want,
          f"etcd refuse (member dir={member} state={state} peer quorum rc={quorum} play={play}) -> {want}")

# redis: the live maxmemory-policy enforce runs only on a managed host whose Sentinel answered;
# a fresh host (distro redis on loopback, AUTH failing with rc 0 and no output) is left to the template.
enforce = next(t for t in tasks("redis-sentinel/tasks/03-redis-configure.yml")
               if t.get("name") == "enforce maxmemory-policy on the running instance")
for managed, answered, render, want in ((False, False, True, False), (True, True, False, True),
                                        (True, False, False, False), (True, True, True, False)):
    v = dict(redis_sentinel_managed={"rc": 0 if managed else 1}, redis_sentinel_state={"rc": 0, "stdout_lines": []},
             redis_sentinel_answered=answered, redis_render_configs=render)
    check(fires(enforce["when"], **v) == want,
          f"redis policy enforce (managed={managed} answered={answered} render={render}) runs={want}")

if failed:
    sys.exit(1)
print("ansible-ha-roles-test: PASS")
PYEOF
