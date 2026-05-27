# J05 — Operator recovers r1 from F-0001/F-0039 cascade

## Inputs

- Operator logs into r1 (`ssh root@136.243.90.96`)
- System state at start: F-0001 (`/dev/md1 49G 47G 0 100%`),
  F-0039 (Redis MISCONF), cascade running for 20+ hours per
  F-0109/F-0116. Findings live in
  `docs/audit-2026-05-26/05-findings-register.md`.

## Pre-flight (read-only)

Confirm state before mutating:
```sh
df -h /
redis-cli ping                        # expect: MISCONF error
redis-cli info persistence | grep -E "rdb_changes_since_last_save|rdb_last_bgsave_status|rdb_last_bgsave_time_sec"
journalctl -u ratesengine-aggregator -n 50 --no-pager | grep -i "redis\|misconf"  # confirm aggregator is logging cache-write errors
```

## Hops (the 8-step cascade-fix sequence from 07-remediation-plan.md)

### Step 1: F-0001 — free root disk

Per the 2026-05-10 SEV-2 post-mortem's documented recovery:
```sh
# top consumers
du -h --max-depth=1 /var/log /var/lib /tmp 2>/dev/null | sort -h | tail -20

# vacuum journal (typically frees several GB)
journalctl --vacuum-size=200M

# rotate stuck postgres log per F-0006/F-0022
# verify ownership/group first
ls -la /var/log/postgresql/postgresql-*-main.log
# if still 11G+ as audit-time:
sudo -u postgres pg_ctl logrotate -D /var/lib/postgresql/15/main   # safe live rotation
# or, if logrotate misconfigured per F-0006:
truncate -s 0 /var/log/postgresql/postgresql-15-main.log

# clear stuck WASM-audit stderr captures per F-0010
rm /var/log/wasm-history-*.stderr 2>/dev/null

# sweep ratesengine binary logs per F-0009
journalctl --vacuum-time=7d

df -h /         # target: <85% used
```

Pass condition: `df -h /` reports `Use% < 85%`.

### Step 2: trigger Redis bgsave + clear MISCONF

```sh
redis-cli config set stop-writes-on-bgsave-error no   # unlock writes temporarily
redis-cli bgsave                                       # force snapshot
sleep 5
redis-cli info persistence | grep last_bgsave_status   # expect: ok
redis-cli config set stop-writes-on-bgsave-error yes   # restore safety
redis-cli ping                                         # expect: PONG
```

Pass condition: `redis-cli ping` returns `PONG`.

### Step 3: restart down exporters (F-0045..F-0048)

```sh
systemctl restart redis_exporter prometheus-postgres-exporter pgbackrest_exporter
# verify
curl -s localhost:9090/api/v1/targets | jq '.data.activeTargets[] | select(.health == "down") | .labels.job'
# expect empty output
```

Address MinIO 403 (F-0048): check that `/etc/prometheus/prometheus.r1.yml` scrape job for minio uses a valid bearer token AND that the minio policy grants `metrics` access.

### Step 4: F-0080 — guard aggregator_silent alert

```sh
# Edit configs/prometheus/rules.r1/aggregator.yml on r1
# Replace:
#   expr: sum(rate(ratesengine_aggregator_vwap_writes_total[5m])) == 0
# With (per F-0081 ingestion.yml template):
#   expr: |
#     sum(rate(ratesengine_aggregator_vwap_writes_total[5m])) == 0
#     OR
#     absent_over_time(ratesengine_aggregator_vwap_writes_total[10m]) == 1

promtool check rules /etc/prometheus/rules.r1/aggregator.yml
# expect: SUCCESS

curl -X POST http://localhost:9090/-/reload   # hot-reload Prometheus
```

### Step 5: F-0085 — add exporter-down meta-alert

Add to `configs/prometheus/rules.r1/meta.yml`:
```yaml
- alert: ratesengine_redis_exporter_down
  expr: up{job="redis_exporter"} == 0 OR absent_over_time(up{job="redis_exporter"}[5m]) == 1
  for: 2m
  labels:
    severity: page
  annotations:
    summary: "redis_exporter is down — F-0039 cascade-blind"
    runbook_url: https://github.com/RatesEngine/rates-engine/blob/main/docs/operations/runbooks/exporter-down.md
```

