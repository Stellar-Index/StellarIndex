---
title: Runbook — DR (disaster-recovery) activation
last_verified: 2026-08-28
status: ratified
related:
  - docs/architecture/ha-plan.md
  - docs/adr/0008-ha-topology.md
  - docs/architecture/multi-region-ha.md
  - docs/adr/0050-multi-region-ha-architecture.md
  - docs/adr/0016-per-region-storage-strategy.md
  - docs/operations/sev-playbook.md
  - docs/operations/archival-node-bringup.md
  - docs/operations/runbooks/timescale-primary-down.md
---

# Runbook — DR (disaster-recovery) activation

The procedure for cutting traffic over to a standby region when
the primary region (R1 today) is partially or fully unrecoverable.
Sister to [`timescale-primary-down.md`](timescale-primary-down.md) §D
"Complete cluster loss (asteroid scenario)", which references this
runbook explicitly.

This is a SEV-1 procedure — open the incident channel, declare
severity, follow [`sev-playbook.md`](../sev-playbook.md) §4 for the
incident-management overlay. The steps below are the technical
flip; the IC drives + validates in parallel.

**Drill status (honest, CS-113; refreshed 2026-08-28):** this runbook
has NOT been executed end-to-end — there is no standby region to cut
over to (R2/R3 are unprovisioned; single-host r1 is the entire
deployment). One scratch restore from the pgBackRest repo has been
performed by hand — 2026-07-03, `repo1`, manual, five attempts, logged
in [`drills/restore-drills.md`](../drills/restore-drills.md) (CS-110;
`scripts/ops/restore-drill.sh` is that drill, installed on-host at
`/usr/local/bin/restore-drill.sh`). Since 2026-07-27 it IS on a timer:
`restore-drill.timer` is enabled + started by the `archival-node` role
(`tasks/18-pgbackrest-backup.yml`), monthly on the first Saturday
04:00 UTC, drill root `/srv/restore-drill`, refusing (exit 2) under
200 GB free. Caveat: until 2026-08-19 every scheduled fire failed at
namespace setup (`226/NAMESPACE` — the drill root was under
`/var/tmp`, shadowed by `PrivateTmp=`), so `restore-drills.md` still
logs only the manual 2026-07-03 series. Confirm a *scheduled* PASS in
`journalctl -u restore-drill.service` before trusting the timer. The
annual DR exercise described in
[sev-playbook §8 "Drills"](../sev-playbook.md#8-drills) is the
INTENDED cadence once multi-region lands; treat every multi-region
step below as untested design, not verified procedure. If you're
reading this for the first time *during* an incident, follow it from
§3.

**Architecture as of 2026-08-28:** r1 is a single host running a single
Postgres and a single Redis — no Patroni, no Redis Sentinel, no
HAProxy VIPs, no Cloudflare load-balancer pool. The `patroni` /
`redis-sentinel` / `haproxy` ansible roles exist in-tree but have no
wired playbooks (`configs/ansible/README.md`). `api.stellarindex.io`
is a bare A record to r1's Caddy (`Caddyfile.j2`). The current
multi-region program is
[`multi-region-ha.md`](../../architecture/multi-region-ha.md)
(ratified by ADR-0050, 2026-08-21; supersedes ADR-0016 and ADR-0008's
multi-region decision) — its §6 "Failover design" is the cutover
model this runbook will eventually execute, and its §3c notes the
control plane is region-local, so API keys minted in the primary
return **401** in a DR region until control-plane replication lands.

> **⛔ The backups are on the box you are trying to recover from.**
> pgBackRest runs **`repo1` only**, at `/var/lib/pgbackrest` — a ZFS
> dataset on r1's own `data` pool. There is no off-site repo:
> `configs/ansible/inventory/r1.yml` sets `pgbackrest_offsite_ack:
> true`, the explicit acknowledgement of that gap (the role's
> `18-pgbackrest-backup.yml` asserts on it), and ClickHouse has no
> DATA backup — only a local daily schema+state snapshot
> (`ch-schema-snapshot.timer`, ADR-0043 §2.1; its off-site target
> `ch_schema_snapshot_mc_target` is unset). So the §1 triggers split in two:
> **logical corruption / partition** (pool still readable) → a
> pgBackRest restore is available; **pool or host-fleet loss** → the
> backups die with the data, and so does the galexie archive (MinIO is
> a dataset on that same pool; ledgers already trimmed to the AWS
> public cold tier under ADR-0027 survive, the hot window does not).
> The remaining path is then a rebuild
> from a public Stellar history archive — days to weeks, per
> `ha-plan.md` §8's total-loss row — not a restore. Do not plan a
> recovery around an off-site copy — see
> `docs/architecture/ha-plan.md` §8 and
> `docs/operations/off-site-backup-plan.md` (status: proposed).

---

## 1. When to activate DR

Activate when ANY of these is true AND no faster recovery exists:

- **Primary region's storage tier is unrecoverable.** Patroni
  has no quorum-eligible replicas; pgBackRest restore is failing
  or estimated > 4 h; the disk-failure mode in
  [`timescale-primary-down.md`](timescale-primary-down.md) §A
  cannot promote a replica.
- **Primary region's network is partitioned for > 30 min and
  external monitoring confirms the partition is regional, not
  local-to-us.** Cloudflare health probe + a third-party
  network-status site both report the same regional outage.
- **Primary region's host fleet is gone** (provider-side
  catastrophic failure, datacentre power loss, etc.). Hetzner
  status page red on R1's location, no projected ETA.
