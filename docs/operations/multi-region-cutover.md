---
title: Multi-region cutover runbook (L4.14 → L4.17 + L5.8)
last_verified: 2026-05-03
status: superseded by ADR-0050 / [ha-plan.md](../architecture/ha-plan.md) (2026-08-21) — see the banner below; do not run
---

# Multi-region cutover runbook

> ⛔ **SUPERSEDED by ADR-0050 / [`../architecture/ha-plan.md`](../architecture/ha-plan.md) (2026-08-21). Do not run this runbook.** It sequenced a Model A cutover (Patroni cross-region replicas, `pg_is_in_recovery()=t` gates, a 5-node etcd quorum R1×2/R2×2/R3×1) through `configs/ansible/site.yml`, which does not exist. The current bring-up is Model B (independent per-region ingest): the plan doc's Phase 2/3.

What remains useful is the region-independent material below: per-host
provisioning shapes, the Cloudflare load-balancer design and the
region-failover test bars. Per-host bring-up lives in
[`archival-node-bringup.md`](archival-node-bringup.md) §"Per-region
variations". Cross-region answers are byte-identical because of
[ADR-0015](../adr/0015-last-closed-bucket-rate-serving.md) (closed-bucket
values); storage shapes per region are
[ADR-0016](../adr/0016-per-region-storage-strategy.md).

## Stage 1 — L4.14 R2 spinup (AWS us-east-1)

- EC2 r7i.4xlarge (16 vCPU / 128 GiB), 1-year Compute Savings Plan
  (ADR-0016 §"R2 — AWS-hybrid"); 1-2 TB EBS gp3.
- No archive mirror: reads `aws-public-blockchain` through the cold-tier
  knobs `stellarindex_s3_cold_endpoint` / `_region` / `_bucket_archive`
  (rendered as `[storage] s3_cold_*`). `galexie_s3_bucket_archive` is
  read from the same `galexie_s3_endpoint` as galexie-live.
- Inventory from `configs/ansible/inventory/r2.example.yml` (the real
  `r2.yml` is gitignored). Post-install gate: `verify-archive -tier=chain`.
- Pass: `stellarindex-{indexer,aggregator,api}` active and
  `curl -sf http://localhost:8080/v1/healthz` returns `{"status":"ok",...}`.

## Stage 2 — L4.15 R3 spinup (Vultr Singapore)

- Vultr Bare Metal Intel Xeon E-2388G (8c/16t, 128 GB ECC) + 2 × 1.92 TB
  NVMe as a ZFS mirror (ADR-0016 §"R3 — Vultr-hybrid"). R1's own pool is
  raidz1, the same single-drive margin.
- Region-local Vultr Object Storage bucket (~$25/mo for 5 TB) as the cold
  tier; initial fill copies AWS public bucket → Vultr, ~6-12 h
  ([`archival-node-bringup.md §"R3 — Vultr Singapore"`](archival-node-bringup.md)).
- Inventory from `r3.example.yml`. Pass: same as Stage 1.

## Stage 3 — L4.17 Cross-region Postgres replication wired

Model A only (Patroni sync replica R2, async R3). Not part of Model B.

## Stage 4 — L4.16 Cloudflare Anycast / GeoIP

Pre-req ([cdn-setup.md](cdn-setup.md)): `api.stellarindex.io` proxied
through Cloudflare.

- Per-region origins `api-r1|r2|r3.stellarindex.io` → each region's
  frontend, DNS-only (grey cloud).
- Load Balancer pool `stellarindex-api-pool`, the three origins at
  weight=1. Health check `GET /v1/healthz`, 200 within 5s, every 30s;
  3 consecutive failures → unhealthy. Geo Steering EU → r1, NA → r2,
  APAC → r3; fallback r1→r2→r3.
- `api.stellarindex.io` → the Load Balancer, proxied (orange cloud).

Verify each origin, then routing from a remote egress:

```sh
for region in r1 r2 r3; do
  curl -sH "Host: api.stellarindex.io" \
    --resolve api.stellarindex.io:443:$(dig +short api-${region}.stellarindex.io | head -1) \
    https://api.stellarindex.io/v1/healthz | jq .
done
curl -s https://api.stellarindex.io/v1/healthz -w '%{remote_ip}\n' -o /dev/null
```

Rollback: set r2/r3 pool weights to 0.

## Stage 5 — L5.8 Region-failover chaos test

Pre-flight: all regions up; `make verify-cross-region`
([`scripts/dev/verify-cross-region.sh`](../../scripts/dev/verify-cross-region.sh))
passes; maintenance notice from
[`deploy/comms/maintenance-window.md`](../../deploy/comms/maintenance-window.md)
posted if production traffic is routed; [`rollback.md`](rollback.md) open.

Take R1's frontend offline cleanly (not a hard reboot), then poll
`/v1/healthz` every 5 s from a non-r1 host via `--resolve` to a surviving
region.

| # | Check | Bar |
|---|---|---|
| 1 | Cloudflare evicts R1 from the pool | within 90s |
| 2 | `/v1/healthz` from an external network | 200 throughout |
| 3 | `/v1/price` for a popular pair | keeps serving, `flags.stale=false` |
| 4 | `verify-cross-region.sh` after R1 is restored | byte-identical closed-bucket values across all regions |

Record under `test/chaos/reports/<UTC-timestamp>/region-failover.md`
([chaos Wave 1 runbook](chaos-wave1-runbook.md) format): offline/online
timestamps, Cloudflare pool transitions, any `flags.stale=true` and its
duration, any 5xx.

## Cross-references

- [`launch-readiness-backlog.md`](../architecture/launch-readiness-backlog.md) — L4.14, L4.15, L4.16, L4.17, L5.8.
- [`multi-region-topology.md`](../architecture/infrastructure/multi-region-topology.md) — Model A design intent (superseded).
- [`rollback.md`](rollback.md) — failure-mode rollback procedures.
