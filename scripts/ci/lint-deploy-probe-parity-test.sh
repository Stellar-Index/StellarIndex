#!/usr/bin/env bash
# lint-deploy-probe-parity-test.sh — regression test for GH-1167: the
# deploy gate's API health probe must use the readiness probe HAProxy
# routes on (`/v1/readyz`), not the constant-200 liveness probe
# (`/v1/healthz`) — a binary readyz would refuse used to pass the deploy
# gate and then get drained by HAProxy the moment it served. Caddy stays
# on /v1/healthz on purpose: it fronts one upstream, so a readyz-driven
# drain would be a full outage.
#
# Run: bash scripts/ci/lint-deploy-probe-parity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
pass=0
fail=0

check() { # check <desc> <ok>
  if [ "$2" -eq 0 ]; then
    echo "  ok   $1"
    pass=$((pass + 1))
  else
    echo "  FAIL $1"
    fail=$((fail + 1))
  fi
}

deploy_path="$(python3 - <<'PY'
import yaml
plays = yaml.safe_load(open("configs/ansible/playbooks/deploy-binary.yml"))
print(plays[0].get("vars", {}).get("api_health_path", ""))
PY
)"
case "$deploy_path" in
  /v1/readyz) check "deploy-binary.yml's api_health_path is /v1/readyz" 0 ;;
  *) check "deploy-binary.yml's api_health_path is /v1/readyz (got '${deploy_path}')" 1 ;;
esac

# Every other daemon is probed on its /readyz (schema-head check), never
# `systemctl is-active`, which reads "active" for a process failing every
# write. Each non-CLI, non-API binary the deploy defaults to must carry a
# probe port, and the port must be the one the archival-node role scrapes.
daemon_problems="$(python3 - <<'PY'
import yaml
play = yaml.safe_load(open("configs/ansible/playbooks/deploy-binary.yml"))[0]["vars"]
ports = play.get("daemon_ready_ports") or {}
cli = set(play.get("cli_binaries") or [])
wf = yaml.safe_load(open(".github/workflows/deploy.yml"))
trigger = wf.get("on", wf.get(True))
default = trigger["workflow_dispatch"]["inputs"]["binaries"]["default"]
daemons = [b.strip() for b in default.split(",") if b.strip() not in cli and b.strip() != "stellarindex-api"]
role = yaml.safe_load(open("configs/ansible/roles/archival-node/defaults/main.yml"))
want = {
    "stellarindex-indexer": role.get("local_prometheus_indexer_port"),
    "stellarindex-aggregator": role.get("local_prometheus_aggregator_port"),
}
problems = []
if not daemons:
    problems.append("no daemon found in deploy.yml's default binaries list")
for d in daemons:
    if d not in ports:
        problems.append(f"{d} has no daemon_ready_ports entry")
    elif d in want and ports[d] != want[d]:
        problems.append(f"{d} probes port {ports[d]}, archival-node scrapes {want[d]}")
tasks = open("configs/ansible/tasks/deploy-one-binary.yml").read()
if 'cmd: "systemctl is-active' in tasks:
    problems.append("deploy-one-binary.yml still probes with systemctl is-active")
if "daemon_ready_ports[binary] }}/readyz" not in tasks:
    problems.append("deploy-one-binary.yml has no daemon /readyz probe")
print("\n".join(problems))
PY
)"
if [ -z "$daemon_problems" ]; then
  check "every deployed daemon is probed on its /readyz port" 0
else
  check "every deployed daemon is probed on its /readyz port: ${daemon_problems}" 1
fi

echo "lint-deploy-probe-parity-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