- **Data corruption suspected on primary AND replica** (read
  beyond storage failure: a logical corruption that's already
  replicated). Restore from `pgBackRest` to the DR region is
  the only option that doesn't propagate the corruption.

**Don't activate when** (applies once Patroni / Sentinel exist per
[`multi-region-ha.md`](../../architecture/multi-region-ha.md) §7
prerequisites; on 2026-08-28 r1 is a single Postgres + single Redis
with no failover automation, so these bullets have no live component
behind them yet):
- Patroni is mid-failover (give it 60 s; it's designed to handle
  this without operator intervention).
- A single Timescale primary is down but the replica is healthy
  — [`timescale-primary-down.md`](timescale-primary-down.md) §A
  is the right runbook (faster + lossless).
- Redis cluster lost a master (Sentinel handles; see
  [`scenarios/sev2-redis-sentinel-failover.md`](../drills/scenarios/sev2-redis-sentinel-failover.md)).
- One of the three regions is degraded but R1 is healthy —
  R2/R3 are independent-ingest regions in the current design (Model B,
  [`multi-region-ha.md`](../../architecture/multi-region-ha.md) §2;
  ADR-0016 is superseded by ADR-0050) and don't need DR activation;
  they self-heal once the partition clears.

The decision itself is reversible (failback in §6). The COST of
a wrong activation is ~5 s RPO data loss (whatever didn't
replicate before primary went down) + a customer-visible blip
during DNS propagation. The COST of NOT activating when you
should is sustained customer-visible outage. Bias toward
activation when the criteria are clearly met.

---

## 2. DR region pre-flight (do this BEFORE any traffic flip)

The IC's tech lead runs these checks while the comms lead drafts
the status-page update. Both happen in parallel.

### 2.1 Confirm DR region's storage is current

```sh
# From a DR-region operator workstation:
ssh root@<dr-region-postgres-primary>

# Check pgBackRest archive freshness
pgbackrest --stanza=stellarindex info | head -20
# → "archive (current)" + most-recent backup/WAL within ~5 min

# If WAL replication lag was ≤ 5 s (the SLO target):
psql -c "SELECT pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn();"
# → both LSNs should be the same or within a handful of bytes
```

If the DR region's storage is more than 30 min behind primary
AND the primary is still partially serving, **delay activation**:
let WAL ship the gap first, then proceed. If the primary is
fully gone, accept the lag and proceed (you can't get more recent
data; the lag IS the RPO).

### 2.2 Confirm DR region's MinIO archive is intact

