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
check(not any(t.get("meta") == "flush_handlers" for t in srv),
      "loki does not flush every pending handler (other roles' included)")
check(any("systemd" in t and t["systemd"].get("state") == "restarted" and t["systemd"].get("name") == "loki"
          and "_loki_config" in str(t.get("when")) and "_loki_unit" in str(t.get("when"))
          for t in srv[start + 1:ready]),
      "loki restarts itself on a config or unit change before the /ready wait")
check(any("loki config changed" in str(t.get("notify", "")) for t in srv)
      and any(h.get("listen") == "loki config changed" for h in yaml.safe_load(text("loki/handlers/main.yml"))),
      "loki keeps its restart handler for other paths")
probe = next((t for t in srv[:start] if "/etc/default/loki" in str(t.get("shell", ""))
              and "AWS_SECRET_ACCESS_KEY" in str(t.get("shell", ""))), None)
check(probe is not None, "loki refuses to start without S3 credentials in /etc/default/loki")
check(probe is not None and probe.get("check_mode") is False and probe.get("changed_when") is False,
      "loki credentials probe runs under --check (check_mode: false) and never reports changed")

# alertmanager: the unit carries the flags, so a unit change restarts it (SIGHUP does not re-read flags).
am_unit = next(t for t in tasks("prometheus/tasks/05-systemd.yml") if t.get("name") == "render alertmanager systemd unit")
am_handler = next((h for h in yaml.safe_load(text("prometheus/handlers/main.yml"))
                   if h.get("listen") == am_unit.get("notify")), None)
check(am_handler is not None and "systemctl restart alertmanager" in am_handler.get("shell", ""),
      "alertmanager unit change notifies a restarting handler")

# firewalls: nft rejects an empty `elements = { }`.
env_fw = jinja2.Environment(trim_blocks=True)
for path, name, pfx, port_vars in (
        ("loki/tasks/server-05-firewall.yml", "render full /etc/nftables.conf (default-drop + loki ingest + ssh)",
         "loki", {"loki_http_port": 3100}),
        ("prometheus/tasks/06-firewall.yml", "render full /etc/nftables.conf (default-drop + prom stack + ssh)",
         "prometheus", {"prometheus_port": 9090, "alertmanager_port": 9093, "alertmanager_cluster_listen_port": 9094})):
    body = next(t for t in tasks(path) if t.get("name") == name)["copy"]["content"]
    for label, internal, ssh, want in (
            ("default", ["10.0.0.0/8"], ["10.0.0.0/8"], 2),
            ("empty", [], [], 0),
            ("ipv6-only", ["fd00::/8"], ["fd00::/8"], 2),
            ("mixed", ["10.0.0.0/8", "fd00::/8"], ["192.0.2.1", "2001:db8::/32"], 4)):
        out = env_fw.from_string(body).render(**{f"{pfx}_internal_cidrs": internal, f"{pfx}_allowed_ssh_cidrs": ssh},
                                              **port_vars)
        check(not re.search(r"elements\s*=\s*\{\s*\}", out) and out.count("elements = {") == want
              and out.count("{") == out.count("}"),
              f"{pfx} firewall ({label} CIDRs) renders only non-empty elements lines")
check(any("archival_nodes" in str(t.get("assert", {})) for t in tasks("loki/tasks/server-01-preflight.yml")),
      "loki server preflight refuses archival nodes (the role replaces the whole firewall)")

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


