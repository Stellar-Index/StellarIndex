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

echo "lint-deploy-probe-parity-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