```sh
# On the DR-region archival host. archive-completeness is timer-driven
# (archive-completeness.timer); either read the last report …
curl -sS http://127.0.0.1:3000/v1/diagnostics/archive | jq '{scanned_at, range, cross_anchor: {expected, found, missing_count}}'
# → scanned_at recent + cross_anchor.missing_count == 0

# … or force a fresh run (unit computes ARCHIVE_TO via
# /usr/local/sbin/compute-archive-to.sh, then runs
# `stellarindex-ops archive-completeness verify -from $ARCHIVE_FROM -to $ARCHIVE_TO
#  -workers 8 -network pubnet -textfile-output … -output-file
#  /var/lib/galexie/last-completeness-report.json` under run-heavy-job.sh)
sudo systemctl start archive-completeness.service && journalctl -u archive-completeness.service -n 30 --no-pager
# → exit 0 and missing_count == 0 in the report
```

Scope note: `archive-completeness verify` checks the cross-anchor
history archive only; MinIO bucket structure is **not** checked
(`cmd/stellarindex-ops/main.go` usage). The verify flag set is
`-archive-root -from -to -workers -owner-user -owner-group -network
-output-file -textfile-output` — there is no `--region` / `--tier`.

If the DR region's MinIO archive has gaps, the historical
since-inception API will serve stale data until the gaps are
filled. Note this in the status-page update; it doesn't block
flip.

### 2.3 Confirm DR region's API/aggregator/indexer hosts are reachable

```sh
# From DR-region SSH bastion:
for host in <dr-api> <dr-aggregator> <dr-indexer>; do
    ssh root@$host 'systemctl status stellarindex-api stellarindex-aggregator stellarindex-indexer galexie stellar-core minio --no-pager' | grep -E "Active|service"
    # ops jobs are timer-driven and NOT named stellarindex-*, so a glob misses them:
    ssh root@$host 'systemctl list-timers --all --no-pager' | grep -E 'archive-completeness|galexie-archive|verify-archive-tier|supply-snapshot|restore-drill|pgbackrest-backup'
done
# → every service "active (running)" or "active (waiting)"; every timer with a NEXT fire
```

Also confirm `stellarindex_binary_version_skew` /
`stellarindex_binary_version_probe_degraded`
(`configs/prometheus/rules.r1/binary-version-skew.yml`) are not firing
on the target region before the flip — a region running mismatched
binaries serves byte-divergent closed buckets.

If a host is down, decide: spin up a replacement (per
[`archival-node-bringup.md`](../archival-node-bringup.md)) before
flipping, OR flip with reduced redundancy in DR. Reduced
redundancy is acceptable for the duration of the incident — note
on the status page.

---

## 3. Traffic flip (the actual cutover)

### 3.1 Status-page update

Comms lead posts the **Investigating** entry per
[`sev-status-page-update.md`](sev-status-page-update.md). Suggested
wording for §"What customers see":

> We've identified an issue with our primary region and are
> failing over to our disaster-recovery region. API requests
> may briefly fail or return stale data during the flip
> (typically < 60 seconds). We'll post an update once the
> flip completes.

Don't name internal infrastructure. Don't speculate on cause
yet — that goes in the **Identified** entry after flip succeeds.

### 3.2 DNS flip

> **AS OF 2026-08-28 this section is design, not procedure.** There
> is no Cloudflare LB pool, no HAProxy VIP and no `cf-cli` tooling
> anywhere in the repo; `api.stellarindex.io` is a bare A record →
> r1's Caddy (`Caddyfile.j2`, `136.243.90.96`). Both mechanisms below
> are the [`multi-region-ha.md`](../../architecture/multi-region-ha.md)
> §6 model. TODO(ash): decide the interim flip mechanism (manual
> Cloudflare-dashboard A-record change is the only option today) and
> where the Cloudflare API token lives — `deploy/ops-keys.md` never
> existed.

The Cloudflare DNS records pointing `api.stellarindex.io` at
the primary region's HAProxy VIPs need to point at the DR
region's HAProxy VIPs.

Two flip mechanisms (use whichever the operator's incident-
console wires up):

**A. Cloudflare load balancer (preferred):** the LB pool
already has all three regions' origins configured per
[ADR-0008](../../adr/0008-ha-topology.md) §"DNS / load balancing".
Mark the primary-region pool members as down via the
Cloudflare dashboard:

1. Cloudflare → Traffic → Load Balancers → `api-prod`
2. Edit the primary pool → set `enabled: false`
3. Save. Health-check probes drop the primary; traffic shifts to
   the DR pool within ~30 s.

**B. Manual DNS swap (fallback if Cloudflare LB is down):**
update the A record directly:

```sh
# Requires the Cloudflare API token (location: TODO(ash) — no ops-keys doc exists in-tree).
# No cf-cli wrapper exists; use the Cloudflare dashboard or the raw API to
# repoint the api.stellarindex.io A record at the DR origin.
# TTL is 60s by design; full propagation < 2 min
```

### 3.3 Verify API is serving from DR

```sh
# Liveness — /v1/healthz carries NO region field
curl -sS -H 'X-Request-Id: dr-flip-verify' https://api.stellarindex.io/v1/healthz
# → {"status":"ok","uptime":"…","status_root":"/v1/status"}

