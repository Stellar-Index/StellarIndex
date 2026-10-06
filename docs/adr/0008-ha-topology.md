---
adr: 0008
title: Per-region HA topology
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0008: Per-region HA topology

## Context

The availability target forces per-component redundancy and a graceful-degradation contract for when
a plane fails. [docs/architecture/ha-plan.md](../architecture/ha-plan.md) holds the full design; this
ADR binds its load-bearing decisions for per-region infrastructure.

## Decision

1. **Per-region HA is the shape; multi-region is ADR-0050.** Each region runs full HA, with cold DR in
   the cloud. The original "multi-region active/active out of scope for v1" call was overturned by
   ADR-0050; [docs/architecture/ha-plan.md](../architecture/ha-plan.md) governs
   anything multi-region, and this topology is its Phase 1.
2. **Three tiers, three failure domains.** Hot: Redis with Sentinel, not Cluster (ADR-0007,
   ADR-0024). Warm: Patroni-managed TimescaleDB, one primary plus two sync replicas. Cold: MinIO
   erasure-coded EC(6+3) across 9 hosts, bucket versioning on. Ingest never blocks serving: when
   ingestion slows the API returns stale-marked responses (ADR-0015, `flags.stale=true`), never errors.
3. **N+1 minimum; N+2 for Galexie's captive-core**, so the three archives ADR-0004 needs survive a
   host failure during a maintenance window.
4. **Process shape.** `stellarindex-api` runs as N=3 stateless instances behind HAProxy with a
   keepalived VIP. `stellarindex-indexer` is one process: it walks ledgers via `internal/ledgerstream`
   and `internal/dispatcher` fans each ledger to every decoder. `stellarindex-aggregator` is one
   process with no leader election and no standby, and the standby is not part of Phase 1. A second
   copy against the same database refuses to start because the first holds the Postgres instance lock
   `hashtext('instance:stellarindex-aggregator')`; the lock gives exclusivity, not failover, and
   systemd restarts a dead process.
5. **Colo primary, cloud DR.** Galexie, Postgres and MinIO run on dedicated colo hardware (cloud is
   about 3x the cost at our IOPS). The AWS DR target holds warm-standby stateless services
   (scale-to-zero, scale-out on DNS flip), an async logical Postgres replica (5-minute RPO), and
   `mc mirror` of MinIO to S3 (1-hour RPO; `galexie-live/` at 5 minutes). Redis is not replicated
   and re-hydrates from Timescale; stellar-core is not replicated and is rebuilt from our MinIO
   archive (about 4 h to `CATCHUP_RECENT`).
6. **Every component has a defined degraded mode.** Source down: `sources` marks the outage and
   `flags.reduced_redundancy=true`. Aggregator down: last published row with `flags.stale=true` and
   `as_of`. Redis down: query Timescale directly with `flags.stale=true`. Timescale primary down:
   Patroni fails over to a sync replica (RPO 0, RTO about 30 s). Regions disagree on a closed bucket:
   `cross-region-monitor` alerts and each region serves its local view.
7. **Availability.** The published commitment is ≥ 99.9 % over a 30-day month
   ([stellarindex.io/sla](https://stellarindex.io/sla)). The 99.99 % in the original ADR is the
   design target the three-node topology was sized against, not a commitment; revisit it only once that
   topology ships and an off-host probe has measured it for 30 days. Colo plus cloud DR cost of at
   most $80k/year is an informative target; the shape is binding.

## Invariant

The aggregator's only exclusivity control is the Postgres instance lock named above; docs describing
its topology stay pinned to it (`TestAggregatorTopologyDocsMatchInstanceLock`,
`internal/ops/chops/ha_plan_citations_test.go`).

The published availability figure, the burn-rate alert budget, the sla-probe default and the operator
docs agree on 99.9 % (`internal/ops/chops/sla_figure_consistency_test.go`).

A change to any `flags.*` value needs an explicit ADR or a test pinning the contract; a handler that
reads cold-tier storage synchronously on a hot-path request violates the tier contract.

## Consequences

Self-hosted hardware plus cloud DR covers the commitment without vendor lock-in, and the tier split
makes the degradation contract reviewable. It costs operational complexity (Patroni, keepalived,
Sentinel, mirror schedules, two failure-mode sets) and manual hardware refresh. A new service should
declare its tier, redundancy and degraded mode; backup retention, alert thresholds and
failover procedures live in `docs/operations/runbooks/`. Full cloud, single-replica Postgres,
stellar-core in AWS and serverless aggregation were rejected.

## Evidence

`cmd/stellarindex-aggregator/main.go` (`HoldInstanceLock`); `deploy/monitoring/rules/slo.yml`
(budget 0.001, label `api_availability_3_nines`); the two chops tests above.
