---
title: Runbook — API dependency down
last_verified: 2026-09-01
status: ratified
severity: P1
---

# Runbook — `stellarindex_dependency_down`

The API's own `/v1/readyz` check for one of its dependencies has been
failing for 2+ minutes. The check already ran on every readiness round;
what is new (#371) is that its result is published as
`stellarindex_dependency_up{dependency="…"}`, so it can be alerted on.

**Which dependency fired matters a lot, because they are not equally
well covered.**

| dependency | other coverage | treat this alert as |
|---|---|---|
| `clickhouse` | **none — no exporter on r1** | the primary signal |
| `postgres` | `postgres_exporter` (`pg_up`) | corroboration |
| `redis` | `redis_exporter` | corroboration |
| `schema` | none | the primary signal |

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_dependency_down` — `configs/prometheus/rules.r1/storage.yml` and `deploy/monitoring/rules/storage.yml` (group `stellarindex.storage`, `severity: page`, `for: 2m`) |
| Severity | **P1** |
| Detected by | `stellarindex_dependency_up{dependency="…"} == 0` — the API's own `/v1/readyz` check, published as a gauge (#371 F2). For `clickhouse` this is the ONLY signal; postgres and redis also have exporter-backed alerts that fire alongside. |
| Typical MTTR | Minutes when the dependency just needs restarting; longer if it is disk pressure or a partial migration. |
| Impact | Depends entirely on which dependency. `clickhouse`: ingest writes and projector reads stop, but the served API keeps working from the Postgres served tier — so customers may see nothing while the archive silently stops advancing. `postgres`: writes halt. `redis`: hot-path cache degrades to Timescale. `schema`: the binary and database disagree about migration state. |

## If `dependency="clickhouse"` — read this first

ClickHouse is the **raw lake** (ADR-0034). It is the substrate the
ADR-0033 completeness claim rests on: every ledger and event lands there
first, the projector reads forward events from `contract_events`, and
`stellarindex-ops` re-derives from it. It is also the one dependency on
r1 with **no Prometheus exporter of its own**, which is why this alert
exists — before it, ClickHouse disappearing had no single symptom, just
endpoints failing one at a time.

What is and is not affected:

- **Ingest writes stop.** The dual-sink indexer cannot write the lake.
- **Projector reads stop** (`storage.clickhouse_projector_source`
  defaults true).
- **The served API mostly keeps working.** Postgres is the served tier;
  `/v1/price`, `/v1/assets` and the rest read from it. So customers may
  see nothing wrong while the archive silently stops advancing — which
  is precisely the failure worth paging on.

### Triage

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

### Common causes, in the order they actually happen here

1. **Disk pressure.** The lake is the largest thing on the box. Check
   `df -h /var/lib/clickhouse` first; ClickHouse refuses writes well
   before the filesystem is full.
2. **A heavy job took it down.** Any one-shot must run under
   `/usr/local/sbin/run-heavy-job.sh` (MemoryMax=20G) — see
   [maintainer-workflow.md](../maintainer-workflow.md). An unwrapped
   re-derive is the 2026-07-05 class.
3. **OOM.** `journalctl -k | grep -i oom` — the host sets strict
   overcommit, so allocation failures usually surface as ENOMEM in the
   process log rather than a kernel OOM kill.

### Recovery

```sh
systemctl restart clickhouse-server
# then confirm the API agrees, not just systemd:
curl -s localhost:3000/v1/readyz | python3 -m json.tool | grep -A2 clickhouse
```

Ingest resumes from its cursor — no manual backfill is needed for a
restart-length outage. If the gap is long enough to matter, verify with
the gap detector rather than assuming, and catch up projected sources
with `projector-replay` (**not** `backfill` — on gated sources that
writes nothing and exits 0; see
[adr-0033-data-recovery.md](../adr-0033-data-recovery.md)).

## If `dependency="postgres"` or `"redis"`

The dedicated exporter alerts (`stellarindex_timescale_primary_down`,
the redis rules) fire alongside this one and carry more detail. Follow
those runbooks; this alert firing *without* them suggests the problem is
between the API and the dependency (credentials, connection pool, a
network path) rather than the dependency itself.

## If `dependency="schema"`

The schema check compares the migration state the binary expects against
what the database has. It fires after a deploy that shipped a binary
without its migration, or after a partial `stellarindex-migrate` run.
Check the deployed version against `schema_migrations` before restarting
anything — a restart does not fix a missing migration.

## False-positive shapes

- **During a deploy.** The API restarts and the check briefly fails. The
  `for: 2m` window is set to ride that out; a firing alert during a
  deploy window that clears itself needs no action.
- **Never on a renamed check.** The alert is `== 0`, not `absent()`,
  precisely so renaming or removing a check cannot look like an outage.

## Verifying the alert itself

```sh
curl -s localhost:3000/metrics | grep stellarindex_dependency_up
```

Every configured check should appear, healthy ones at `1`. A dependency
missing from that output entirely is a bug in the publisher, not a
healthy dependency — the metric is written for failing checks too, and
`internal/api/v1/dependency_up_test.go` pins exactly that.

## Related

- [`timescale-primary-down.md`](timescale-primary-down.md) — fires alongside
  this alert when `dependency="postgres"`, and carries the exporter detail.
- [`../maintainer-workflow.md`](../maintainer-workflow.md) — the heavy-job
  wrapper, whose absence is a recurring cause of ClickHouse pressure.
- [`../adr-0033-data-recovery.md`](../adr-0033-data-recovery.md) — catch-up
  after a long outage. Note the gated-source trap: `backfill` writes nothing
  and exits 0; use `projector-replay`.
- [`../alerts-catalog.md`](../alerts-catalog.md) — the full alert inventory.
- ADR-0034 (tiered ClickHouse architecture) — why the lake going away is
  invisible from the served surface.

## Changelog

- 2026-09-01 — created alongside `stellarindex_dependency_up` (#371 F2).
  Before this, ClickHouse had no health signal in either rule tree.
