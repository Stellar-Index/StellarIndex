---
title: Multi-region HA — the current, authoritative program plan
status: authoritative (ratified by ADR-0050, 2026-08-21)
supersedes:
  - docs/architecture/infrastructure/multi-region-topology.md
  - docs/architecture/r2-r3-bringup.md
  - docs/operations/multi-region-cutover.md
  - docs/adr/0008-ha-topology.md (multi-region / active-active decision only)
  - docs/adr/0016-per-region-storage-strategy.md
audit: cold adversarial 6-auditor audit, 2026-08-20/21 (see §13)
---

# Multi-region HA — the current plan

> ⚠️ **This document is the single source of truth for how Stellar Index goes
> multi-region.** It is ratified by **ADR-0050**. If any other document
> (multi-region-topology.md, r2-r3-bringup.md, multi-region-cutover.md,
> ADR-0008's active/active line, ADR-0016) says something different, **that
> document is superseded and this one wins.** Do not implement a second region
> from any other doc — several of them describe architectures we deliberately
> rejected (cross-region Postgres replication, per-region S3-tiered lake, R2 on
> AWS as a full lake holder). Those are dead. This is live.

## 0. Requirements (Ash's decision)

- **Multi-region HA before v1**, with genuine **cross-region failover for both
  the API and the explorer** (not just DR).
- **Regions are deliberately different** (the ADR-0016 principle survives: same
  served answers, different plumbing/role).
- **Cost-efficient.** Meet the need without paying for capacity we don't use.
- The agreed latency SLO (ADR-0009: **p95 ≤ 200 ms / p99 ≤ 500 ms** on
  `/v1/price` + `/v1/oracle/*`) must not regress.

## 0b. Amendment — API-first (Ash, 2026-08-29)

**Decision: the API is the product; the explorer can pay the ocean.** Ash, after
seeing the measured split: *"api is the main purpose really, people can wait 200 ms
for explorer results."*

Consequences, which SIMPLIFY §3b and §4:

- **R2 does not carry a hot lake set.** The "optional hot local set" in §3b/§4 is
  dropped for v1. R2 and R3 are the same shape: full local pricing/Timescale,
  Redis, API — and **every** lake-backed route proxies to R1's API (request-level,
  so a deep page pays ONE extra RTT, not one per query).
- **Disk requirement falls accordingly.** Neither R2 nor R3 needs TiB-scale NVMe:
  the local footprint is Timescale (524 GiB on R1 today) + Redis + OS. A 2 TB
  box is generous, which is why these are the "cheap replicas" of the decision.
- **Determinism hardening (§7.2) becomes launch-blocking, not nice-to-have.** If
  the API is the product and three regions answer it independently (Model B), two
  customers hitting different regions MUST get the same number. The three known
  non-deterministic spots — OHLC `open`/`close` via `first/last` over non-unique
  `ts`, `account_movements` missing `FINAL`/`LIMIT 1 BY`, and wall-clock/region-local
  watermarks in served responses — are the correctness gate for multi-region, and
  a cross-region divergence prober (same request to R1/R2/R3, compare payloads
  modulo documented volatile fields) is the evidence that it holds.
- **Measured basis (2026-08-29, r1).** The pricing/ticker surface is ~99% cache-served
  locally (`list_coins` 77,113 hit / 48 miss; `distinct_pairs` 83,672 / 38;
  `list_issuers` 76,193 / 365; `latest_oracle_updates` 75,281 / 1,207), so it never
  needs R1. Organic explorer traffic (~3,250 requests / 2 h: accounts, asset detail,
  movements) is essentially all lake-backed — those are the requests that will cross
  the ocean, and this amendment accepts that cost explicitly.


## 0c. Status — DEFERRED to post-v1.0 (Ash, 2026-08-29)

**R2/R3 are not on the v1.0 path.** Ash, after working the cost/benefit live:
*"lets put the r2/r3 thing to bed for now, post r1, back to focusing on path to 1.0."*
This section records WHY, so the next session does not re-litigate it from memory.

**What the evidence said (all measured on r1, 2026-08-29):**