# Origin identity — the region label lives on /v1/status
curl -sS https://api.stellarindex.io/v1/status | jq '.region'
# → {"name": "<dr region_name>", "deployment": "…"}

# Spot-check a real query
curl -sS 'https://api.stellarindex.io/v1/price?asset=native' | jq '.data.price, .as_of'
# → non-empty price + recent timestamp
```

The `.region.name` field in `/v1/status` confirms the origin we hit
(`[region] name` in the node's `stellarindex.toml`; r1 is
`Hetzner-r1`). A request landing on the primary (because some CDN
edge is still caching DNS) would show the primary's region — wait for
TTL expiry rather than declaring success early.

### 3.4 Update status page

Comms lead posts **Identified** with one-line cause + that
mitigation is deployed. Customer-facing wording:

> We've completed failover to our disaster-recovery region.
> API requests are now serving normally. Some historical data
> may temporarily reflect a brief gap during the flip; we're
> investigating root cause and will post an update with full
> service confirmation within the hour.

---

## 4. Post-flip monitoring (first hour)

The DR region is now serving production. Validate the next few
metrics actually look right rather than just "running":

### 4.1 API SLO + SLA-probe alerts

```sh
# On the DR serving host (no Grafana vhost exists; Prometheus :9090 is
# firewalled to internal addresses, not a public hostname)
curl -sS http://127.0.0.1:9090/api/v1/alerts | jq -r '.data.alerts[] | select(.state=="firing") | .labels.alertname'
```

Specifically watch (rule files under `configs/prometheus/rules.r1/`):
- `http_request_duration_seconds` p95 < 200 ms (service SLA F3.1) —
  `stellarindex_api_latency_p95_high` (`api.yml`),
  `stellarindex_slo_latency_burn_fast` (`slo.yml`)
- `http_requests_total{status=~"5.."}` rate < 0.1 % (F3.3) —
  `stellarindex_api_error_rate_high` / `_critical` (`api.yml`),
  `stellarindex_slo_availability_burn_fast` (`slo.yml`)
- external probe view — `stellarindex_sla_probe_p95_breach` /
  `_freshness_breach` / `_unit_failed_alert` / `_stale`
  (`sla-probe.yml`; the probe is the `stellarindex-sla-probe` wrapper
  stack — the Go sla-probe stack was retired 2026-08-24)
- price freshness — `stellarindex_price_staleness_seconds` within
  baseline / `stellarindex_api_price_stale` (`api.yml`) not firing.
  (`flags.stale` is a per-response envelope flag, not a metric; a
  rising staleness gauge means the aggregator's upstream sources
  aren't all reaching DR.)

### 4.2 Aggregator + ingest health

```sh
# On the DR host. Should match primary's pre-incident steady state within ~5 min
stellarindex-ops list-cursors -config /etc/stellarindex.toml
# → every source's last_ledger advancing
stellarindex-ops detect-gaps -config /etc/stellarindex.toml
# → exit 0 (exit 1 = a source lags > 100 ledgers behind the network tip)
```

If the indexer's cursors aren't advancing, check Galexie's
read connectivity to MinIO — the DR region's MinIO bucket
should be writable + readable per
[`multi-region-ha.md`](../../architecture/multi-region-ha.md) §4/§5
(ADR-0050; ADR-0016 is superseded).

### 4.3 Customer-visible flag rates

```sh
# Aggregator's anomaly + freeze counters — on the DR serving host
# (same expression as alert stellarindex_anomaly_freeze_engaged, rules.r1/anomaly.yml)
curl -sS http://127.0.0.1:9090/api/v1/query \
    --data-urlencode 'query=sum by (class) (rate(stellarindex_anomaly_freeze_engaged_total[5m]))' | jq
