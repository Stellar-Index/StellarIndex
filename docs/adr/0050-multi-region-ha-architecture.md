---
adr: 0050
title: Multi-region HA — active/active pricing, R1-authority lake, provider-independent archive DR
status: Accepted
date: 2026-08-21
supersedes: [0016]
amends: [0008]
superseded_by: []
---

# ADR-0050: Multi-region HA architecture

## Context

No cross-region replication exists or is buildable for the 14.6 TiB ClickHouse lake, and a per-region S3-tiered lake fails on cost, latency and R3's disk. ADR-0008 had ruled multi-region active/active out of v1 and ADR-0016's Model A (R1-canonical Postgres replication) does not exist.
The full plan, cost model and phasing live in `docs/architecture/multi-region-ha.md`; implementation is deferred until after v1.0 (plan §0c), so the build is not in the tree yet.

## Decision

Section labels match the plan doc and are cited from code.

1. **Model B: independent per-region ingest.** Each region ingests the chain itself and builds its own stores; consistency is by determinism (ADR-0015), not replication. No cross-region Postgres replication and no stretched Patroni cluster.
2. **§3a: pricing and oracle tier is active/active in every region** on local Timescale and Redis. The ADR-0009 SLO holds because no SLO'd route crosses a region boundary.
3. **§3b: the lake and explorer tier is R1-authoritative.** R2 and R3 are the same shape: local pricing, Redis and API, with lake routes proxied to R1 and Cloudflare R2 as fallback only. Per-region S3-tiered ClickHouse is rejected.
4. **§3c: control-plane state** (accounts, keys, sessions, webauthn, alerts, webhooks) needs real cross-region replication as its own workstream; until it lands only anonymous traffic fails over cleanly.
5. **§3d (the rate-limit amendment): rate limit and monthly quota count per region,** in the serving region's own Redis, keyed per principal with no region dimension, so under active/active the global ceiling is N regions times the limit and a Redis outage fails closed in that region only. The mitigation is an open decision due before a second region serves authenticated traffic; a synchronous cross-region counter is excluded for the rate limit because it would put a cross-region round trip on every request.
6. **§4: per-region shapes** differ in storage but serve byte-identical closed-bucket answers (ADR-0016's surviving principle). R2 moves off AWS to US bare metal.
7. **§5: durability.** The raw galexie-archive is a copy of SDF's public dataset and is re-pulled from there, not mirrored off-site (`galexie_archive_mirror_enabled` defaults to false). Off-site copies are the ClickHouse lake and Postgres on Backblaze B2 (ADR-0043).
8. **§7.3: lake-aware health.** `GET /v1/livez/lake` is the load-balancer probe for lake-backed routes; `/v1/readyz` covers the pricing tier.
9. **HA is cross-region failover with one box per region;** ADR-0008's single-region HA topology and DR principle carry forward as Phase 1 (HAProxy, Patroni, Sentinel fleets, deferred post-v1.0); the multi-region layer adds failover between single-box regions, never a stretched cluster.
10. **Determinism hardening is a launch gate** for multi-region (plan §7.2): three independent regions must return equal answers.

## Invariant

- SLO'd handlers never read lake-backed fields or cross a region boundary: `TestSLORoutesNeverTouchTheLake` in `internal/api/v1/slo_guard_test.go`.
- `/v1/livez/lake` shares one ping per round, fails closed when the lake is absent and never echoes the driver error: `internal/api/v1/livez_lake_test.go`.
- The rate limiter and monthly quota keep per-principal Redis counters with no region dimension: `internal/api/v1/middleware/ratelimit.go`, `internal/api/v1/middleware/monthly_quota.go`.

## Consequences

- ADR-0008's single-region HA topology and DR principle carry forward; only its multi-region decision is overturned, and ADR-0016 is superseded.
- `multi-region-topology.md`, `r2-r3-bringup.md` and `multi-region-cutover.md` describe rejected architectures and are banner-marked; implement from the plan doc.
- ADR-0044 explorer edge rendering is the enabler for runtime cross-region explorer failover.
- Lake failover is fast when R1 is healthy and degraded during an R1 outage; fleet cost is about $15-18K/yr against $180-288K/yr for per-region HA fleets.
- Until a second region serves authenticated traffic the published limits are exact (N = 1).

## Evidence

`docs/architecture/multi-region-ha.md`, `internal/api/v1/slo_guard_test.go`, `internal/api/v1/livez_lake_test.go`, `internal/api/v1/server.go` (`/v1/livez/lake` route).
