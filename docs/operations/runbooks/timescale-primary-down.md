---
title: Runbook — timescale primary down
last_verified: 2026-08-28
status: ratified
severity: P1
---

# Runbook — `stellarindex_timescale_primary_down`

> **R1 DEPLOYMENT REALITY (re-verified 2026-08-28).** R1 runs a
> **single, unclustered Postgres 15 + TimescaleDB** as
> `postgresql@15-main.service`, managed by the Ansible `archival-node`
> role (`configs/ansible/roles/archival-node/tasks/05-postgres.yml`).
> There is **no Patroni, no etcd, no replica** — the HAProxy/Patroni
> design in `docs/architecture/ha-plan.md` §3.3 is a ratified DESIGN
> that no playbook invokes (see that doc's "DEPLOYMENT STATE" banner and
> `configs/ansible/roles/patroni/README.md` F-1266). Nothing fails over
> automatically; recovery is "restart the service" or "restore from
> pgBackRest". The rule comment in
> `configs/prometheus/rules.r1/storage.yml` (F-1329) says the same.
> The pre-2026-08-28 version of this runbook described the undeployed
> cluster — see the appendix at the bottom.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_primary_down` — `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: page`, `for: 30s`) |
| Severity | **P1** (SEV-1) |
| Detected by | Prometheus rule `pg_up == 0` from the `postgres_exporter` scrape job (`localhost:9187`, `configs/prometheus/prometheus.r1.yml`). Companions: `stellarindex_postgres_ping_failing` (page, `for: 2m`) within ~2–3 min; `stellarindex_api_price_stale` + `stellarindex_api_error_rate_critical` (`rules.r1/api.yml`) as served data goes stale. |
| Typical MTTR | **No automatic failover on r1.** MTTR is time-to-restart `postgresql@15-main.service` (minutes) when the data dir is intact, or a pgBackRest restore (hours — see [`backup-failed.md`](backup-failed.md) and [`../drills/restore-drills.md`](../drills/restore-drills.md)) when it is not. |
| Impact | Writes halt everywhere (trade ingestion, API-key mint, usage rollups). Reads: the Redis hot path keeps serving cached prices with `stale_flag=true`; ClickHouse-backed lake/explorer endpoints keep working; Timescale-backed endpoints 5xx and `/v1/readyz` returns 503. |

## Symptoms

- Alert `stellarindex_timescale_primary_down` fires — `pg_up == 0` for 30 s
  (`postgres_exporter` could not reach the server).
- `/v1/readyz` returns 503 with the `postgres` check `ok:false` (the
  `postgres` checker is `Critical()` — `cmd/stellarindex-api/main.go`).
- API write endpoints (`POST /v1/account/keys` etc.) error; reads from the
  Redis hot path still work with `stale_flag=true`.
- `stellarindex_postgres_ping_failing` (page) follows within ~2–3 min — the
  indexer's 60 s pool-ping probe starts logging errors.
- `stellarindex_ingestion_cursor_stuck` (**ticket**, `increase(...[5m]) == 0`
  with `for: 5m`, `rules.r1/ingestion.yml`) follows ~10 min later. It cannot
  fire within 60 s — do not wait for it as a confirmation signal.

## Quick diagnosis (≤ 5 min)

**Step 1 — start with `/v1/readyz`.** It's the fastest signal
that distinguishes "API hitting a real DB problem" from
"Prometheus scrape blip". `checks` is an **array** of
`{name, ok, error}` (`internal/api/v1/server.go` `checkResult`):

```sh
curl -sS https://api.stellarindex.io/v1/readyz | jq '.checks[] | select(.name=="postgres")'
# Expect: {"name":"postgres","ok":true} when DB is reachable.
# {"name":"postgres","ok":false,"error":"dial tcp ... connection refused"} → real DB outage.
# (drop -f: on outage readyz returns 503 and -f would hide the body)
```

The 2026-04 SEV-1 tabletop drill found this ordering shaved
~1 min off detection vs the older "metric → readyz" path
(see [drills/2026-04-sev1-timescale-failover.md](../drills/2026-04-sev1-timescale-failover.md)
— a pre-launch tabletop that assumed a Patroni replica; historical only).

**Step 2 — confirm on r1 (unit, exporter signal, direct psql):**

```sh
# The cluster unit, NOT the `postgresql` umbrella (F-0154).
ssh root@r1 'systemctl status postgresql@15-main.service --no-pager'

# What postgres_exporter sees (this is the alert's source signal).
ssh root@r1 'curl -s localhost:9187/metrics | grep ^pg_up'
# pg_up 1 = exporter reaches the server; pg_up 0 = it cannot.

