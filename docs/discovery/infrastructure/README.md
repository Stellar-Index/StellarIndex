# Infrastructure & HA design

**Status:** 🧪 Phase-6 scope but started early per user directive to
design to enterprise-grade. Documented here **before** Phase 2 coding
so that infrastructure constraints inform our data-model + ingestion
choices, not the other way round.

This directory covers:

```
infrastructure/
├── README.md                 — this index
├── topology.md               — overall colo + cloud hybrid
├── storage-timescaledb.md    — primary time-series store
├── cache-redis.md            — hot-path L1 cache + rate limit bucket
├── storage-minio.md          — Galexie S3-compatible data lake
├── core-rpc-galexie.md       — Stellar stack layout (already covered
                                in data-sources/archival-nodes.md; this
                                captures multi-instance topology)
├── api-layer.md              — API gateway + reverse proxy + TLS
├── observability.md          — Prometheus, Grafana, Alertmanager, Sentry
├── backup-dr.md              — backups + disaster recovery
├── load-test-plan.md         — pre-launch SLA validation
├── sev-playbook.md           — SEV-1 / SEV-2 incident response
├── capacity-plan.md          — headroom, scaling triggers
└── secrets-mgmt.md           — HSM, key material, validator seeds
```

## Design principles

### 1. Correctness before performance

Our i128 invariant (see [../decisions.md](../decisions.md)) is the
canonical example: we don't "optimise" by storing amounts as `int64`.
Every layer preserves precision end-to-end.

### 2. Boring tech, proven at scale

- **PostgreSQL + TimescaleDB** — 20+ years of Postgres operational
  knowledge, Timescale is a thin extension. Not DuckDB. Not Cassandra.
  Not "interesting."
- **Redis** — for hot cache + rate limit buckets. Not Memcached.
- **MinIO** — S3-compatible on baremetal. Not Ceph. Not GlusterFS.
  Not raw NFS.
- **Prometheus + Grafana + Alertmanager** — standard SRE stack.
- **Go** — one language for the whole server. Not "part Rust part Go."

Enterprise deployments by third parties will recognise every piece.

### 3. Self-hostable by default

Our open-source story requires that **every component** has a
baremetal-installable equivalent. No managed-service hard
dependencies. If a customer wants to run on-prem, they can.

- MinIO = S3-API on-prem.
- TimescaleDB = self-hostable Postgres extension.
- Prometheus / Grafana = self-hostable.
- Reverse proxy = Traefik / nginx / Caddy.

When we offer our own hosted service, it uses the same binaries. No
feature skew between hosted + self-hosted.

### 4. Redundancy at every layer

Single points of failure we explicitly eliminate:

- stellar-core watcher → **two** co-running (one colo, one cloud fail-
  over).
- stellar-rpc → two (colo + cloud).
- Galexie → two (writing to same MinIO bucket with different
  paths; reconciliation via `detect-gaps`).
- MinIO → clustered 4-drive erasure-coded on colo; replicated to a
  cloud-hosted S3 as async backup.
- TimescaleDB → primary + sync replica in same DC, async replica off-
  site.
- Redis → cluster mode, 3-node minimum, Sentinel for failover.
- API pods → N behind a load balancer, N ≥ 2 per region.

### 5. Observability is a deliverable, not an afterthought

Every component exposes Prometheus metrics. Every API response is
sampled into OpenTelemetry traces. Every log line is structured JSON.
Dashboards and alert rules ship in the same repo as the code, not in
a separate "ops" toolkit.

### 6. Data contracts are versioned

- API: versioned URLs (`/v1/...`). Breaking changes = new major.
- Internal message schemas between pipeline stages: protobuf with
  backwards-compatible evolution rules.
- Database schemas: `goose` or `golang-migrate` migrations, never
  hand-run SQL.

## Deployment tiers

Our architecture supports three tiers so we can make production
decisions based on the right tier.

### Tier 1 — Colo (primary)

Baremetal in the Vancouver co-location DC. From the proposal (shoutout
to the Dell R640 story). This is our primary production deployment.

Components:
- 2× stellar-core watchers
- 2× Galexie
- 2× stellar-rpc
- MinIO cluster (4× NVMe drives, erasure-coded)
- TimescaleDB primary + sync replica
- Redis cluster (3 nodes)
- 2× API pods
- Prometheus + Grafana + Alertmanager
- Loki (logs) or Elasticsearch if we need full-text search
- Traefik reverse proxy