def run_patroni_restart(before, after=None, switched=None, me="db-02"):
    """Run the rendered restart handler with curl/systemctl/patronictl stubbed.

    `after` replaces the /cluster answer once systemctl restarts, `switched` once
    patronictl switches over; None for `before` means this host's API is down.
    Returns (rc, log of systemctl and patronictl calls)."""
    jenv = jinja2.Environment()
    jenv.filters["quote"] = shlex.quote
    script = jenv.from_string(handlers["restart patroni"]["shell"]).render(
        patroni_rest_connect_address="10.0.0.12:8008", inventory_hostname=me,
        patroni_restart_max_lag_bytes=16777216, groups={"postgres_cluster": ["db-01", "db-02", "db-03"]})
    with tempfile.TemporaryDirectory() as d:
        script = script.replace("/etc/patroni/.restart-pending", f"{d}/restart-pending")
        stub = {"curl": f'case "$*" in *"/cluster"*) cat {d}/cluster.json 2>/dev/null || exit 7 ;; esac\n',
                "systemctl": f'echo "systemctl $*" >> {d}/calls.log\n'
                             f'case "$*" in restart*) [ -f {d}/after.json ] && cp {d}/after.json {d}/cluster.json ;; esac\nexit 0\n',
                "patronictl": f'echo "patronictl $*" >> {d}/calls.log\n'
                              f'[ -f {d}/switched.json ] && cp {d}/switched.json {d}/cluster.json\nexit 0\n',
                "sleep": ""}
        for name, body in stub.items():
            with open(f"{d}/{name}", "w") as f:
                f.write("#!/bin/sh\n" + body)
            os.chmod(f"{d}/{name}", 0o755)
        for name, doc in (("cluster", before), ("after", after), ("switched", switched)):
            if doc is not None:
                with open(f"{d}/{name}.json", "w") as f:
                    f.write(json.dumps(doc))
        env = {**os.environ, "PATH": f"{d}:{os.environ['PATH']}"}
        rc = subprocess.run(["bash", "-c", script], env=env, stdin=subprocess.DEVNULL,
                            capture_output=True).returncode
        log = open(f"{d}/calls.log").read() if os.path.exists(f"{d}/calls.log") else ""
        run_patroni_restart.pending = os.path.exists(f"{d}/restart-pending")
    return rc, log


def member(name, role, state, lag=0):
    m = {"name": name, "role": role, "state": state}
    if role != "leader":
        m["lag"] = lag
    return m


if shutil.which("jq") is None and os.environ.get("CI") != "true":
    print("  skip — patroni restart handler run (no jq locally; CI enforces)")
else:
    healthy = {"members": [member("db-01", "leader", "running"), member("db-02", "sync_standby", "streaming"),
                           member("db-03", "replica", "streaming")]}
    rc, log = run_patroni_restart(healthy)
    check(rc == 0 and "restart patroni" in log and "switchover" not in log and not run_patroni_restart.pending,
          "patroni restart handler restarts a healthy cluster's replica without a switchover and clears the pending marker")
    degraded = {"members": healthy["members"][:2]}
    rc, log = run_patroni_restart(degraded)
    check(rc != 0 and "restart" not in log, "patroni restart handler refuses a healthy member while another is missing")
    check(run_patroni_restart.pending, "a refused patroni restart leaves the pending marker for the next run")
    leaderless = {"members": [member(n, "replica", "streaming") for n in ("db-01", "db-02", "db-03")]}
    rc, log = run_patroni_restart(leaderless)
    check(rc != 0 and "restart" not in log, "patroni restart handler refuses a healthy member while there is no leader")
    running_replica = {"members": healthy["members"][:2] + [member("db-03", "replica", "running")]}
    rc, log = run_patroni_restart(running_replica)
    check(rc != 0 and "restart" not in log, "patroni restart handler counts a replica that is running but not streaming as unhealthy")
    lagging = {"members": healthy["members"][:2] + [member("db-03", "replica", "streaming", lag=1 << 30)]}
    rc, log = run_patroni_restart(lagging)
    check(rc != 0 and "restart" not in log, "patroni restart handler counts a replica beyond the lag bound as unhealthy")
    self_down = {"members": [member("db-01", "leader", "running"), member("db-02", "replica", "stopped", "unknown"),
                             member("db-03", "replica", "streaming")]}
    rc, log = run_patroni_restart(self_down, after=healthy)
    check(rc == 0 and "restart patroni" in log, "patroni restart handler restarts a member that is itself down")
    rc, log = run_patroni_restart(None, after=healthy)
    check(rc == 0 and "restart patroni" in log, "patroni restart handler restarts a member whose own API does not answer")
    switched = {"members": [member("db-01", "sync_standby", "streaming"), member("db-02", "leader", "running"),
                            member("db-03", "replica", "streaming")]}
    rc, log = run_patroni_restart(healthy, after=switched, switched=switched, me="db-01")
    check(rc == 0 and log.find("switchover --leader db-01") != -1
          and log.find("switchover --leader db-01") < log.find("systemctl restart patroni"),
          "patroni restart handler switches the leader away before restarting it")
    rc, log = run_patroni_restart(healthy, me="db-01")
    check(rc != 0 and "restart patroni" not in log,
          "patroni restart handler does not restart a leader that still holds the lock after the switchover")

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

