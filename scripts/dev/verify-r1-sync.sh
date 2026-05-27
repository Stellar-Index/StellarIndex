#!/usr/bin/env bash
# verify-r1-sync.sh — md5-compare every tracked config path against r1.
# Wave-1 follow-up to the 2026-05-26 audit's drift cluster (F-0133..F-0142).
# Fails noisily on any mismatch; operator runs BEFORE deploy.yml to catch
# drift early.

set -uo pipefail
R1_HOST="${R1_HOST:-root@136.243.90.96}"
PAIRS=(
  "configs/caddy/Caddyfile.api:/etc/caddy/Caddyfile"
  "configs/prometheus/prometheus.r1.yml:/etc/prometheus/prometheus.yml"
  "configs/alertmanager/alertmanager.r1.yml:/etc/prometheus/alertmanager.yml"
  "scripts/dev/r1-smoke.sh:/opt/ratesengine/healthchecks/r1-smoke.sh"
  "configs/healthchecks/smoke.sh:/opt/ratesengine/healthchecks/smoke.sh"
  "configs/healthchecks/heartbeat.sh:/opt/ratesengine/healthchecks/heartbeat.sh"
  "configs/healthchecks/sla-probe.sh:/opt/ratesengine/healthchecks/sla-probe.sh"
)
FAILS=0
for pair in "${PAIRS[@]}"; do
  local_path="${pair%%:*}"
  remote_path="${pair##*:}"
  local_md5=$(md5 -q "$local_path" 2>/dev/null || md5sum "$local_path" 2>/dev/null | awk '{print $1}')
  remote_md5=$(ssh -o ConnectTimeout=5 "$R1_HOST" "md5sum '$remote_path' 2>/dev/null | awk '{print \$1}'")
  if [ "$local_md5" != "$remote_md5" ]; then
    echo "DRIFT: $local_path ($local_md5) != $remote_path ($remote_md5)"
    FAILS=$((FAILS + 1))
  fi
done
# Also compare every file in configs/prometheus/rules.r1/ → /etc/prometheus/rules.r1/
for f in configs/prometheus/rules.r1/*.yml; do
  name=$(basename "$f")
  local_md5=$(md5 -q "$f" 2>/dev/null || md5sum "$f" 2>/dev/null | awk '{print $1}')
  remote_md5=$(ssh -o ConnectTimeout=5 "$R1_HOST" "md5sum '/etc/prometheus/rules.r1/$name' 2>/dev/null | awk '{print \$1}'")
  if [ "$local_md5" != "$remote_md5" ]; then
    echo "DRIFT: rules.r1/$name"
    FAILS=$((FAILS + 1))
  fi
done
if [ "$FAILS" -gt 0 ]; then
  echo "FAIL: $FAILS file(s) drifted on r1"
  exit "$FAILS"
fi
echo "OK: all tracked files in sync"
