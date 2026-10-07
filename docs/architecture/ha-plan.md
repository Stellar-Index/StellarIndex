---
title: High-Availability and Infrastructure Plan
last_verified: 2026-10-06
status: current — the one HA/infra page. What runs today is checked against configs/, deploy/ and test/load/; the multi-region plan is ratified by ADR-0050 and deferred post-v1.0.
---

# High-Availability and Infrastructure Plan

This page is the single source for how Stellar Index stays up: what runs
today, the planned in-region HA and R2/R3, and the decisions that bind both. Decision records: [ADR-0008](../adr/0008-ha-topology.md)
(single-region topology),
[ADR-0024](../adr/0024-redis-ha-via-sentinel.md) (Redis Sentinel),
[ADR-0043](../adr/0043-backup-and-restore-strategy.md) (backup) and
[ADR-0050](../adr/0050-multi-region-ha-architecture.md) (multi-region).
This page wins over any older document.

**Status in one line.** Production is one box (r1). Every HA component
beyond it (HAProxy, Patroni, Redis Sentinel, the Prometheus pair, the Loki
role, R2, R3) is designed, mostly has role code in
`configs/ansible/roles/`, and is **deferred until after v1.0**
(INV-0834, INV-1048).

---

## 0. Service targets

- **p95 ≤ 200 ms, p99 ≤ 500 ms** on `/v1/price` and `/v1/oracle/*`
  ([ADR-0009](../adr/0009-latency-budget.md)). Measured p95 68 ms / p99 98 ms
  (k6, see [coverage-matrix.md](coverage-matrix.md#service-objectives-and-their-proof)).
- **≥ 99.9 % availability** — the figure published to customers at
  [stellarindex.io/sla](https://stellarindex.io/sla), an error budget of
  ~43 min per 30-day month. The 99.99 % in ADR-0008 is the internal target
  the topology was sized against, not a commitment. The burn-rate alerts in
  `deploy/monitoring/rules/slo.yml` budget against 99.9 %.
- **≤ 30 s freshness** on `/v1/price/tip`. `/v1/price` serves the last
  closed bucket ([ADR-0015](../adr/0015-last-closed-bucket-rate-serving.md))
  and is 30–150 s old by design.

**Availability is not externally measured.** `stellarindex-sla-probe` runs
on the API host against `http://localhost:3000/v1`
(`configs/healthchecks/sla-probe.sh:27`), so it cannot see a Caddy, TLS, DNS
or network failure. The Alertmanager dead-man's switch catches a hard
outage but computes no percentage. Until an off-host probe ships, any
published availability figure is an objective, not a measurement. See
[`../operations/sla-probe.md`](../operations/sla-probe.md).

---

## 1. Decisions that still bind

| Area | Decision | Rejected | Source |
|---|---|---|---|
| HA model | HA by **cross-region failover, one box per region** | Full per-region fleets (Patroni 3 + HAProxy 2 + Sentinel 3 per region, ~$180–288 K/yr) | ADR-0050 §8 |
| Region consistency | **Model B**: each region ingests the chain itself; answers match by determinism (closed buckets) | Cross-region Postgres replication; a Patroni/etcd cluster stretched across regions | ADR-0050 §2, ADR-0015 |
| Lake | **R1 is the lake authority**; other regions proxy lake routes to R1; object storage is a fallback only while R1 is unreachable | Per-region S3-tiered ClickHouse (1,000–8,000 GETs per cold page, breaks the 8 s budget) | ADR-0050 §3b |
| Product priority | **API first**: pricing serves locally in every region; no SLO'd route crosses a region | Hot lake set on R2 | ADR-0050 §0b, §3a |
| Providers | R1 Hetzner FSN1; R2 Vultr US bare metal; R3 Vultr Singapore | R2 on AWS (only made sense holding the full lake) | ADR-0050 §4 |
| Postgres HA | Patroni 3.x + etcd, 3 nodes, per region | Stolon; TimescaleDB native multi-node | ADR-0008 |
| Redis HA | **Sentinel** (1 primary + 2 replicas), client-side discovery, no VIP | Redis Cluster (sharding) | ADR-0024 |
| Load balancer | HAProxy + keepalived VRRP, 2 hosts | nginx; Kubernetes ingress | ADR-0008 |
| Aggregator, indexer | **One instance each**, enforced by a Postgres advisory lock; no standby | Redis-lease leader election | §3.7, §3.8 |
| Metrics | Two independent Prometheus instances + gossiped Alertmanager | Clustered Prometheus; Thanos (deferred) | §7 |
| Logs | Single-host Loki, chunks in S3-compatible storage | Paired Loki for v1 | §7 |
| Status page | Self-hosted in the explorer; incidents are Markdown in git | Instatus or another SaaS; Cachet | §7.4 |
| Load testing | k6, self-hosted runner, arrival-rate scenarios | Vegeta, Gatling, wrk2, custom Go | §7.3 |
| Backups | Off-site copies of the ClickHouse lake and Postgres on Backblaze B2; the raw archive is re-pulled from SDF's public dataset | Mirroring public data off-site | ADR-0043, §8 |

---

## 2. Physical topology

### 2.1 Today: r1, one box

r1 is a Hetzner dedicated server in FSN1 (Falkenstein, DE) with a ZFS pool
`data` of 4 × 7.68 TB NVMe in **raidz1**. Everything runs on it:

```
   Internet ── Caddy (TLS, :443) ── stellarindex-api (one process)
                                         │
             ┌───────────────────────────┼──────────────────────────┐
     Redis (single node,         Postgres 15 + Timescale      ClickHouse lake
     internal bind, no AUTH)     (postgresql@15-main,         (one instance,
                                  no replicas)                 non-replicated)
             └───────────────────────────┼──────────────────────────┘
                         stellarindex-aggregator (one, instance lock)
                         stellarindex-indexer    (one, instance lock)
                                         │
                         MinIO (galexie-live, galexie-archive)
                                         │
                         galexie (captive core is its subprocess)
```

- The public edge is Caddy
  (`configs/ansible/roles/archival-node/tasks/19-caddy.yml`), not HAProxy.
- `configs/ansible/roles/archival-node` is the only role applied to r1.
  `playbooks/monitoring.yml` targets a `prometheus_pair` group r1 does not
  define, so it matches no hosts; no playbook invokes `haproxy`, `patroni`
  or `redis-sentinel` (INV-0978).
- `deploy.yml` deploys to `r1`, `testnet` and `futurenet`; `r2` and `r3`
  exist only as `configs/ansible/inventory/r{2,3}.example.yml`.

### 2.2 Planned in-region shape

ADR-0050 §10 Phase 1 is an in-region HA build on R1: procure and build, not
role wiring. The roles hard-gate on inventory groups (`postgres_cluster` = 3,
`haproxy_lb` = 2, `redis_cluster` = 3, `prometheus_pair` = 2), and the
archival-node role installs neither ClickHouse nor Redis (it overlays config
on a hand-built box), so that automation must be written too.

```
                 Cloudflare (WAF, cache, cross-region LB)
                                 │
                      keepalived VIP (VRRP)
                     HAProxy-A ─── HAProxy-B
                                 │
                 stellarindex-api × 3 (stateless, /v1/readyz)
                                 │
     ┌───────────────────────────┼──────────────────────────────┐
 Redis Sentinel             Patroni: primary + 2 sync        ClickHouse
 1 primary + 2 replicas,    replicas, etcd × 3               (one instance;
 3 Sentinels co-located     (per region, never stretched)    HA is cross-region)
     └───────────────────────────┼──────────────────────────────┘
                aggregator (one) · indexer (one) · galexie + captive core
```

ADR-0050 §8 makes one box per region the default. Whether Phase 1 is still
wanted once R2 provides cross-region failover is open (§11).

### 2.3 Planned regions

| | R1 — Hetzner FSN1 | R2 — Vultr US | R3 — Vultr Singapore |
|---|---|---|---|
| Role | primary, lake authority, integrity leader | active pricing, lake proxy | active pricing, lake proxy |
| Pricing (Timescale + Redis) | full local | full local | full local |
| Lake (ClickHouse) | full local | none; all lake routes proxy to R1 | none; all lake routes proxy to R1 |
| Raw archive | local MinIO | re-pulled from SDF's public dataset | re-pulled from SDF's public dataset |
| Verification tiers | all (A/B/D/E), trust anchor | A + D local, trusts R1 for B + E | same as R2 |
| Disk | existing | ~2 TB is generous (Timescale + Redis + OS) | same |

R2 and R3 have the same shape. The explorer accepts one extra
transatlantic round trip per lake-backed page; the API does not.

Region notes that hold under this shape:

- Stellar-core nodes are SCP peers, not primary/replica.
- Tier D (every region, weekly) cross-compares checkpoint hashes from ~6 Tier-1
  archives against each other. It catches forks, not local byte drift (Tier B, R1).
- Edge failover: Cloudflare geo steering, `/v1/readyz` check every 15 s, a region
  is pulled after 3 failures, DNS TTL 60 s.

---

## 3. Component by component

### 3.1 Edge and load balancer

**Today:** Caddy on r1 terminates TLS and proxies to the single API
process. It does not expose `/metrics`.

**Planned:** HAProxy + keepalived on two hosts (`lb-01`, `lb-02`), public
DNS on the keepalived VIP. Role: `configs/ansible/roles/haproxy/` (no
playbook).

- **Health check:** `GET /v1/readyz`, `inter 5s fall 3 rise 2 slowstart 10s`
  (`templates/haproxy.cfg.j2`). Postgres is critical (503); Redis and
  ClickHouse are non-critical (200 with `status="degraded"`), so a cache or
  lake outage does not drain the pool. `GET /v1/livez/lake` is the
  lake-only signal (503 while ClickHouse is unreachable) for steering lake
  routes separately.
- **Failover:** API instance down: ejected after ~15 s; HAProxy host or
  process down: VIP moves in 1–4 s; both LB hosts down: manual, same blast
  radius as a region outage.
- **Gotchas the role handles or documents:** `net.ipv4.ip_nonlocal_bind=1`
  (bind the VIP before keepalived assigns it); VRRP multicast (224.0.0.18)
  is blocked on some clouds, so use `unicast_peer`; keepalived silently
  truncates `auth_pass` to 8 bytes; `reload`, never `restart`; stats stay on
  `127.0.0.1:8404`; use HAProxy's built-in Prometheus exporter, not
  `haproxy_exporter`.
- **Not in the role:** certificate automation (the operator drops a cert in
  `/etc/haproxy/certs/`, INV-1050) and PgBouncer (no role yet; each
  process's `pgxpool` pools, INV-1049).
- **Cross-region (planned):** Cloudflare routes to the nearest healthy
  region. The explorer can follow because ADR-0044's edge SSR resolves the
  API region at request time.

### 3.2 Ingest: galexie and captive core

**Today:** one galexie on r1, captive core as its subprocess, writing to
MinIO; one `stellarindex-indexer` reads it via `internal/ledgerstream` →
`internal/dispatcher`. Recovery from a dead galexie is a systemd restart
plus catch-up from local state.

**Planned (Model B):** every region runs its own galexie + captive core
(~10.5 GiB) and indexer, and builds its own stores. There is no ingest
failover between regions; a region whose ingest stalls serves stale-flagged
data until it catches up.

**Archive contents (gap-scan 2026-08-21):** r1's `galexie-archive` holds the
genesis chunk `[0, 63999]` and `[49984000, tip]`. The middle
`[64000, 49983999]` was capacity-trimmed and is read through the
`aws-public-blockchain` cold tier
([ADR-0027](../adr/0027-lcm-cache-tiering.md)), with SDF
`history.stellar.org` as the canonical upstream. `public-dataset-check.yml`
monitors that dataset weekly. Run a genesis-to-tip gap scan before trusting
any archive copy as a rebuild source.

### 3.3 Postgres / TimescaleDB

**Today:** one `postgresql@15-main` on r1. No replicas, slots or
publications. Backups in §8.

**Hypertables and retention (AGENTS.md invariant 8):** `trades` keeps raw
rows forever — migration 0031 removed the old 90-day policy, and a
`drop_after` on `trades` is drift. `prices_1m` … `prices_1mo` are
continuous aggregates with no `drop_after`. Their oldest bar is a coverage
floor, not a retention one: `prices_1d` starts **2018-07-01** with a single
pair (`crypto:XLM`/`fiat:USD`) and holds nothing between 2021-02-01 and
2024-03-10 (measured on r1 2026-09-03). Size a restore or backfill off that, not
chain genesis.

**Planned (Patroni, role `configs/ansible/roles/patroni/`):**

- Primary + 2 synchronous replicas (`db-01..03`),
  `synchronous_commit=remote_apply`,
  `synchronous_standby_names='ANY 1 (db-02, db-03)'`. Failover RTO 60 s.
- etcd, 3 nodes, one cluster per region; `patroni_cluster_name` and
  `etcd_cluster_token` differ per region so clusters cannot join across
  regions. etcd traffic uses TLS with client-cert auth by default
  (`etcd_tls_enabled: true`); the role refuses to render without the PEM
  material from vault.
- Preflight requires 3 nodes and ≥ 32 GB RAM.
- Bootstrap: `db-01` initialises, the others join; reruns detect a running
  cluster via the REST API (`:8008/cluster`) and do nothing. DR rebuild:
  set `patroni_bootstrap_method: pgbackrest` (default `initdb`); the primary
  restores from pgBackRest, replicas `pg_basebackup` from it.
- Operating risks: losing 2 of 3 etcd nodes stops writes (correct; restore
  quorum first). With `remote_apply`, two slow replicas raise commit
  latency — alert on sustained replica lag > 5 s. Pin the `timescaledb`
  package version identically on every node. A crashed primary may not be
  `pg_rewind`-eligible; Patroni re-images it, which is slower. Gate the
  REST API behind the firewall and Basic Auth from vault.
- Implementer questions (INV-1053), with the current default: Patroni from
  apt, not PyPI; etcd 3.5; 3 etcd nodes, not 5; write routing via a
  Patroni-aware PgBouncer sidecar watching `/leader` rather than a bare
  keepalived VIP.

**Control-plane data** (accounts, API keys, sessions, WebAuthn, alerts,
webhooks) is non-chain state that determinism cannot reproduce. It is
region-local today, so a key minted on R1 would 401 on R2. It needs its own
small replicated store before a second region serves authenticated traffic
(ADR-0050 §3c, covering ADR-0049's tables); until then only anonymous
traffic fails over cleanly.

### 3.4 Redis

**Today:** a single `redis-server` on r1, internal bind, no AUTH.
`internal/storage/redisclient` builds a `go-redis` `FailoverClient` when
`[storage] redis_sentinel_addrs` and `redis_master_name` are set, and a
plain client otherwise. r1 sets neither, so the Sentinel branch is shipped
but dormant.

**Planned (Sentinel, ADR-0024, role `configs/ansible/roles/redis-sentinel/`):**

- 1 primary + 2 replicas, 3 Sentinels co-located on the same hosts, quorum
  2. Sentinel, not Cluster: the hot set is a few GB, and sharding adds
  operational cost without solving a capacity problem.
- No VIP or HAProxy in front: clients ask any Sentinel for the primary.
- Failover takes 15–30 s; affected responses carry `stale: true` and fall
  back to Timescale. Replicas serve pre-failover data during the window
  (`replica-serve-stale-data yes`).
- Persistence is AOF every second plus RDB. Correctness never depends on
  it: a wiped Redis is re-warmed by the aggregator from Postgres within
  minutes. Rate-limit counters reset on a wipe.
- Auth: the role templates `requirepass` + `masterauth`, a Sentinel
  password, and an optional ACL lockdown (`redis_acl_lockdown`). A password
  change needs all three nodes rolled before Sentinel re-converges. Use
  `redis_exporter` for metrics (INV-1058).
- **No TLS** in the role (TODO in `defaults/main.yml`); the Go client dials
  plain `host:port`. Decide `tls-port`/`tls-replication` plus client TLS
  when replication leaves one host. Redis never spans regions, so this is
  in-region traffic only.
- Never replicated cross-region.

**Rate limits and quotas** count in the serving region's Redis, keyed per
principal without a region dimension
(`internal/api/v1/middleware/ratelimit.go`, `monthly_quota.go`). The limiter
is a fixed-window counter (INCR + EXPIRE), not a token bucket. With N
regions a caller gets N × the limit; see §11.

### 3.5 Object storage (MinIO)

**Today:** one MinIO on r1's ZFS pool holding `galexie-live` and
`galexie-archive`. It is protected by local ZFS snapshots
(`zfs_snapshot_datasets`, 7-day retention), not by an off-site copy (§8).

ADR-0008 sized a 9-node EC(6+3) MinIO cluster. Nothing in the ADR-0050
phasing procures it, and the raw archive is re-pullable public data, so it
is not on the current plan.

### 3.6 API pool

**Today:** one `stellarindex-api` on r1 (`Restart=on-failure`,
`RestartSec=5s`).

**Planned:** 3 stateless instances behind HAProxy; static count, no
autoscaling (scaling up is an operator decision so an autoscaler cannot
hide a bug); rolling deploy one at a time with a 60 s drain; 30 s graceful
shutdown for in-flight requests and SSE (clients reconnect).

**Clock skew is a correctness precondition, not hygiene.**
`chartWindow.covered` (`internal/api/v1/chart.go`) stops a chart's
multi-source walk once it holds as many distinct buckets as the window's
closed-bucket count, computed from the API host's clock. It is exact today
only because the API and Postgres share one host. Once API instances run on
other hosts, NTP skew of one bucket width or more lets an incomplete set
(26 of 27 buckets) satisfy the predicate, and the series is silently
truncated: a clean `covered` stop does not set `flags.stale`. Before
splitting the API out: pin every API host and the database to the same NTP
source, and alert when offset exceeds the narrowest served granularity
(60 s for `1m`), as a chart-correctness alert. The same precondition breaks
if a non-CAGG reader joins the walk.

### 3.7 stellarindex-aggregator

- **One instance.** `cmd/stellarindex-aggregator` has no leader election
  and no standby. At startup it takes the Postgres session advisory lock
  `hashtext('instance:stellarindex-aggregator')`
  (`timescale.Store.HoldInstanceLock`). While another process holds it, the
  aggregator exits non-zero. It re-checks every 30 s, re-takes the lock on
  a new session if its session dies, and exits if another process got
  there first. `-dry-run` takes no lock. This is exclusivity, not failover.
- **Role:** each tick (default 30 s) computes VWAP/TWAP and confidence from
  the indexer's output and writes Redis and Timescale.
- **Failure:** prices serve stale cached values until systemd
  (`Restart=on-failure`, `RestartSec=10s`) or an operator restarts it.

### 3.8 stellarindex-indexer

- **One process**, not one per source. It walks ledgers once and the
  dispatcher fans each ledger to every registered decoder. Off-chain
  CEX/FX connectors run as goroutines in the same binary, outside the
  dispatcher.
- **Enforcement:** as §3.7, with `hashtext('instance:stellarindex-indexer')`.
- **Cursors** persist per source in Postgres; a restart resumes from them.
- **Catch-up** follows the replay rule in
  [ingest-pipeline.md](ingest-pipeline.md#the-replay-decision-rule):
  `projector-replay` for projected domains, `ch-rebuild` for the rest.
- **Failure:** one source stalls → its freshness alert fires and affected
  pairs lose redundancy; other sources continue.

### 3.9 ClickHouse lake

- **One instance per region**, not a cluster: plain/Replacing/Aggregating
  MergeTree on one local disk, no `Replicated*` tables, no Keeper. In a
  region the lake is a single point of failure by design; HA comes from
  cross-region failover (§1).
- **Blast radius:** the API's ClickHouse readiness check is non-critical
  (`clickhouseChecker.Critical()` in `cmd/stellarindex-api/main.go`). With
  ClickHouse down, `/readyz` returns 200 `degraded`, pricing serves from
  Postgres + Redis, and lake routes return 503.
- **Cross-region:** R2 and R3 proxy every lake route to R1's API (one extra
  round trip per request, not per query). Object storage is read only while
  R1 is unreachable. An R1 lake outage degrades deep-history reads
  everywhere; it does not take pricing down anywhere.
- **Recovery and bootstrap:** restore the latest `ch-lake-backup` chain
  (§8), pass `stellarindex-ops verify-lake`, then follow live ingest.
  Re-deriving from the archive (~1–2 weeks per region) is the last resort.
  A new region bootstraps only from R1's verified lake; seeding from an
  unverified one copies its gaps.

### 3.10 Migrations and the ops CLI

`stellarindex-migrate` runs before each deploy under a Postgres advisory
lock, so two migrators cannot race. `stellarindex-ops` is the operator CLI;
the runbooks name the subcommand each needs.

---

## 4. Capacity

**Traffic envelope.** Target 500 rps sustained and 2,000 rps burst, about
30× a 10,000-daily-active baseline (~6 rps). Most handlers are a Redis read
plus JSON encode.

**r1 footprint.** Storage is the binding constraint. The pool is 4 ×
7.68 TB NVMe in raidz1; the ClickHouse lake is the dominant store
(14.6 TiB `bytes_on_disk`, 2026-09-28), then the galexie archive, then
Postgres and pgBackRest. Live ingest adds roughly 10–20 GiB/day. Current
figures: [`../operations/r1-deployment-state.md`](../operations/r1-deployment-state.md).

### Headroom levers

r1 is not hardware-upgradeable: fixed 4 × 7.68 TB NVMe, no fifth drive,
no raidz expansion (ADR-0027). The levers are software-only, in priority
order:

1. **Trim local `galexie-archive`** behind the ADR-0027 cold tier
   (`s3_cold_bucket_archive`; reads fall through to the free public
   dataset). Biggest single lever.
2. **`tx_hash` → `FixedString(32)`** in ClickHouse (~0.6–1 TiB). A schema
   and binary migration, not started (INV-1098).
3. **ZSTD recompression** of the remaining LZ4 XDR and hash columns. The
   four largest tables are done (~3.8 TiB reclaimed).
4. **TimescaleDB compression policies** on the compression-eligible
   hypertables that have none (~0.2–0.4 TiB).
5. **pgBackRest diff-retention prune** on repo1 (~1 TiB), only once the
   off-site repo has a verified restore (INV-1100).
6. **Horizontal growth:** R2 coming online, not a bigger r1.

---

## 5. Failure matrix

| Component dies | Today (r1) | Planned (Phase 1 / multi-region) |
|---|---|---|
| API process | systemd restarts in ~5 s; full outage meanwhile | HAProxy ejects it within ~15 s; the other instances serve |
| Edge | Caddy down = full outage | HAProxy host → VIP moves in 1–4 s; region → Cloudflare fails over to another region |
| Redis | Cache misses fall through to Postgres; `/readyz` 200 `degraded`; pricing serves | Sentinel failover 15–30 s, `stale: true` on affected keys |
| Postgres | Full outage of served data; restore from pgBackRest (§8) | Patroni promotes a replica, 30–60 s |
| ClickHouse | Lake routes 503; pricing unaffected | Same in-region; other regions keep serving recent data and lose deep history until R1 returns |
| Galexie / captive core | Ingest stops; API serves stale-flagged data; systemd restart + catch-up | Same per region; other regions unaffected |
| Indexer | Ingest stops; restart resumes from cursors | Same per region |
| Aggregator process | Price hot keys stop refreshing; reads serve `stale: true`; no standby (§3.7); systemd `RestartSec=10s` | Same — one instance per region |
| Whole r1 | Total outage; rebuild from off-site backups (§8), hours to days | Traffic fails over to R2/R3 for pricing; lake routes degrade to the object-storage fallback |

No single-component failure in the Phase 1 shape breaches 99.9 % a month,
except a crash-looping aggregator, which leaves prices stale until an
operator fixes it. Today r1 is a single point of failure for everything.

---

## 6. Security posture

- **Secrets:** Ansible Vault; `configs/ansible/inventory/<region>.secrets.yml`
  holds them and is never committed. Validator keys are never on disk
  unencrypted (ADR-0004).
- **External TLS:** Caddy on r1; HAProxy in the Phase 1 shape.
- **Internal traffic today** stays on one host. In the multi-host build:
  etcd uses TLS + client certs (on by default); the Patroni REST API is
  firewalled with Basic Auth; Redis authenticates but has no TLS (open,
  §3.4); Promtail → Loki is HTTP on the internal network.
- **Loopback-only admin surfaces:** HAProxy stats `:8404`, Prometheus
  `:9090`, Alertmanager `:9093`, Loki `:3100`. Operators SSH-tunnel.
- **Firewall:** each role opens its ports to the internal range only.

---

## 7. Observability

### 7.1 Metrics and alerting

**Today (r1, managed outside the roles):**
- Prometheus: `configs/prometheus/prometheus.r1.yml`, rules in
  `configs/prometheus/rules.r1/`, kept in lockstep with
  `deploy/monitoring/rules/` by `scripts/ci/lint-rule-equivalence`.
- Alertmanager: `configs/alertmanager/alertmanager.r1.yml` → Discord
  receivers plus a dead-man's-switch webhook.
- Healthchecks.io pushes catch total death only.
- `archival-node/tasks/23-local-prometheus.yml` is an optional
  single-host scrape path (`run_local_prometheus`, default false).
- No distributed tracing.
- No exporter publishes Galexie export lag in ledgers: an export that falls behind the network but keeps advancing does not alert; only a stop does.

**Planned (role `configs/ansible/roles/prometheus/`):** two independent
Prometheus + Alertmanager hosts (`prometheus_pair`). Each scrapes every
target; Alertmanagers gossip on `:9094` and dedupe. Prometheus does not
cluster, and two independent copies survive one host's loss without
Thanos. Retention 30 days. Rule files are copied from
`deploy/monitoring/rules/` and validated with `promtool` before reload.
Scrape targets come from inventory groups (no service discovery), so adding
a host means re-running the role. The role cannot run on r1 without a
single-host code path. Deferred: Thanos long-term storage (INV-1054) and
cloud-Prometheus federation for DR, which needs a second region
(INV-1055).

### 7.2 Logs

**Today:** a hand-installed single-host Loki on r1
([`configs/loki/README.md`](../../configs/loki/README.md)). Promtail is
ansible-managed by `archival-node/tasks/10-observability.yml`.

**Planned (role `configs/ansible/roles/loki/`):** single-host Loki, chunks
in an S3-compatible `loki-chunks` bucket the operator creates first (the
role only probes it), BoltDB index, 30-day retention owned by the
compactor — never add a bucket lifecycle rule, it races the compactor.
Preflight checks time sync and ≥ 50 GB free on `/var`. Promtail needs
`systemd-journal` group membership; wiping
`/var/lib/promtail/positions.yaml` re-ships the whole journal. The
server/agent playbooks are still placeholders (INV-1045). HA path when
needed (INV-1062): `replication_factor` 2, `memberlist` KV, TSDB index on
S3, a second host; existing chunks need no migration.

### 7.3 Load testing (k6)

Built in `test/load/`: scenarios `00`–`09` and `99-spike` under
`test/load/scenarios/`, shared thresholds and the production-target guard
in `scenarios/lib/`. Run with `make test-load` / `test-load-mixed` /
`test-load-<name>` against `$K6_TARGET`; `make test-load-check`
compile-checks every scenario without a target.

- **Canonical proof:** `06-mixed-realistic.js`. Its weighted endpoint
  mix and the reasoning behind it are in the file header.
- **Decisions:** k6 because its Prometheus remote-write output graphs a
  load run on the same dashboards as production; self-hosted runner;
  `ramping-arrival-rate` rather than VU-count control; warm the cache
  before measuring; the spike scenario posts an Alertmanager silence for
  its window.
- **Where it runs:** `k6-weekly.yml` is manual-dispatch only. No
  production-shaped load target exists (`K6_TARGET_STAGING` is unset on
  purpose), and the guard refuses production. The weekly SLA proof comes
  from `sla-proof-weekly.yml`, which renders
  `docs/operations/sla-proof-<date>.md` from the on-host SLA probe's
  series. Procedure:
  [`../operations/sla-proof-procedure.md`](../operations/sla-proof-procedure.md).

### 7.4 Status page

Built: `stellarindex.io/status` is a route in the explorer
(`web/explorer/src/app/status/`, postmortems at `status/incident/[slug]`).
Incidents are Markdown files in `internal/incidents/data/`, rendered by
`web/explorer/src/lib/incidents.ts` and embedded in the API binary so
`stellarindex-ops emit-incident` sends `incident.sev1` /
`incident.resolved` webhooks from the same source. `status.stellarindex.io`
is a redirect-only Cloudflare Pages stub (`web/status/public/_redirects`).
Operator steps: [`runbooks/sev-status-page-update.md`](../operations/runbooks/sev-status-page-update.md).

Why not a SaaS (Instatus was the original pick): webhooks are the primary
channel, so the page is an archive, not a live UI, and one commit drives
both page and webhook. Behaviour: `severity:` sets the card (SEV-1 major,
SEV-2 minor, SEV-3 maintenance); no email list (no PII) — subscribers use
the Atom feed `GET /v1/incidents.atom` or dashboard webhooks; the git
corpus is the permanent record. Revisit if the team opens 5+ incidents a
week; migrating is "replay the Markdown corpus into the vendor".

---

## 8. Backup & restore

Current state on r1:

| Asset | Mechanism | Off-site | RPO | Restore |
|---|---|---|---|---|
| Postgres | pgBackRest `repo1` at `/var/lib/pgbackrest` (same ZFS pool as the DB): full Sunday, diff Mon–Sat 02:00 UTC, continuous async WAL archiving | **Live:** `repo2`, encrypted, on AWS S3 since 2026-08-29, moving to B2. Rendered only when `pgbackrest_repo2_s3_bucket` is set (`configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml:130`); staleness alert `stellarindex_backup_offsite_stale` | 5 min (WAL) | ~1–3 h |
| ClickHouse lake | `scripts/ops/ch-lake-backup.sh`: native `BACKUP DATABASE` to an `s3_plain` disk, 28-day full + daily incrementals | **Not running:** installed, but backs nothing up until `ch_lake_backup_s3_endpoint` and its vault keys are set; `stellarindex_ch_lake_backup_stale` tickets each lake host until then | daily (target) | ~4–36 h (10 Gbps / 1 Gbps); re-derivation ~1–2 weeks is the last resort |
| Raw galexie archive | Not backed up by design; re-pulled from `aws-public-blockchain` (`galexie_archive_mirror_enabled: false`) | — | — | days to weeks |
| Config, vault, systemd | No job exists | — | — | — |
| Redis | AOF only; cache | — | — | re-warms from Postgres |

Provider, cost and sequencing:
[`../operations/off-site-backup-plan.md`](../operations/off-site-backup-plan.md#provider).

**Restore drill.** Automated and monthly, per ADR-0043 §3. The timer fires
on the first Saturday at 04:00 UTC
(`configs/ansible/roles/archival-node/templates/systemd/restore-drill.timer.j2:27`);
the enable task and its rationale are at
`configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml:691-703`.
The drill's precondition check refuses (exit 2, not counted) when free
space is short. It emits only `last_success_unix` and `failures`, not
throughput, so a lake-scale restore time is unmeasured. Evidence goes to
`docs/operations/drills/`.

**Principle.** Back up what is costly to rebuild. The archive is public
data and is re-pulled. Postgres and the lake are derived, but rebuilding
them takes days to weeks, so they are backed up for RTO. The off-site lake
snapshot doubles as the R2/R3 bootstrap source. A warm standby beats any
restore for downtime; backups are the floor.

---

## 9. Degradation modes (what we promise under failure)

Responses carry advisory flags; none is an error. The authoritative list
and wording is the `Flags` schema in
[`openapi/stellar-index.v1.yaml`](../../openapi/stellar-index.v1.yaml).
The ones failure produces:

| Flag | Set when |
|---|---|
| `stale` | The response is below its surface's freshness baseline, e.g. the aggregator stopped refreshing or Redis failed over |
| `reduced_redundancy` | Cross-region redundancy is degraded (R2/R3 set it when R1's last completeness run is stale, ADR-0017) |
| `frozen` | Anomaly detection refused to publish a new value (ADR-0019) |
| `divergence_warning` | An anomaly check or cross-reference failed its bound |

The value is always the best available. `stale: true` means "last known
good, decide accordingly".

| Scenario | Response |
|---|---|
| Ingest down in a region | Pricing serves the last closed buckets with `stale: true` until ingest catches up |
| ClickHouse down | Lake routes 503; pricing and `/readyz` (200 `degraded`) unaffected |
| Redis down | Reads fall through to Postgres; slower, still correct |
| R1 down (multi-region) | R2/R3 serve pricing from local stores; lake routes read the object-storage fallback; authenticated traffic fails until control-plane replication exists (§3.3) |
| Reconnecting SSE client | Ledger and observation streams have no replay (gap on reconnect); price-stream resume tokens are per region |

What multi-region does not buy:

- An application-layer bug (a miscomputed VWAP) or a poisoned upstream replicates to every region.
- Cloudflare is the single edge dependency (no multi-CDN at launch), and writing endpoints such as `/v1/account/keys` are read-only during failover.

---

## 10. Roadmap and launch checklist

R2/R3 and in-region HA are deferred past v1.0 (ADR-0050 §0c). The real
exposure is r1 as a single point of failure, so a second origin is bought
for availability first.

**Prerequisites** (nothing multi-region lands before these):

| Workstream | State |
|---|---|
| Off-site DR | Postgres `repo2` live; lake backup waits on its B2 bucket (§8) |
| Determinism hardening | OHLC tiebreak shipped (`migrations/0147_ohlc_deterministic_tiebreak.up.sql`); `account_movements` FINAL closed as not needed (duplicates are identical tuples); stripping wall-clock watermarks from served responses still open; a cross-region divergence prober is the evidence it holds |
| SLO guard | Shipped: `internal/api/v1/slo_guard_test.go` fails if an SLO'd handler gains a remote-ClickHouse, cross-region or S3 dependency |
| Lake-aware health and routing | Signal shipped (`/v1/livez/lake`); path-steering not built |
| Control-plane replication | Not started (§3.3) |
| Multi-region inventory and deploy | r2/r3 not in `deploy.yml`; inventories are examples only |

**Sequence when it resumes**, cheapest first:

1. Cloudflare in front (WAF + cache).
2. Test a 1–5 s `s-maxage` on `/v1/price` and `/v1/oracle/latest`. Closed
   buckets may allow it; if so, much of the API is edge-servable and R3's
   case weakens.
3. R2 (US): removes the single point of failure. Independent ingest,
   pricing active/active, lake proxy to R1, control-plane replication,
   added to the LB.
4. R3 (Singapore), only on evidence of Asian API usage by endpoint.
5. Global failover: lake-aware routing for API and explorer, scheduled
   failover drills.

In-region HA on R1 (ADR-0050 Phase 1) goes before R2 if still wanted (§11).

**Launch checklist for the HA build** (INV-1101; none green yet):

- [ ] Patroni failover drilled end to end in staging (primary OOM).
- [ ] Redis Sentinel failover drilled under load.
- [ ] Load test at 2,000 rps with p95 ≤ 200 ms on cached endpoints.
- [ ] Point-in-time restore to 24 h ago in under 2 h wall clock.
- [ ] DR drill: fail over to another region, serve for 1 h, fail back.
- [ ] Every alert has a runbook link.
- [ ] SEV-1 and SEV-2 playbooks rehearsed as a tabletop.
- [ ] Pre-flip gate for a new region: `scripts/dev/verify-cross-region.sh` and `stellarindex-ops cross-region-check` show byte-identical closed-bucket VWAPs, then hold DNS for a 24 h clean window.

The load-test and restore items can run on a single box now.

---

## 11. Open decisions

1. **Rate limit and quota across regions** (ADR-0050 §3d), due before a
   second region serves authenticated traffic. Options: per-region limits
   (the 429 body must say so); limit ÷ N per region (a failover cuts a
   caller to 1/N); monthly quota reconciled through the replicated control
   plane with bounded lag. Rejected: a synchronous cross-region counter (a
   WAN round trip on every request).
2. **Is in-region HA on R1 still wanted** once R2 provides cross-region
   failover? ADR-0050 §8 says one box per region; §10 still lists the
   Phase 1 build.
3. **Redis in-flight encryption** for the multi-host build (§3.4).
4. **Cross-region SSE resume**: accept the gap, or add a portable
   `Last-Event-ID` cursor.
5. **Patroni implementer questions** (INV-1053) and **PgBouncer**
   (INV-1049): see §3.3.

---

## 12. Cost

Annual, list price (committed pricing ~30–40 % lower), verified 2026-08-21:

| Region | Shape | ~Annual |
|---|---|---|
| R1 (Hetzner, existing) | primary + lake authority | ~$5,000 |
| R2 (Vultr US bare metal) | pricing + lake proxy | ~$4,200 |
| R3 (Vultr SG bare metal) | pricing + lake proxy | ~$4,500 |
| Off-site backups | lake + Postgres on B2 | see the backup plan |
| **Fleet, one box per region** | | **~$15,000–18,000** |

Excluded: user-facing egress, and the one-time ~1–2 week backfill per
region. The $180–288 K/yr figure in older documents is the per-region HA
fleet ADR-0050 rejected.