# firewalls: nft rejects `elements = { }`, so an address family with no CIDRs must render no element set.
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
check(any("systemctl mask --now postgresql@" in str(t.get("command", "")) + str(t.get("shell", ""))
          for t in tasks("patroni/tasks/07-patroni-systemd.yml")), "the distro postgresql unit is masked")

# patroni: the data-dir gate runs on every host, untagged-proof, after the package install
# (which can create a cluster) and before 07 masks anything.
order = [t.get("import_tasks") for t in tasks("patroni/tasks/main.yml")]
gate_entry = next(t for t in tasks("patroni/tasks/main.yml") if t.get("import_tasks") == "data-dir-gate.yml")
check(order.index("data-dir-gate.yml") < order.index("07-patroni-systemd.yml") and "when" not in gate_entry
      and "always" in gate_entry.get("tags", []), "data-dir gate runs on every host before the distro unit is masked")
check(order.index("05-patroni-install.yml") < order.index("data-dir-gate.yml"),
      "data-dir gate runs after the package install, so a cluster the install created is refused")

# patroni: the mask task repeats the gate itself, since --start-at-task skips always-tagged tasks.
mask = next(t for t in tasks("patroni/tasks/07-patroni-systemd.yml") if t.get("name") == "mask the distro postgresql cluster unit")
mask_env = jinja2.Environment()
mask_env.filters["quote"] = shlex.quote
mask_env.filters["bool"] = lambda v: v if isinstance(v, bool) else str(v).lower() in ("1", "true", "yes")
for files, active, host, method, adopt, want in (
        ((), False, "db-02", "initdb", False, True), (("PG_VERSION",), False, "db-02", "initdb", False, False),
        (("PG_VERSION", "patroni.dynamic.json"), False, "db-02", "initdb", False, True),
        (("PG_VERSION",), True, "db-02", "initdb", False, True),
        (("PG_VERSION",), False, "db-01", "pgbackrest", True, True)):
    with tempfile.TemporaryDirectory() as d:
        os.mkdir(f"{d}/data")
        for f in files:
            open(f"{d}/data/{f}", "w").close()
        with open(f"{d}/systemctl", "w") as f:
            f.write(f'#!/bin/sh\necho "$*" >> {d}/calls.log\ncase "$*" in is-active*) exit {0 if active else 3} ;; esac\n')
        os.chmod(f"{d}/systemctl", 0o755)
        body = mask.get("shell", "")
        script = mask_env.from_string(body).render(
            patroni_data_dir=f"{d}/data", patroni_postgres_version=17, inventory_hostname=host,
            groups={"postgres_cluster": ["db-01", "db-02", "db-03"]}, patroni_bootstrap_method=method,
            patroni_adopt_existing_data_dir=adopt)
        rc = subprocess.run(["bash", "-c", script], env={**os.environ, "PATH": f"{d}:{os.environ['PATH']}"},
                            stdin=subprocess.DEVNULL, capture_output=True).returncode if body else 1
        masked = os.path.exists(f"{d}/calls.log") and "mask --now" in open(f"{d}/calls.log").read()
    check(bool(body) and (rc == 0 and masked) == want and (rc != 0 and not masked) == (not want),
          f"07 mask with {list(files)} patroni-active={active} {host} {method} adopt={adopt} -> masks={want}")

# Ansible's undefined chains through attribute access, so `a.json.state | default('')` renders.
ansible_env = jinja2.Environment(undefined=jinja2.ChainableUndefined)
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
             patroni_bootstrap_method=method, patroni_adopt_existing_data_dir=adopt,
             _patroni_unit_active={"rc": 3}, _patroni_member_state={"status": -1})
    want = pgv and not marker and not (host == "db-01" and method == "pgbackrest" and adopt)
    check(fires(refuse, **v) == want, f"data-dir gate on {host} marker={marker} PG_VERSION={pgv} {method} adopt={adopt} -> refuse={want}")
    check(fires(adopt_ok, **v) == (not adopt or method == "pgbackrest"), f"adopt={adopt} accepted only with pgbackrest ({method})")