# Directly poke Postgres over the local socket (Postgres is loopback-only on r1).
ssh root@r1 'sudo -u postgres pg_isready -t 5; sudo -u postgres psql -d stellarindex -c "SELECT pg_is_in_recovery();"'
# Any error = Postgres really down.
```

If all three confirm Postgres is down → real incident, proceed to mitigation.
If only the Prometheus alert fires and `systemctl status` + `psql` say
healthy → a monitoring-side issue: check `stellarindex_postgres_exporter_down`
(`rules.r1/meta.yml`, [`exporter-down.md`](exporter-down.md)) and
`stellarindex_prometheus_scrape_failing` ([`scrape-failing.md`](scrape-failing.md)).
Treat as P3 scrape failure, not P1.

## Mitigation (≤ 15 min)

### A. Find out why it stopped before restarting it

```sh
ssh root@r1 'df -h /var/lib/postgresql; zpool status -x'
ssh root@r1 'tail -n 100 /var/log/postgresql/postgresql-15-main.log'
ssh root@r1 'dmesg -T | grep -i -E "oom|out of memory|nvme|i/o error" | tail -n 30'
ssh root@r1 'journalctl -u postgresql@15-main.service --since "1 hour ago" --no-pager | tail -n 50'
```

- Disk full → follow [`db-disk-full.md`](db-disk-full.md) first; a restart
  onto a full volume will just crash again.
- ZFS pool degraded / NVMe dropped → [`zfs-degraded.md`](zfs-degraded.md);
  do not restart until the pool is online.
- OOM-killed → restart (B) is safe; capture `dmesg` for RCA.

### B. Restart the cluster unit

```sh
# The CLUSTER unit, not the `postgresql` umbrella (F-0154 / postgres-ping-failing.md).
ssh root@r1 'systemctl restart postgresql@15-main.service && sleep 5 && systemctl status postgresql@15-main.service --no-pager'
ssh root@r1 'sudo -u postgres pg_isready -t 5'
```

Verify recovery:

- [ ] `pg_up 1` at `localhost:9187/metrics`; alert clears on the next
      30 s evaluation.
- [ ] `/v1/readyz` → 200 with `{"name":"postgres","ok":true}`.
- [ ] Indexer logs resume inserting trades
      (`ssh root@r1 'journalctl -u stellarindex-indexer.service -f'`).

### C. Un-wedge the indexer pool after Postgres comes back

F-0151 (2026-05-26): the indexer's `*sql.DB` pool held dead connections
for ~14 h after `postgresql@15-main` recovered. The pool now retires
connections every 30 min, but do not wait for that:

```sh
ssh root@r1 'journalctl -u stellarindex-indexer.service --since "30 min ago" | grep -E "postgres.ping|pool may be wedged"'
# If `pool may be wedged` appears, or stellarindex_postgres_ping_failing stays firing:
ssh root@r1 'systemctl restart stellarindex-indexer.service'
```

See [`postgres-ping-failing.md`](postgres-ping-failing.md). Then confirm
`stellarindex_ingestion_cursor_stuck` clears — it needs the cursor to
advance for a full 5 m window, so allow ~10 min.

### D. Data directory unrecoverable → restore from pgBackRest

If Postgres will not start (corrupt WAL, lost dataset) the only copy of
the Timescale data is the pgBackRest repo:

```sh
ssh root@r1 'sudo -u postgres pgbackrest --stanza=stellarindex info'
ssh root@r1 'systemctl list-timers pgbackrest-backup.timer restore-drill.timer --no-legend'
```

Declare the incident on the status page ([`sev-status-page-update.md`](sev-status-page-update.md))
— writes are unavailable for the duration. Restore per
[`backup-failed.md`](backup-failed.md) and the rehearsed procedure in
[`../drills/restore-drills.md`](../drills/restore-drills.md)
(`scripts/ops/restore-drill.sh`). RPO is the age of the last successful
backup, not seconds. On return, the indexer's idempotent upserts
(`ON CONFLICT DO NOTHING`) re-fill the gap from the archive; check
`stellarindex_ingestion_cursor_stuck` and `stellarindex_ingest_gap_detected`.

<!-- TODO(ash): decide the operator-facing bar for "restore in place vs.
     bring up the standby box" — there is no replica to promote today, and
     ADR-0050 explicitly builds no cross-region Postgres replication
     (R2/R3 re-ingest independently). Until the Phase-1 Patroni playbook
     lands (patroni README F-1266), D above is the only unrecoverable-path
     procedure; dr-activation.md still describes the undeployed design. -->

### E. Complete host loss

Refer to [`runbooks/dr-activation.md`](dr-activation.md). Out-of-scope for
this runbook. **Caveat:** `dr-activation.md` is itself `last_verified
2026-05-03` and describes the same undeployed Patroni/replica design;
it needs the same single-node correction before it can be followed
literally.

## Root cause analysis

Gather for the postmortem:

- `journalctl -u postgresql@15-main.service --since "1 hour ago"` on r1.
- Postgres logs: `/var/log/postgresql/postgresql-15-main.log` from around the
  event (`log_directory`/`log_filename` in
  `configs/ansible/roles/archival-node/templates/postgresql.conf.j2`).
- `dmesg -T` around the event (OOM, NVMe).
- Grafana screenshot of the Postgres panels.
- Disk-space + IOPS metrics — was it full, was it OOMKilled?
- Recent deploys: anything touching the Ansible `archival-node` role
  (`tasks/05-postgres.yml`, `templates/postgresql.conf.j2`) in the last 24 h?
  Check `stellarindex_binary_version_skew` /
  `stellarindex_binary_version_probe_degraded`
  (`rules.r1/binary-version-skew.yml`, [`binary-version-skew.md`](binary-version-skew.md))
  and recent `playbooks/deploy-binary.yml` runs.

Common root causes observed in similar systems:
1. **Disk full** — WAL couldn't write; Postgres halted writes. Catches: `stellarindex_timescale_disk_warning` (`storage.yml`) fires BEFORE this one usually. If it didn't, tune thresholds. This is the actual 2026-05-26/27 cascade.
2. **OOMKill** — Postgres process killed by the kernel. Check dmesg. Often means `shared_buffers` + `work_mem` × active_connections exceeded host RAM.
3. **Kernel / ZFS issue** — NVMe drive dropped, ZFS pool degraded. Catches: `stellarindex_zfs_pool_degraded` (`infra.yml`) fires first.
4. **Runaway query** — a long-running SELECT blocked WAL recycling. pg_stat_activity shows it.
5. **Config drift** — a hand edit to `/etc/postgresql/15/main/*.conf` that the next Ansible run reverted (or vice versa) and the service failed to start on reload.

## Known false-positive patterns

- **postgres_exporter restart / scrape gap**: `pg_up` is emitted by the
  exporter, so an exporter restart or a `postgres_exporter` scrape failure
  can look like a DB outage. Confirm via
  `systemctl status postgresql@15-main.service` and
  `stellarindex_postgres_exporter_down` before declaring.
- **Network partition between Prometheus and the exporter**: both run on r1
  today, so this is unlikely; still confirm via direct `psql` before declaring.

## Related

- ADR-0006 (TimescaleDB) — storage choice.
- HA plan §3.3 — Patroni topology (**design only — NOT deployed on r1**, see
  `docs/architecture/ha-plan.md` "DEPLOYMENT STATE" banner).
- ADR-0050 / `docs/architecture/multi-region-ha.md` — no cross-region
  Postgres replication; the older `multi-region-topology.md` §5 is superseded.
- [`postgres-ping-failing.md`](postgres-ping-failing.md),
  [`db-disk-full.md`](db-disk-full.md), [`backup-failed.md`](backup-failed.md),
  [`exporter-down.md`](exporter-down.md).
- Postmortems: `docs/operations/postmortems/` (none yet; first one goes here).

## Appendix — undeployed HA design (do not follow on r1)

Kept only so the intent is not lost when the Patroni playbook lands.
Everything here is **not runnable on r1 today**: no `patronictl`/`etcdctl`
binaries, no `patroni`/`etcd` units, no `db-*.internal` hostnames.

- Patroni scope would be `patroni_cluster_name` (`stellarindex-r1` in the
  role README's example inventory — not `stellarindex`); commands would be
  `patronictl -c /etc/patroni/patroni.yml list` / `... failover stellarindex-r1 --candidate <host>`.
- etcd would be a 3-node cluster (Patroni role preflight asserts exactly 3
  `postgres_cluster` hosts; quorum 2/3), keys under `/service/<scope>/`.
- Automatic sync-replica promotion in 30–60 s; async-replica promotion
  accepts ≤ 5 s RPO.

## Changelog

- 2026-08-28 — rewritten for the single-node r1 reality (no Patroni/etcd/
  replicas; `postgresql@15-main.service` restart + pgBackRest restore paths);
  alert expr is `pg_up == 0` from `postgres_exporter` (F-1329); readyz
  `checks` is an array; companion alerts are `stellarindex_postgres_ping_failing`
  then `stellarindex_ingestion_cursor_stuck` (ticket, ~10 min); dropped the
  `deploy/timescale-statefulset.yaml` and R3-async-replica references
  (ADR-0050). Patroni text moved to the appendix.
- 2026-04-22 — initial draft. @ash.