Hardware: at minimum 3 physical machines (the existing R640 plus 2
more). Dedicated validator hosts (Phase-3 goal) are separate from
these.

### Tier 2 — Cloud (redundancy + overflow)

AWS or GCP (we're agnostic; pick one and document).

- 1× stellar-core watcher (cold standby)
- 1× Galexie (hot standby)
- 1× stellar-rpc
- S3 bucket (async replica of MinIO)
- TimescaleDB read replica (async)
- 1× API pod
- CDN (CloudFront or equivalent)

Used for:
- Failover if colo has a network event.
- Serving API traffic regionally (lower latency for EU / Asia).
- Async backups + DR.

### Tier 3 — Self-hosted (downstream operators)

Docker Compose and Kubernetes manifests published in our open-source
repo. Anyone can stand up their own instance. Minimal deploy:

- 1× stellar-core watcher
- 1× Galexie
- 1× stellar-rpc
- MinIO single-node
- TimescaleDB single-node
- Redis single-node
- 1× API pod

Hardware: one 32 GB / 8-core / 2 TB NVMe machine is enough. Meets
99 % uptime, not 99.99 %.

## SLO targets (derived from Freighter RFP + proposal)

| SLO | Target | Measurement |
| --- | ------ | ----------- |
| API uptime | ≥ 99.99 % (monthly) | 5xx rate / total requests; measured externally via synthetic checks |
| API latency p95 | ≤ 200 ms | RED metrics per endpoint, Prometheus histogram |
| API latency p99 | ≤ 500 ms | Same |
| Price freshness | ≤ 30 s staleness | `now() - last_update_time` exposed per asset |
| Event ingest lag | ≤ 10 s behind tip of ledger | Prometheus gauge from consumer |
| Backup freshness | RPO ≤ 1 h | Timescale WAL archive age |
| Disaster recovery | RTO ≤ 1 h | Practiced quarterly |

## Capacity targets

- **1000 req/min per API key** (RFP floor). With ~100 keys at peak,
  that's ~1666 req/s total. Our API pods should comfortably serve
  5× headroom = ~8000 req/s across the fleet.
- **Per-pair live aggregation**: ~200 tracked asset pairs × ~5-10
  updates per window = ~1000 agg writes/s to Redis, ~10 writes/s
  to Timescale continuous aggregates.
- **Event ingest**: pubnet currently closes ledgers every ~5 s. Per
  ledger: hundreds of operations, tens of events. Sustained ~20-50
  events/s on our consumer; peak spikes to ~500/s.

Everything under those numbers fits comfortably on modest hardware.

## Sub-docs (to be written)

Each of the below gets its own doc in this directory, to be filled in
by the next iterations of this loop:

- [ ] `topology.md` — wire diagram of colo + cloud.
- [ ] `storage-timescaledb.md` — hypertable schemas, retention
      policies, continuous aggregates, replication.
- [ ] `cache-redis.md` — keyspace design, eviction, cluster config.
- [ ] `storage-minio.md` — cluster layout, bucket structure, S3-compat
      testing.
- [ ] `api-layer.md` — gateway choice, TLS, rate limit, auth.
- [ ] `observability.md` — metrics names, dashboards, alert rules.
- [ ] `backup-dr.md` — RPO/RTO, WAL archiving, restore drills.
- [ ] `load-test-plan.md` — k6 or Vegeta, scenarios, pass/fail.
- [ ] `sev-playbook.md` — SEV-1 / SEV-2 runbook.
- [ ] `capacity-plan.md` — scaling triggers, cost model.
- [ ] `secrets-mgmt.md` — HSM, validator keys, API-key rotation.

## Related

- [../decisions.md](../decisions.md) — foundational decisions that
  constrain infrastructure (i128, MinIO, Horizon excluded, Tier-1
  validator aspiration).
- [../data-sources/archival-nodes.md](../data-sources/archival-nodes.md)
  — the Stellar stack layout.
- [../rfp-requirements-matrix.md](../rfp-requirements-matrix.md) —
  which SLA / infrastructure rows the infrastructure serves.