# patroni: a re-run during a replica's first base backup (PG_VERSION, no marker yet) passes.
for active, state, want in ((0, "", False), (3, "creating replica", False), (3, "stopped", True), (3, None, True)):
    v = dict(inventory_hostname="db-02", groups={"postgres_cluster": ["db-01", "db-02", "db-03"]},
             _patroni_initialized={"stat": {"exists": False}}, _pg_data_present={"stat": {"exists": True}},
             patroni_bootstrap_method="initdb", patroni_adopt_existing_data_dir=False,
             _patroni_unit_active={"rc": active},
             _patroni_member_state={"status": 200, "json": {"state": state}} if state is not None else {"status": -1})
    check(fires(refuse, **v) == want, f"data-dir gate with patroni unit rc={active} /patroni state={state!r} -> refuse={want}")
join_confirm = next(t for t in tasks("patroni/tasks/09-patroni-join.yml") if t["name"] == "confirm we joined as a replica")
check("patroni_join_retries" in str(join_confirm.get("retries"))
      and yaml.safe_load(text("patroni/defaults/main.yml")).get("patroni_join_retries") == 180,
      "replica join retries are patroni_join_retries, default 180")

# patroni: --target-action is valid only with a recovery target (pgBackRest error 031).
restore = next(t for blk in boot if "block" in blk for t in blk["block"] if t["name"] == "bootstrap via pgBackRest restore")
ansible_env.filters["quote"] = shlex.quote
for target, want in (("latest", False), ("immediate", True), ("time:2026-04-30 14:00:00", True)):
    cmd = ansible_env.from_string(restore["command"]).render(patroni_pgbackrest_stanza="s", patroni_pgbackrest_restore_target=target)
    check(("--target-action=promote" in cmd) == want, f"pgbackrest restore for {target} has target-action={want}")

# patroni: a replica join is confirmed by the member's own /replica check, retried through the base backup.
join_tasks = {t["name"]: t for t in tasks("patroni/tasks/09-patroni-join.yml")}
confirm = join_tasks["confirm we joined as a replica"]
check(confirm.get("uri", {}).get("url", "").endswith("/replica") and "_replica_check" in str(confirm.get("until")),
      "replica join asserts /replica answers 200")
# A member that joined before may be the leader now (/replica answers 503 there).
rejoin = join_tasks.get("confirm a member that joined before is running in either role", {})
for initialized in (False, True):
    v = dict(_patroni_initialized={"stat": {"exists": initialized}})
    check(fires(confirm.get("when", "true"), **v) == (not initialized) and fires(rejoin.get("when", "false"), **v) == initialized
          and rejoin.get("uri", {}).get("url", "").endswith("/health"),
          f"join check (patroni data dir already initialized={initialized}) uses "
          f"{'/health, which a leader passes' if initialized else '/replica'}")

# patroni/etcd: "new" on an empty data dir is refused beside a live quorum or on a partial play.
pdefaults = yaml.safe_load(text("patroni/defaults/main.yml"))
check(pdefaults["etcd_initial_cluster_state"] == "new" and pdefaults.get("etcd_allow_new_bootstrap") is False,
      "etcd initial cluster state defaults to new; the new-bootstrap override defaults to false")
etcd_conf = jinja2.Environment(undefined=jinja2.ChainableUndefined).from_string(text("patroni/templates/etcd.conf.j2"))


def render_etcd(member_dir, state, started=None):
    v = {**pdefaults, "inventory_hostname": "db-02", "ansible_host": "10.0.0.12",
         "groups": {"postgres_cluster": ["db-01", "db-02"]},
         "hostvars": {"db-01": {"ansible_host": "10.0.0.11"}, "db-02": {"ansible_host": "10.0.0.12"}},
         "etcd_initial_cluster_state": state, "_etcd_member_dir": {"stat": {"exists": member_dir}}}
    if started is not None:
        v["_etcd_started_state"] = started
    return etcd_conf.render(**v)


for member_dir, state, started, want in ((False, "new", None, "new"), (False, "existing", None, "existing"),
                                         (True, "new", "new", "new"), (True, "new", "existing", "existing"),
                                         (True, "existing", "new", "new"), (True, "new", None, "existing")):
    check(f'ETCD_INITIAL_CLUSTER_STATE="{want}"' in render_etcd(member_dir, state, started),
          f"etcd.conf renders initial state {want} for member dir={member_dir} variable={state} started with={started}")