1. **A CDN cannot replace regional origins for the product surface** — but NOT
   for the reason this entry originally gave. It claimed `/v1/price`,
   `/v1/oracle/latest` and `/v1/ledgers/latest` "all return `Cache-Control:
   no-store`". All three are wrong at HEAD (corrected 2026-08-31, wave-D PS-07):

   | Endpoint | Actual `Cache-Control` |
   |---|---|
   | `/v1/price` | `public, max-age=30, s-maxage=60` — the SAME switch case as `/v1/assets`, which this entry contrasts it against |
   | `/v1/oracle/latest` | `public, max-age=60, s-maxage=300` (the `/v1/oracle/` prefix arm) |
   | `/v1/ledgers/latest` | **not a route.** `latest` binds `{seq}`, fails `parseLedgerSeq`, and 400s. The nearest real endpoint is `/v1/ledger/tip`, which the handler sets to `public, max-age=2` |

   The policy is the ORIGINAL April 2026 one (`33b9f567`), four months older than
   this text, so "the deployed binary was older" is not available as a defence.
   The same table appears correctly in `docs/reference/api-design.md`, and this
   section contradicted itself — it named `/v1/assets` as `max-age=30,
   s-maxage=60` while `/v1/price` sits in that identical case.

   **The conclusion survives the correction, on the real numbers.** A 30-60 s
   edge TTL on a price surface is not a substitute for a regional origin: a
   Singapore consumer still pays full origin RTT (~170 ms, typical) on every
   MISS, and at 30 s freshness a low-traffic pair misses far more often than it
   hits. What changes is the *shape* of the argument — the hot path is
   thinly-cacheable rather than uncacheable, which makes the micro-cache
   experiment below more attractive, not less.
2. **The explorer does not justify a box.** Ash: *"people can wait 200 ms for
   explorer results."* Deep explorer pages are lake-backed and proxy to R1 anyway
   (§0b); Cloudflare caching covers the immutable ones far more cheaply.
3. **The real exposure is not latency, it is the single point of failure.** r1 is one
   machine in one DC holding the only copy of the 9.3 TiB lake and the 2.6 TiB raw
   archive. Postgres restores from S3 in hours (drilled monthly); the lake rebuild
   from the public dataset is **unmeasured** because the restore drill emits only
   `last_success_unix` and `failures`, no throughput. A second origin is bought for
   availability, with latency as the bonus — not the other way round.

**Sequence when this resumes (post-v1.0), cheapest-first:**

1. Cloudflare in front (WAF + caching) — covers the explorer and the cacheable API.
2. **Micro-cache test on the thinly-cached hot path.** We serve CLOSED buckets (ADR-0015),
   so a 1-5 s `s-maxage` on `/v1/price` and `/v1/oracle/latest` may be inside the
   contract rather than a staleness risk. If it holds, much of the API becomes
   edge-servable and R3's case weakens sharply. Test before buying hardware.
3. **R2 (US)** — removes the SPOF and covers the likeliest customer base.
4. **R3 (Singapore)** — only on evidence of Asian API usage, split by endpoint.

**Not re-opened by this deferral:** ADR-0050's Model B (independent per-region ingest,
determinism not replication), R1 as lake authority, and the §0b API-first amendment
all stand as the design for when it resumes.


## 1. Ground truth (measured on R1, 2026-08-20/21 — not assumed)

| fact | value | source |
|---|---|---|
| ClickHouse lake | **14.59 TiB** (CH-reported) / **9.30 TiB** physical on ZFS; grows monotonically | `system.parts`, `zfs list` |
| Biggest CH tables | `ledger_entry_changes` 6.55 TiB (45%), `operations` 2.16, `operation_results` 1.50, `tx_hash_index` 1.33, `transactions` 1.14 | `system.parts` |
| CH partitioning | `intDiv(ledger_seq, 1e6)` on all big tables **except `tx_hash_index`** (unpartitioned, hash-sorted → mandatory-hot) | `system.tables` |
| CH replication | **none** — all plain/Replacing MergeTree, no Replicated/Distributed, single `default` Local disk | `system.disks`, `system.storage_policies` |
| Postgres/Timescale (pricing) | **459 GiB** physical / ~1.0 TiB logical (ext4). **Standalone primary**, zero replicas/slots/publications | `pg_database_size`, `pg_stat_replication` |
| Raw galexie-archive (the INPUT) | **2.49 TiB on R1 MinIO — NOT full history** (gap-scan 2026-08-21): genesis chunk `[0, 63999]` + `[49984000 → tip]` only; `[64000, 49983999]` (~50M ledgers, ~2.3 TiB) was capacity-trimmed (`ARCHIVE_FROM=49984000`) and exists locally **only via the `aws-public-blockchain` cold tier** (ADR-0027) | partition gap-scan |
| Pricing SLO today | p95 **68 ms** / p99 **98 ms** (k6) — ~3–5× under budget | slos-and-guarantees.md |
| CH on `/readyz` | **non-critical** — CH down ⇒ `degraded` (200), pricing serves, ~21 lake routes 503 | `clickhouseChecker.Critical()=false` |
| Explorer | separate Next.js app, **migrating to edge SSR** (ADR-0044, accepted) | `web/explorer/` |
| Off-site backup | **none** — pgBackRest 920 GiB on the *same* ZFS pool; no CH data backup | `pgbackrest.conf`, ADR-0043 |
| Pool headroom | 13.2 TiB used / 5.16 TiB free (~72%) | `zfs list` |

## 2. The model decision — Model B (independent per-region ingest)

Two ground-truth facts kill the alternative:

- There is **no cross-region Postgres replication** today (Model A is paper), and
- ClickHouse **cannot** replicate cross-region (no Replicated tables), so no amount
  of Model-A engineering covers the lake.

**Decision: Model B.** Each region **independently ingests the chain**
(galexie → embedded captive-core → indexer → dual-sink Postgres + ClickHouse) and
builds its own stores. Regions stay consistent by **determinism** (ADR-0015
closed-bucket), not replication — the served answer is identical even though each
region built it independently. This is what makes failover clean and what lets the
regions be physically different.

> **Anti-goal:** we do **not** build cross-region streaming replication of
> Postgres, and we do **not** build a stretched Patroni/etcd cluster across
> regions. Those appear in the superseded docs; they are rejected.

## 3. Architecture — three tiers, three shapes

The product splits at the storage seam. Design each tier for what it actually is.

### 3a. Pricing / oracle (Timescale, 459 GiB) — **active/active, all 3 regions**
- Each region runs its own Timescale + Redis, fed by its own ingest → identical
  closed-bucket output. Money endpoints (`/v1/price*`, `/vwap`, `/twap`, `/ohlc`,
  `/oracle/*`, `/observations`) serve **locally** in every region.
- **Invariant (SLO-protecting):** no SLO'd route may cross a region boundary. The
  pricing path is always local Redis+Timescale; it never proxies, never touches
  the lake, never hits S3. This is why the p95≤200/p99≤500 SLO holds in every
  region (see §6). Enforce with a guard test (§9).

### 3b. Lake / explorer (ClickHouse, 9–14 TiB) — **R1 authority + proxy, NOT active/active, NOT S3-tiered**
The lake is too big to replicate fast everywhere and too latency-sensitive to serve
from S3. So:
- **R1 is the lake authority** — full local lake on fast NVMe/ZFS.
- **R2/R3 serve the hot/recent set locally** and **proxy cold/archive queries to
  R1's API** (immutable history caches hard, so most cold reads are cache hits).
- **S3-compatible object storage (Cloudflare R2) is the *fallback*** — queried only
  when R1 is unreachable. It is **not** on the steady-state path, which is what
  makes it affordable (see §11) and keeps latency off the cliff.
- R3 (1.75 TiB disk) **cannot hold the lake at all** — it is pricing-local + a
  thin lake proxy (all lake → R1). R2 (elastic) can optionally hold a hot set.
- **Rejected: per-region S3-tiered ClickHouse.** The audit showed cold explorer
  queries fan out to 1,000–8,000 S3 GETs/page and serialize past the 8 s route
  budget; the storage saving is erased at a few thousand queries/day. See §13-B.

Consequence, stated honestly: lake failover is **fast-normal, degraded-on-R1-outage**
(recent data stays local+fast everywhere; deep history goes to the slow S3 fallback
while R1 is down). You cannot buy cheap *and* fast lake failover for 14 TiB — this
is the right trade.

### 3c. Control plane (accounts, API keys, sessions, webauthn, alerts, webhooks) — **needs real cross-region replication**
This is **non-chain** Postgres state; determinism cannot reproduce it. Today it lives
in the single serving DSN, region-local. Without replication, a key minted in R1
returns **401** in R2 on failover and dashboard users are logged out. **This is its
own workstream** (a small, replicated/shared control-plane Postgres — logical
replication or a managed global store — separate from the pricing/lake data path).
Until it lands, only *anonymous* traffic fails over cleanly.

## 4. Per-region shapes (amends ADR-0016)

| | **R1 — Frankfurt / Hetzner** | **R2 — US / Vultr** | **R3 — Singapore / Vultr** |
|---|---|---|---|
| Role | **primary + lake authority + integrity leader** | active pricing + explorer edge/proxy | active pricing + thin lake proxy |
| Provider | Hetzner dedicated (existing) | **Vultr US bare metal** (was AWS — see note) | Vultr SG bare metal |
| Pricing (Timescale) | full local | full local | full local |
| Lake (ClickHouse) | full local (~9.3 TiB ZFS) | hot-local optional + proxy cold→R1 | **no local lake**; proxy all→R1 |
| Raw archive | full local MinIO + **off-site copy (crown jewel)** | backfill from our off-site archive | backfill from our off-site archive |
| Verify tiers | all (A/B/D/E) — trust anchor | A+D local, trust R1 for B+E | A+D local, trust R1 for B+E |
| ~cost/yr | ~$5K | ~$4.2–7K | ~$4.5K |

**Provider note (amends ADR-0016):** R2 moves **off AWS**. AWS was only justified when
R2 held the full lake (elastic EBS + in-region `aws-public-blockchain` reads). With the
lake removed from R2, a cheap US bare-metal box (Vultr, matching R3) does the job at
~⅓ the cost. Deliberate-difference is preserved (EU primary vs two independent-DC
proxies); provider concentration is acceptable because the *primary* (R1) stays on an
independent provider. OVH-US is the drop-in alternative if more provider diversity is
wanted.

## 5. Data durability & recovery — the raw archive is the crown jewel

- **Source of truth = the 2.49 TiB raw galexie-archive** (genesis→tip). The 14.6 TiB
  ClickHouse lake and the pricing DB are **derived projections** — reproducible from
  the archive by re-ingest. This is exactly why ADR-0043 rejects backing up the
  derived lake.
- **⚠️ CORRECTED (2026-08-21 gap-scan): our local archive is NOT full-history.** R1's
  MinIO holds the genesis chunk + `[49984000, tip]` (~14M ledgers); the middle
  `[64000, 49983999]` (~50M ledgers, ~2.3 TiB) was deliberately capacity-trimmed and is
  reachable today **only through `aws-public-blockchain`** (ADR-0027 cold tier), with SDF
  `history.stellar.org` as the canonical upstream. So until the off-site copy below is
  built **including a one-time pull of that middle range**, deep-history recovery DOES
  depend on AWS's Open Data program — the exact exposure this section exists to close.
- **The gap:** our archive sits on R1's single ZFS pool (SPOF). **Fix: replicate it
  off-site to provider-independent storage (Cloudflare R2 / Backblaze B2)** — ~$500–900/yr
  because it's the 2.49 TiB *input*, not the 14.6 TiB output. This makes us independent
  of both AWS's Open Data program and R1's survival.
- **Two off-site artifacts, two RTOs** — this reconciles ADR-0043 (which rejected a full
  CH backup as the *primary* strategy) with `off-site-backup-plan.md` (which correctly
  argued the re-derive RTO is too slow for a production API):
  1. **Raw galexie-archive, FULL genesis→tip (~5 TiB, Cloudflare R2)** — the ultimate,
     provider-independent source of truth. Built from R1's local archive (genesis chunk +
     [49984000, tip], ~2.49 TiB) **plus a one-time pull of the capacity-trimmed middle
     [64000, 49983999] (~2.3 TiB) from `aws-public-blockchain`**, integrity-checkable
     against SDF checkpoints. Until that pull lands, we are NOT AWS-independent for deep
     history (gap-scan 2026-08-21). Re-ingest from it (~1–2 week walk) is the
     **last-resort** recovery and the region-bootstrap path.
  2. **Derived cold-lake copy (~11.6 TiB, Cloudflare R2)** — *the same copy that backs the
     §3b serving fallback*. It doubles as a **fast-RTO restore source**: restoring CH parts
     is hours, not the weeks a full re-ingest takes. We keep it justified primarily by the
     serving-fallback need; it satisfies the RTO argument as a bonus. (This is more than
     ADR-0043's raw-archive-only posture, and it closes `off-site-backup-plan.md`'s P1
     concern.)
- **Prerequisite verification:** run a completeness/gap scan on the archive
  (genesis→tip, no missing ledger ranges) before trusting it as the sole rebuild source.

## 6. Failover design

- **API (pricing/anon):** LB (Cloudflare) routes to nearest healthy region on a
  health signal; deterministic output ⇒ no consistency issue. Pricing stays local ⇒
  **SLO preserved** (p95 68 ms in every region; the WAN hop is confined to the lake
  path, off the SLO'd routes — §3a invariant).
- **Explorer:** the ADR-0044 **edge-SSR** migration is the enabler — it renders at
  request time on Cloudflare Workers, so it resolves the API region at **runtime** and
  can fail over to a healthy region (the current static-export build-time bake, flagged
  by the audit, is transitional and resolved by ADR-0044 + a runtime region list).
- **Per-tier / lake-aware health (audit gap — must build):** `/readyz` treats CH as
  non-critical and returns 200 even when CH is dead, so an LB on `/readyz` would keep a
  CH-broken region in the pool. Add a **lake-critical health signal** (e.g. `/livez/lake`
  → 503 when CH is unreachable) and **split origins or path-steering** so lake routes can
  fail over while pricing does not.
- **Streaming/SSE:** ledger/observations streams have no replay (silent gap on
  reconnect); price-stream resume tokens are per-region. Accept-and-document, or add
  content-anchored resume (`Last-Event-ID` with a portable cursor) if needed.

## 7. Prerequisite workstreams (nothing multi-region lands before these)

1. **Off-site raw-archive DR** (§5) — the crown-jewel copy on Cloudflare R2. Also the
   region-bootstrap source. Fold into ADR-0043's offsite-repo2 work.
2. **Determinism hardening** (audit §13-A) — make the served answer actually
   byte-identical where it isn't:
   - OHLC `open`/`close`: the CAGG uses `first/last` over non-unique `ts` → add a
     deterministic ordering column (`seq` = packed `ledger,tx_index,op_index,source`)
     and rebuild the continuous aggregate; fix the raw-bar path's `ORDER BY` too.
   - `account_movements`: add `FINAL`/`LIMIT 1 BY` dedup (it's the one lake reader
     missing it).
   - Strip wall-clock/region-local watermark from served responses (throughput chart,
     movements `coverage_note`/cursor).
3. **Lake-aware health + failover routing** (§6).
4. **Control-plane replication** (§3c).
5. **HA foundation is greenfield multi-node** (audit §13-F): the Patroni/HAProxy/Sentinel
   roles hard-gate on cluster inventory groups (`postgres_cluster`=3, `haproxy_lb`=2,
   `redis_cluster`=3, `prometheus_pair`=2) that no inventory defines and that need
   multiple hosts per region. "Single-region HA on R1" is a **procure-and-build**, not a
   role-wiring. The archival-node role also does **not install** ClickHouse/Redis (it
   config-overlays a hand-built box) — that automation must be written.
6. **Multi-region inventory + deploy**: create the group-structured inventory; add r2/r3
   to the `deploy.yml` enum + case + per-region SSH host-keys; fix the dead
   `postgres_replication_role`/seam-ledger config; wire ClickHouse install + the
   R1-proxy/fallback config.

## 8. Explicit HA model choice — cross-region failover, single box per region

**We achieve HA by cross-region failover, one box per region** — a node/region failure
fails traffic to another region. We do **not** build full per-region HA fleets (Patroni
3-node + HAProxy 2-node + Sentinel 3-node *per region*). That alternative is the
$180–288 K/yr topology in the superseded docs; it survives a single in-region node
failure with zero cross-region impact, which is overkill for our scale and cost target.
If a hard requirement for sub-second in-region failover emerges later, that's a separate,
much larger decision.

## 9. The SLO guard (make the invariant enforceable, not aspirational)

Add a test/lint that **fails** if any SLO'd handler (`/v1/price`-class or `/v1/oracle/*`)
gains a remote-CH, cross-region-proxy, or S3 dependency. This turns §3a's "no SLO'd route
crosses a region boundary" from a promise into a checked invariant, so a future change
can't silently route pricing over a WAN.

## 10. Phasing

- **Phase 0 — foundations (no user-visible change):** off-site raw-archive DR (§7.1) +
  archive gap-scan (§5) + determinism-hardening (§7.2) + lake-aware health (§7.3) + this
  doc/ADR reconciliation. All are valuable stand-alone regardless of multi-region.
- **Phase 1 — single-region HA on R1:** the greenfield Patroni/HAProxy/Sentinel build
  (§7.5). Procure the in-region nodes; wire the roles; the ha-plan topology, actually
  deployed.
- **Phase 2 — R2 (Vultr-US):** independent ingest; pricing active/active; explorer edge
  proxy to R1; control-plane replication (§3c); cross-region consistency check; add to LB.
- **Phase 3 — R3 (Vultr-SG):** same, pricing-local + thin lake proxy.
- **Phase 4 — global failover:** LB + lake-aware routing for API and explorer; scheduled
  failover drills.

## 11. Cost model (annual, list-price ballparks; committed pricing ~30–40% lower)

| region / item | shape | ~annual |
|---|---|---|
| R1 (Hetzner, existing) | primary + lake authority | ~$5,000 |
| R2 (Vultr-US bare metal) | pricing + explorer proxy (bigger NVMe if local hot lake) | ~$4,200 (–$7,000) |
| R3 (Vultr-SG bare metal) | pricing + thin lake proxy | ~$4,500 |
| Off-site raw-archive DR | ~2.49 TiB on Cloudflare R2 (free egress) | ~$700 |
| **Fleet total (single box per region)** | | **~$15,000–18,000** |

Anchors verified 2026-08-21: r7i.4xlarge $1.0584/hr (the AWS option we rejected for R2);
Vultr E-2388G bare metal $350/mo. Excluded: user-facing egress (scales with traffic) and
the one-time ~1–2 week backfill per region (box clock, not a line item). The
**$180–288 K/yr** figure in the superseded topology doc is the *full per-region HA fleet*
model we rejected (§8) — do not conflate.

## 12. What this supersedes / what carries forward

**Superseded (do not implement from):**
- ADR-0008 — *the multi-region active/active-out-of-scope decision* (overturned). Its
  single-region HA topology + DR principle carry forward into Phase 1.
- ADR-0016 — Model A ("R1 canonical via replication"), R2-on-AWS, no-ClickHouse sizing.
  Its "same answers, different plumbing" principle carries forward (§4).
- `multi-region-topology.md`, `r2-r3-bringup.md`, `multi-region-cutover.md` — Model A,
  cross-region Patroni, per-region S3 lake, R1-only deploy, nonexistent `site.yml`.

**Carries forward / consistent (reference, not superseded):**
- ADR-0009 (latency budget) — unchanged; §3a/§9 protect it.
- ADR-0015 (closed-bucket determinism) — the load-bearing premise; §7.2 hardens it.
- ADR-0024 (Redis Sentinel), ADR-0027 (LCM cache tiering / aws-public as fallback),
  ADR-0034 (tiered data architecture), ADR-0043 (backup — offsite repo2; §5 refines the
  target to the raw archive), ADR-0044 (edge SSR — §6 enabler), ADR-0048 (serve-by-query-shape),
  ADR-0049 (anonymous/passkey — control-plane replication in §3c must cover its tables).

## 13. Audit provenance

This plan is the product of a **cold adversarial audit (6 auditors, 2026-08-20/21)** that
refuted the first draft's load-bearing claims. Summary of what each auditor established:

- **A — determinism:** REFUTED "byte-identical everywhere." `/price`/`/vwap`/`/twap` hold;
  **OHLC candles diverge** (live: 2,629 trades at one `ts`, 2,579 distinct prices; CAGG
  `first/last` picks arbitrarily), plus movements + throughput. → §7.2.
- **B — tiering/cost:** REFUTED per-region S3-tiering (GET fan-out + 8 s-budget breach). →
  §3b R1-authority + fallback-only S3.
- **C — API/explorer failover:** control-plane is region-local (401 on failover); explorer
  static bake (→ ADR-0044 SSR fixes it); `/readyz` lake-blind. → §3c, §6.
- **D — ingest capacity:** **R3's 1.75 TiB disk physically can't hold the lake**;
  captive-core (~10.5 GiB) runs in every region. → §3b R3 = proxy-only.
- **E — backup/bootstrap:** no off-site backup exists; bootstrap = re-ingest (not
  restore-from-image). → §5, §7.1.
- **F — HA foundation:** roles are un-runnable without multi-host inventory; ADR-0008
  forbids active/active. → §7.5, §8, this supersession.

Full findings ledger retained privately (not committed — public repo). See ADR-0050 for
the ratified decision record.
