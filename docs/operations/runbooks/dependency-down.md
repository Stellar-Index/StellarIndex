---
title: Runbook — API dependency down
last_verified: 2026-09-01
status: ratified
severity: P1
---

# Runbook — `stellarindex_dependency_down`

`stellarindex_dependency_up{dependency="..."} == 0` for 2m (P1, page): the API's own `/v1/readyz` check for one dependency is failing. Rule: `configs/prometheus/rules.r1/storage.yml` and `deploy/monitoring/rules/storage.yml` (group `stellarindex.storage`).

| dependency | other coverage | treat this alert as |
|---|---|---|
| `clickhouse` | none (no exporter on r1) | the primary signal |
| `postgres` | `postgres_exporter` (`pg_up`) | corroboration |
| `redis` | `redis_exporter` | corroboration |
| `schema`, `nonstandard_decimals`, `closed_buckets` | none | the primary signal |

## At a glance

P1 page, `for: 2m`. MTTR minutes when a restart fixes it; longer for disk pressure or a partial migration. Impact depends on the dependency (table above).

## If `dependency="clickhouse"` — read this first

ClickHouse is the raw lake (ADR-0034). Ingest writes and projector reads stop, but the served API keeps working from Postgres, so customers see nothing while the archive silently stops advancing.

**Check.**

```sh
# 1. Is the service up at all?
systemctl status clickhouse-server --no-pager | head -20

# 2. Does it answer?
clickhouse-client --query "SELECT 1"

# 3. Is it out of disk? (the most common cause on a single box)
df -h /var/lib/clickhouse
zfs list -o name,used,avail 2>/dev/null | head

# 4. What did it say when it stopped?
journalctl -u clickhouse-server --since "30 min ago" --no-pager | tail -40
```

**Likely causes.**

- Disk pressure (ClickHouse refuses writes well before the filesystem is full).
- A heavy job took it down; one-shots must run under `/usr/local/sbin/run-heavy-job.sh` (MemoryMax=20G), see [maintainer-workflow.md](../maintainer-workflow.md).
- OOM: `journalctl -k | grep -i oom` (strict overcommit usually surfaces as ENOMEM in the process log).

**Fix.**

1. Restart and confirm the API agrees, not just systemd:

   ```sh
   systemctl restart clickhouse-server
   curl -s localhost:3000/v1/readyz | python3 -m json.tool | grep -A2 clickhouse
   ```
2. `SELECT 1` works but readyz says down with "boot retry window": the API never wired its lake readers. `systemctl restart stellarindex-api`.
3. Ingest resumes from its cursor. For a longer gap verify with the gap detector and catch up projected sources with `projector-replay` (not `backfill`, which writes nothing on gated sources and exits 0); see [adr-0033-data-recovery.md](../adr-0033-data-recovery.md).

**Escalate:** page per [sev-playbook](../sev-playbook.md).

## If `dependency="postgres"` or `"redis"`

The exporter alerts (`stellarindex_timescale_primary_down`, the redis rules) fire alongside and carry more detail: follow those runbooks. This alert alone suggests a problem between the API and the dependency (credentials, pool, network path).

## If `dependency="schema"`

The binary's expected migration state differs from the database (binary deployed without its migration, or a partial `stellarindex-migrate`). Compare the deployed version with `schema_migrations` before restarting anything; a restart does not fix a missing migration.

## If `dependency="nonstandard_decimals"`

The API's `nonstandard_decimals_assets` cache has never loaded since process start (startup load failed, no 60 s refresh has succeeded). Until then every confirmed non-7-decimal asset serves its raw, power-of-ten-skewed price (see [`dex.md#dex-nonstandard-decimals`](dex.md#dex-nonstandard-decimals)). Look for `nonstandard-decimals cache initial refresh failed` in the API log and `stellarindex_nonstandard_decimals_cache_refresh_failures_total` rising. A failure after a successful load keeps the last-good snapshot and does not turn this red.

## If `dependency="closed_buckets"`

A continuous aggregate outside the real-time allowlist (`source_volume_1h`, `pools_per_source_1h`) has `timescaledb.materialized_only = false`, so readers without their own closed-bucket predicate serve a partial bucket. The readyz body names the views. Migration 0172 pins the property, so this means an out-of-band `ALTER`: restore each named view with
`ALTER MATERIALIZED VIEW <view> SET (timescaledb.materialized_only = true);`
(rewrites the view definition only; nothing re-materializes).

## False positives and self-check

- During a deploy the check briefly fails; `for: 2m` rides it out.
- The alert is `== 0`, not `absent()`, so a renamed check cannot look like an outage.

```sh
curl -s localhost:3000/metrics | grep stellarindex_dependency_up
```

Every configured check should appear, healthy at `1`. A dependency missing entirely is a publisher bug (`internal/api/v1/dependency_up_test.go`).

## Related

- [`postgres.md#stellarindex_timescale_primary_down`](postgres.md#stellarindex_timescale_primary_down)
- [`../maintainer-workflow.md`](../maintainer-workflow.md), [`../adr-0033-data-recovery.md`](../adr-0033-data-recovery.md), [`../alerts-catalog.md`](../alerts-catalog.md)