# The second converge of a new cluster must not change the file (a change restarts etcd on every member).
first = render_etcd(False, "new")
started = re.search(r'^ETCD_INITIAL_CLUSTER_STATE="(\w+)"', first, re.M).group(1)
check(render_etcd(True, "new", started) == first, "etcd.conf renders byte-identical on the run after etcd first started")
started_task = next((t for t in tasks("patroni/tasks/03-etcd-configure.yml")
                     if "_etcd_started_state" in str(t.get("set_fact", {}))), None)
check(started_task is not None and "ETCD_INITIAL_CLUSTER_STATE" in str(started_task["set_fact"]),
      "03 reads the started initial cluster state back from /etc/default/etcd")
unit = code("patroni/templates/patroni.service.j2")
check(not re.search(r'^(Requires|BindsTo|PartOf|Requisite)=.*\betcd\b', unit, re.M) and re.search(r'^Wants=.*\betcd\.service', unit, re.M),
      "patroni.service only Wants= etcd, so an etcd restart does not restart Patroni")
check(re.search(r'^ETCD_AUTO_COMPACTION_RETENTION="(?!0")[^"]+"', code("patroni/templates/etcd.conf.j2"), re.M) is not None,
      "etcd auto-compaction is enabled")
etcd_tasks = {t.get("name"): t for t in tasks("patroni/tasks/03-etcd-configure.yml")}
etcd_refuse = etcd_tasks["refuse to bootstrap a new etcd cluster beside a live one"]["when"]
etcd_unasked = etcd_tasks.get("refuse to bootstrap a new etcd cluster without knowing whether one is live", {}).get("when")
group = ["db-01", "db-02", "db-03"]
ansible_env.filters["difference"] = lambda a, b: [x for x in a if x not in b]
for member, state, quorum, play, allow, want in (
        (False, "new", 1, group, False, False), (False, "new", 0, group, False, True),
        (False, "new", 1, ["db-02"], False, True), (True, "new", 0, ["db-02"], False, False),
        (False, "existing", 0, ["db-02"], False, False),
        (False, "new", 0, group, True, False), (False, "new", 1, ["db-02"], True, False)):
    v = dict(_etcd_member_dir={"stat": {"exists": member}}, etcd_initial_cluster_state=state,
             _etcd_peer_quorum={"rc": quorum}, groups={"postgres_cluster": group}, ansible_play_hosts_all=play,
             etcd_allow_new_bootstrap=allow)
    check(fires(etcd_refuse, **v) == want,
          f"etcd refuse (member dir={member} state={state} peer quorum rc={quorum} play={play} allow={allow}) -> {want}")
for member, quorum, allow, want in ((False, 2, False, True), (False, 2, True, False), (False, 1, False, False),
                                    (True, 2, False, False)):
    v = dict(_etcd_member_dir={"stat": {"exists": member}}, etcd_initial_cluster_state="new",
             _etcd_peer_quorum={"rc": quorum}, etcd_allow_new_bootstrap=allow)
    check(etcd_unasked is not None and fires(etcd_unasked, **v) == want,
          f"etcd could-not-ask refusal (member dir={member} probe rc={quorum} allow={allow}) -> {want}")
refuse_msg = etcd_tasks["refuse to bootstrap a new etcd cluster beside a live one"]["fail"]["msg"]
check("half-done first bootstrap" in refuse_msg and "-e etcd_allow_new_bootstrap=true" in refuse_msg,
      "etcd refusal names the half-done first bootstrap way through")

# etcd peer probe: a peer that could not be asked (TLS, firewall) is not "no quorum".
probe = etcd_tasks["ask the other members whether an etcd cluster already has quorum"]["shell"]["cmd"]
probe_sh = ansible_env.from_string(probe).render(
    groups={"postgres_cluster": group}, inventory_hostname="db-01", etcd_tls_enabled=False, etcd_client_port=2379,
    hostvars={"db-02": {"ansible_host": "10.0.0.12"}, "db-03": {"ansible_host": "10.0.0.13"}})
peer_cases = {"healthy": 'exit 0', "no-quorum": 'case "$*" in *health*) echo "unhealthy: context deadline exceeded" >&2; exit 1 ;; esac; exit 0',
              "refused": 'echo "dial tcp 10.0.0.1:2379: connect: connection refused" >&2; exit 1',
              "tls": 'echo "transport: authentication handshake failed: x509: certificate signed by unknown authority" >&2; exit 1'}
