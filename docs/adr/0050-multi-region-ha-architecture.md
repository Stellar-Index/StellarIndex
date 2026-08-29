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

Deciders: @ash (2026-08-21). Ratifies the program plan in
[`docs/architecture/multi-region-ha.md`](../architecture/multi-region-ha.md),
which holds the full detail, cost model, phasing, and audit provenance. This ADR
records the **decision** and the **supersessions**.

## Context

Ash decided to bring genuine multi-region HA before v1, with cross-region failover
for both the API and the explorer. A cold adversarial audit (6 auditors,
2026-08-20/21) validated a first-draft plan against the actual code and the live R1
host and refuted its load-bearing claims. Two measurements forced the architecture:

1. **No cross-region replication exists or is buildable for the lake.** Postgres is a
   standalone primary (zero replicas/slots/publications); ClickHouse has no Replicated
   tables. The Model A of ADR-0016/`multi-region-topology.md` ("R1 canonical history via
   Postgres replication") is paper and cannot cover the 14.6 TiB ClickHouse lake at all.
2. **The lake cannot be active/active cheaply.** Per-region S3-tiering fails on cost
   (1,000–8,000 S3 GETs per cold explorer page) and latency (breaches the 8 s route
   budget); and R3's 1.75 TiB disk physically cannot hold the lake.

The prior ratified position (ADR-0008: "multi-region active/active is out of scope for
v1") is overturned by Ash's decision; this ADR replaces it.

## Decision

Adopt the three-tier architecture detailed in `docs/architecture/multi-region-ha.md`:

- **Model B — independent per-region ingest.** Each region ingests the chain itself and
  builds its own stores; consistency is by **determinism** (ADR-0015), not replication.
  We build **no** cross-region Postgres replication and **no** stretched Patroni cluster.
- **Pricing/oracle tier → active/active in all 3 regions** (local Timescale+Redis). The
  agreed SLO (ADR-0009, p95≤200/p99≤500) is preserved because **no SLO'd route crosses a
  region boundary** — enforced by a guard test.
- **Lake/explorer tier → R1 is the authority.** R2/R3 serve the hot set locally and
  **proxy cold/archive queries to R1**, with Cloudflare R2 as a **fallback-only** copy
  (not the steady-state path). R3 holds no local lake (disk-bound); it is a thin proxy.
  **Rejected: per-region S3-tiered ClickHouse.** Lake failover is fast-normal,
  degraded-on-R1-outage — the correct trade for 14 TiB.
- **Control-plane state** (accounts, API keys, sessions, webauthn, alerts, webhooks) gets
  **real cross-region replication** as its own workstream; determinism cannot reproduce
  it. Until it lands, only anonymous traffic fails over cleanly.
- **HA model = cross-region failover, one box per region.** Not full per-region HA fleets
  (the $180–288 K/yr topology we reject).
- **R2 moves off AWS** to cheap US bare metal (Vultr, matching R3). AWS was justified only
  when R2 held the full lake; it no longer does.
- **Durability crown jewel = the 2.49 TiB raw galexie-archive**, replicated off-site to
  provider-independent storage (Cloudflare R2/Backblaze). Everything else is a derived,
  re-ingestable projection (consistent with ADR-0043, which rejects backing up the derived
  lake). Recovery/bootstrap = re-ingest from the archive.

Fleet cost: **~$15–18 K/yr** (single box per region). Full detail, phasing (Phase 0–4),
per-region shapes, and the prerequisite workstreams (determinism hardening, lake-aware
health, off-site archive DR, greenfield HA foundation, multi-region inventory/deploy) are
in the plan doc.

## Consequences

- The four multi-region documents (`multi-region-topology.md`, `r2-r3-bringup.md`,
  `multi-region-cutover.md`, and ADR-0016) described architectures we deliberately reject.
  They are superseded and banner-marked; **implementation must follow the plan doc, not
  them**, to avoid accidentally building Model A / per-region S3 lake / R2-on-AWS.
- ADR-0008's single-region HA topology and DR principle **carry forward** (Phase 1); only
  its multi-region active/active decision is overturned.
- ADR-0044 (explorer edge SSR) becomes load-bearing: it is the enabler for runtime
  cross-region explorer failover.
- New prerequisite work is created (determinism fixes, lake-aware `/livez/lake`, off-site
  archive DR, control-plane replication) — all tracked in the plan doc's Phase 0/§7.

## Alternatives rejected

- **Model A (cross-region Postgres replication + R1-canonical):** doesn't exist, and
  cannot cover the ClickHouse lake. (ADR-0016, topology doc.)
- **Per-region active/active lake via S3-tiered ClickHouse:** cost + latency failure
  (audit B); impossible on R3's disk (audit D).
- **Full per-region HA fleets (~$180–288 K/yr):** overkill for scale/cost; cross-region
  failover with one box per region meets the need.
- **Keep R2 on AWS:** ~3× the cost for a role that no longer needs elastic lake storage.

## Amendment — 2026-08-29 (API-first)

Ash: *"api is the main purpose really, people can wait 200 ms for explorer results."*
R2's optional hot lake set is dropped for v1 — R2 and R3 are the same shape (local
pricing + Redis + API, all lake routes proxied to R1 at request level), which lowers
both boxes to ~2 TB commodity bare metal. In exchange, **determinism hardening
(plan §7.2) is promoted to a launch gate**: with the API as the product and three
regions answering independently, cross-region answer equality is a correctness
promise, not an optimisation. See `docs/architecture/multi-region-ha.md` §0b.