Add equivalent rules for postgres_exporter, pgbackrest_exporter,
minio. `promtool check rules` + Prometheus reload.

### Step 6: F-0049 + F-0050 — flip fail-OPEN to fail-CLOSED

Code change (not operator):
- `internal/auth/signup_ip_throttle.go:75-79` — replace
  fail-open path with `return SignupRateLimitErr`
- `internal/ratelimit/bucket.go:138-190` — same
- Add tests asserting 503 + Retry-After under Redis-error

Land via PR; deploy via `gh workflow run deploy.yml -f region=r1 -f version=rc.82-rate-limit-failclosed`.

### Step 7: F-0086/F-0087/F-0089/F-0090 — translate Redis MISCONF to HTTP 503

Code change:
- `internal/api/v1/oracle.go`, `lending.go`, `vwap.go` —
  catch `errors.Is(err, redis.ErrMISCONF)` (or equivalent),
  return 503 + `Retry-After: 30` via `writeProblem`
- Document the contract in `openapi/rates-engine.v1.yaml`

### Step 8: F-0055 — `/v1/status` self-consistency

Code change:
- `internal/api/v1/status.go` — if every signal is `unknown`
  + zero-time, set `overall: "degraded"` (NOT `"ok"`)

### Step 9: backfill steady-state recovery

Once cache is rebuilding:
```sh
# Force aggregator to re-warm all default-window keys
systemctl restart ratesengine-aggregator
sleep 60
# verify
redis-cli --scan --pattern "vwap:*" | head -20
# expect: keys present with TTLs in [300, 86400]
```

## Sinks

- Root disk → ≤85% used
- Redis → PONG; `rdb_changes_since_last_save < 100`
- All 4 exporters → up
- All cascade-fragile alerts → guarded + reloaded
- /v1/price returning fresh (non-stale) responses on default pairs
- /v1/oracle/* + /v1/vwap + /v1/lending/pools returning data (not 500)

## Failure modes

| Step | Failure | Recovery |
| --- | --- | --- |
| 1 | journalctl --vacuum errors out | use `journalctl --vacuum-time=2d` instead; if still stuck, identify largest journal namespace via `journalctl --disk-usage` then vacuum per-unit |
| 1 | postgres log truncate fails on permission | run via `sudo -u postgres pg_ctl logrotate -D /var/lib/postgresql/15/main` |
| 2 | bgsave still fails after disk-free | check `dmesg` for IO errors; verify `redis-server.service` user has write perms on `/var/lib/redis` |
| 3 | exporter unit isn't found | check actual unit name: Debian uses `redis-server.service`, `prometheus-redis-exporter.service`. Names per distro differ. |
| 4 | promtool check rules fails | aggregator.yml syntax error; revert before reload |
| 6+7 | code change needs PR + CI + deploy | this is multi-step — operator should plan ~2h with verify.sh green + R1 deploy + post-deploy probe |

## Tests

- Validate end-state via `bash scripts/dev/r1-smoke.sh`
  (13 GETs across health / catalogue / pricing / diagnostics)
- Specifically:
  - `/v1/healthz` → 200 ok
  - `/v1/readyz` → 200 ok (NOT degraded)
  - `/v1/price?asset=native&quote=fiat:USD` → `flags.stale:false`
  - `/v1/oracle/latest?asset=native` → 200 (not 500)
  - `/v1/diagnostics/ingestion` → fresh values matching `/v1/network/stats`

## Closure rule

The cascade is "fully recovered" when:
- `bash scripts/dev/r1-smoke.sh` exits 0
- `/v1/readyz` reports `status: ok`
- F-0001/F-0039/F-0027/F-0049/F-0050/F-0055/F-0086/F-0087/F-0089
  rows in `05-findings-register.md` updated to `closed-by-PR-####`
- Post-fix R1 probe captured at `evidence/r1-probes/r1-pXX-2026-05-XX.md`
- A new EV-#### row links the closing PR to the audit register

**Cross-references:**
- 07-remediation-plan.md "Cascade-fix sequence (Wave 0 ordering)"
- F-0099 (the 2026-05-10 SEV-2 post-mortem this recovery
  effectively re-walks)
- EXECUTIVE-SUMMARY.md "The 8-step cascade-fix sequence"
