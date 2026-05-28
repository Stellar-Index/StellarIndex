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
# Pre-deploy migration check (added 2026-05-28).
#
# deploy.yml syncs binaries only — Postgres migrations are operator-
# manual per feedback_migrations_not_auto_deployed. A binary release
# that adds a column / table will crash on its first DB write if the
# matching migration hasn't been applied. Compare local
# migrations/NNNN_*.up.sql versus the schema_migrations table on r1.
# Pending = local has it, r1 doesn't.
LOCAL_LATEST_MIG=$(ls migrations/[0-9]*_*.up.sql 2>/dev/null | sed -E 's|migrations/0*([0-9]+)_.*|\1|' | sort -n | tail -1)
R1_LATEST_MIG=$(ssh -o ConnectTimeout=5 "$R1_HOST" "sudo -u postgres psql -tA -d ratesengine -c 'SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1;' 2>/dev/null" | tr -d '[:space:]')
if [ -n "$LOCAL_LATEST_MIG" ] && [ -n "$R1_LATEST_MIG" ]; then
  if [ "$LOCAL_LATEST_MIG" -gt "$R1_LATEST_MIG" ]; then
    pending=$((LOCAL_LATEST_MIG - R1_LATEST_MIG))
    echo "DRIFT: migrations — local latest is $LOCAL_LATEST_MIG, r1 schema_migrations.version is $R1_LATEST_MIG ($pending pending)"
    echo "       scp migrations/00*.sql root@r1:/tmp/ && ssh root@r1 'cd /tmp && ratesengine-migrate up -database \$RE_PG_DSN -path .'"
    FAILS=$((FAILS + 1))
  fi
else
  echo "WARN: migration check skipped — local latest='$LOCAL_LATEST_MIG' r1 latest='$R1_LATEST_MIG'"
fi

if [ "$FAILS" -gt 0 ]; then
  echo "FAIL: $FAILS drift(s) on r1"
  exit "$FAILS"
fi
echo "OK: all tracked files + migrations in sync"