for p2, p3, want in (("refused", "healthy", 0), ("no-quorum", "refused", 1), ("refused", "refused", 1),
                     ("tls", "refused", 2), ("refused", "tls", 2), ("tls", "healthy", 0)):
    with tempfile.TemporaryDirectory() as d:
        with open(f"{d}/etcdctl", "w") as f:
            f.write("#!/bin/bash\ncase \"$*\" in\n"
                    f"  *10.0.0.12*) {peer_cases[p2]} ;;\n  *10.0.0.13*) {peer_cases[p3]} ;;\nesac\n")
        os.chmod(f"{d}/etcdctl", 0o755)
        rc = subprocess.run(["bash", "-c", probe_sh], env={**os.environ, "PATH": f"{d}:{os.environ['PATH']}"},
                            stdin=subprocess.DEVNULL, capture_output=True).returncode
    check(rc == want, f"etcd peer probe (db-02 {p2}, db-03 {p3}) -> rc {want} (got {rc})")

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

# patroni: a refused or failed restart is re-notified from the handler's marker on the next run.
requeue = next((t for t in tasks("patroni/tasks/06-patroni-configure.yml") if t.get("notify") == "patroni config changed"
                and "_patroni_restart_pending" in str(t.get("when"))), None)
marker = next((t for t in tasks("patroni/tasks/06-patroni-configure.yml") if t.get("register") == "_patroni_restart_pending"), {})
check(requeue is not None and requeue.get("changed_when") is True
      and marker.get("stat", {}).get("path") == "/etc/patroni/.restart-pending"
      and "pending=/etc/patroni/.restart-pending" in handlers["restart patroni"]["shell"],
      "06 re-notifies the patroni restart while the handler's pending marker exists")

# patroni: the handler's state vocabulary ("streaming") and `switchover --leader` need Patroni >= 3.0.4.
install = tasks("patroni/tasks/05-patroni-install.yml")
gate = next((t for t in install if "version('3.0.4', '>=')" in str(t.get("assert", {}))), None)
check(gate is not None and "always" in gate.get("tags", []), "patroni install refuses Patroni older than 3.0.4, on every tag")
scraper = code("patroni/tasks/11-monitoring.yml")
check('"$STATE" == "streaming"' in scraper and '"$ROLE" == "sync_standby"' in scraper,
      "patroni textfile scraper counts a 3.x streaming replica and a sync standby as running replicas")

# patroni: a pgBackRest restore outside Patroni loses its recovery settings, so it is refused.
boot = tasks("patroni/tasks/08-patroni-bootstrap.yml")
refuse_restore = next((t for t in boot[0]["block"] if "fail" in t), {"when": "false"})
for method, initialized, want in (("pgbackrest", False, True), ("pgbackrest", True, False), ("initdb", False, False)):
    v = dict(patroni_bootstrap_method=method, _patroni_initialized={"stat": {"exists": initialized}})
    check(fires(refuse_restore["when"], **v) == want and boot[0]["block"][0] is refuse_restore,
          f"bootstrap refuses an out-of-Patroni restore (method={method} initialized={initialized}) -> {want}")

# promtail: a finite retry budget drops a batch while Loki is down, after the journal position moved on.
promtail = yaml.safe_load(jinja2.Environment(undefined=jinja2.ChainableUndefined).from_string(
    text("loki/templates/promtail-config.yaml.j2")).render(promtail_target_url="http://10.0.2.5:3100/loki/api/v1/push"))
check(all(c.get("backoff_config", {}).get("max_retries") == 0 for c in promtail["clients"]),
      "promtail retries a batch until Loki accepts it (max_retries 0)")

# firewalls: every role that renders the whole /etc/nftables.conf refuses an archival node,
# whose own role renders the same file.
for role in sorted(os.listdir(roles)):
    tdir = f"{roles}/{role}/tasks"
    if role == "archival-node" or not os.path.isdir(tdir):
        continue
    files = [f for f in os.listdir(tdir) if f.endswith(".yml")]
    if not any("/etc/nftables.conf" in open(f"{tdir}/{f}").read() for f in files):
        continue
    guarded = any("archival_nodes" in str(t.get("assert", {}))
                  for f in files for t in (yaml.safe_load(open(f"{tdir}/{f}")) or []) if isinstance(t, dict))
    check(guarded, f"{role} renders /etc/nftables.conf and refuses hosts in archival_nodes")

if failed:
    sys.exit(1)
print("ansible-ha-roles-test: PASS")
PYEOF