```

A freeze-engaged spike right after flip is normal (some pairs
will see source-class diversity drop briefly during the flip).
A SUSTAINED freeze rate above pre-incident baseline is a signal
that DR's source set is incomplete.

---

## 5. Status-page resolution

When SLA + ingest + flag rates have all returned to baseline
for ≥ 30 min, comms lead posts **Resolved**:

> Service is fully restored as of <UTC>. Total customer impact:
> <duration> with elevated error rates and one ~60 s gap during
> the failover. We'll post a full post-mortem within 72 hours
> per our SEV-1 commitment.

The full post-mortem follows
[`sev-playbook.md`](../sev-playbook.md) §6.

---

## 6. Failback to primary

DO NOT failback during the same incident shift unless the
primary's failure mode was clearly transient. Most DR
activations should run on the DR region for 24–72 hours so the
team can validate primary's underlying fix isn't itself faulty.

When the primary is verified healthy:

### 6.1 Primary catch-up

```sh
# Check WAL streaming from DR back to primary
psql -h <primary-postgres> -c "SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn();"
# → in_recovery=true, replay_lsn closely tracking DR's send_lsn
```

The primary should be running as a streaming replica of DR
during the failback window. If `pg_is_in_recovery()` returns
false, the primary is still acting as a former-primary that
diverged — escalate to the IC; this needs `pg_rewind` or full
restore from DR's pgBackRest, NOT a simple flip.

### 6.2 Reverse the DNS / Cloudflare LB flip

Same procedure as §3.2 but flipping the DR pool down + primary
pool up. Run the same §3.3 verifications.

### 6.3 Post-failback monitoring

Same as §4 but watching primary's metrics. Pay extra attention
to anything that would indicate replication lag — a primary
that's "caught up" but missing recent rows would surface as
`stellarindex_api_price_stale` firing / `stellarindex_price_staleness_seconds`
rising on /v1/price.

---

## 7. Escalation

If any §3 step fails (DNS flip won't propagate, DR region's
healthz returns 5xx, etc.):

1. Don't roll back — the primary is presumed lost; rolling back
   to it makes things worse.
2. Page secondary on-call + engineering manager.
3. Post a status-page update acknowledging "extended outage
   during failover" without speculation on cause.
4. The fallback path is to spin up a fresh archival node in a
   third region per
   [`archival-node-bringup.md`](../archival-node-bringup.md);
   that's measured in hours, not minutes.

---

## 8. What this runbook does NOT cover

- **Bringing up a fresh region** — see
  [`archival-node-bringup.md`](../archival-node-bringup.md).
- **Per-component HA failover** (Patroni primary swap, Sentinel
  Redis swap) — see [`timescale-primary-down.md`](timescale-primary-down.md)
  and [`scenarios/sev2-redis-sentinel-failover.md`](../drills/scenarios/sev2-redis-sentinel-failover.md).
- **Annual DR exercise procedure** — same flip, but pre-
  announced + conducted on staging-equivalent traffic. See
  [`sev-playbook.md`](../sev-playbook.md#8-drills) §8 "Drills".
- **Incident-management overlay** (severity declaration,
  comms cadence, postmortem timing) — see
  [`sev-playbook.md`](../sev-playbook.md) §4.

## 9. Drift signals

Run quarterly:

- DR region's `/v1/status` `.region.name` returns the right region
  label (`/v1/healthz` carries no region).
- Cloudflare LB's primary + DR pools both have current host
  IPs (operator-supplied DNS rolls invalidate the pool config
  silently).
- pgBackRest stanza fresh in BOTH regions (not just primary).
- `dr-activation.md` itself reflects
  [`multi-region-ha.md`](../../architecture/multi-region-ha.md) /
  ADR-0050 — re-read on each change to those docs.

A failed drift signal is a launch-readiness regression — file
an issue with the `dr-readiness` label (TODO(ash): confirm the label
exists on GitHub; nothing in-tree defines it).
