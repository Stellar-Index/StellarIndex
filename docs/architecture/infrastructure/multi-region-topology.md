---
title: Multi-Region Topology — 3-region active/active with primary/replica degradation
last_verified: 2026-05-03
status: ratified — per-region storage shapes captured in [ADR-0016](../../adr/0016-per-region-storage-strategy.md); cross-region serving invariant in [ADR-0015](../../adr/0015-last-closed-bucket-rate-serving.md)
---

> **DEPLOYMENT STATE (audit 2026-07-16):** the 3-region topology below is a
> **ratified DESIGN, not the deployed reality.** Only **R1** is provisioned
> (R2/R3 inventory files are absent; `deploy.yml`'s region enum is `[r1]`;
> `docs/architecture/r2-r3-bringup.md` is the bringup plan). Read this doc as
> the target architecture, not a description of what runs today.

# Multi-Region Topology

**Owner:** @ash.
**Scope:** how our three archival-node deployments (per
[archival-node-spec.md](archival-node-spec.md)) are arranged across
three regions, replicated consistently, and degrade gracefully when
any one region is unavailable.

**Supersedes:** [HA plan §2 Physical topology](../ha-plan.md#2-physical-topology)
for the "single region colo primary + cloud DR" default. The HA
plan's per-component HA rules (§3) still apply *within* each region.

## 1. Goals (in priority order)

1. **Three regions, active/active for reads.** Users globally get
   sub-100 ms RTT to the nearest region.
2. **Consistent state across regions.** Every region serves the
   same asset catalogue, same aggregated prices, same historical
   OHLC — no region lies about what another region knows.
3. **Graceful degradation.** If one region is down, the other two
   keep serving. If two are down, the survivor serves read-only
   with `stale_flag=true` on writes-dependent endpoints.
4. **One write endpoint at a time.** Writes always go to a single
   "primary" region to avoid split-brain; the other two apply changes
   via replication.
5. **Region-local latency for hot reads.** Redis + API pods are
   per-region; they never cross a region boundary on the hot path.

Non-goals at launch:

- Multi-writer active/active (we chose not to pay the CRDT /
  conflict-resolution tax at v1).
- Zero-downtime region-loss for writes during the ≤ 30 s failover
  window (we accept 30 s of read-only on the affected endpoints).

---

## 2. Regional choice

Target regions, chosen for:
- continental coverage (Europe / Americas / APAC),
- independent network providers (no shared backbone SPOF),
- colo availability that matches archival-node-spec.md §3.

| Region | Location | Provider | Role | Notes |
| ------ | -------- | -------- | ---- | ----- |
| **R1** | Falkenstein, DE (FSN1) | Hetzner bare metal | Primary at launch | EU coverage; full local archive mirror per [ADR-0016](../../adr/0016-per-region-storage-strategy.md) §"R1". |
| **R2** | us-east-1 | AWS (EC2 r7i.4xlarge) | Secondary (sync replica) | Reads galexie data direct from `s3://aws-public-blockchain` (free egress, sub-15 ms RTT) per ADR-0016 §"R2 — AWS-hybrid". |
| **R3** | Singapore | Vultr Bare Metal + Object Storage | Tertiary (async replica) | APAC coverage; galexie-archive on Vultr Object Storage, postgres + galexie-live on local NVMe per ADR-0016 §"R3 — Vultr-hybrid". |

Cross-region RTT expectations:

| Pair | Expected RTT | Our assumed replication ceiling |
| ---- | ------------ | ------------------------------- |
| R1 ↔ R2 | 70–90 ms | sync replication viable |
| R1 ↔ R3 | 160–200 ms | async only |
| R2 ↔ R3 | 180–210 ms | async only |

This asymmetry drives the topology: **sync replication R1→R2,
async R1→R3**. If R1 dies and R2 promotes, R2→R3 stays async.

---

## 3. The topology — layer by layer

```
             Anycast DNS (Cloudflare) + WAF + CDN
             (per-request GeoIP → closest healthy region)
                              │
       ┌──────────────────────┼──────────────────────┐
       │                      │                      │
  ┌────┴────┐            ┌────┴────┐            ┌────┴────┐
  │   R1    │            │   R2    │            │   R3    │
  │  FSN1   │            │us-east-1│            │Singapore│
  │ Hetzner │            │   AWS   │            │  Vultr  │
  │ PRIMARY │            │SYNC REPL│            │ASYNC RPL│
  └────┬────┘            └────┬────┘            └────┬────┘
       │                      │                      │
  ┌────┴────────────┐    ┌────┴────────────┐    ┌────┴────────────┐
  │ HAProxy pair    │    │ HAProxy pair    │    │ HAProxy pair    │
  │ + keepalived VIP│    │ + keepalived VIP│    │ + keepalived VIP│
  └────┬────────────┘    └────┬────────────┘    └────┬────────────┘
       │                      │                      │
  ┌────┴────────────┐    ┌────┴────────────┐    ┌────┴────────────┐
  │ 3× api pods     │    │ 3× api pods     │    │ 3× api pods     │
  │ 1× aggregator*  │    │ 1× aggregator*  │    │ 1× aggregator*  │
  │   (standby)     │    │   (standby)     │    │   (standby)     │
  │ 3× indexers     │    │ 3× indexers     │    │ 3× indexers     │
  │ Redis cluster   │    │ Redis cluster   │    │ Redis cluster   │
  │ (local only)    │    │ (local only)    │    │ (local only)    │
  │ MinIO EC(6+3)   │    │ MinIO EC(6+3)   │    │ MinIO EC(6+3)   │
  │ Timescale prim**│────┼─ sync replica ──┼───→ async replica    │
  │ Archival node   │    │ Archival node   │    │ Archival node   │
  └─────────────────┘    └─────────────────┘    └─────────────────┘

   *  Aggregator is leader-elected globally via Patroni DCS (etcd).
      Whichever region holds the Timescale primary also hosts the
      active aggregator.
   ** Timescale primary lives in whichever region Patroni has elected;
      on R1 by default. Failover = promote R2's sync replica.
```

---

## 4. Stellar-core layer (special case)

**Stellar-core nodes are peers, not master-slave.** The Stellar
network reaches consensus via SCP; all three of our archival nodes
are equal participants. "Primary region" in this doc refers to
*our application layer*, not Stellar consensus.

Consequences:

- Each region's node independently follows the network and builds
  its own history archive. Occasional divergence on the tip of the
  ledger during partitions is normal and self-heals (SCP guarantees
  convergence once connectivity returns).
- **Hash verification:** a cross-region job (`archive-cross-check`)
  compares per-ledger hashes across the three published archives
  every hour. Any mismatch pages P1. ADR-0004 binds us to this.
- **Galexie output:** three independent Galexie instances write the
  same ledger-meta to three MinIO buckets. The indexer ingests from
  *its own region's* Galexie (lowest-latency read) and reconciles
  against the persisted trade key (`ledger, tx_hash, op_index, ts`) across
  regions — duplicates from cross-region ingest are no-ops.
- **stellar-rpc:** same story. Three independent captive-cores.
  Events are subscribed regionally; the application layer dedups by
  `event_id`.

### 4.1 Quorum set

Each of our three archival nodes runs with a quorum set that
includes:

- All three of our own nodes (weight 2 each).
- SDF Tier-1 full validators (weight 2 each).
- Two other Tier-1 orgs (e.g. LOBSTR, Satoshipay) at weight 1 each.

The exact quorum structure is an ADR (planned: ADR-0012 Quorum set
policy) — SDF's Tier-1 Orgs doc is the reference shape.

### 4.2 Archive cross-check specifics

`scripts/ops/archive-cross-check.sh` — runs hourly from an `ops-01`
host in each region:

1. Pulls the `.well-known/stellar-history.json` from each region's
   published archive URL.
2. Extracts `currentLedger`, `currentBuckets[*].hash` for recent
   checkpoints.
3. For the three most recent checkpoints in common, compares the
   bucket-hash lists byte-for-byte.
4. Mismatch → Prometheus counter `stellarindex_archive_divergence_total{a,b}`
   increments + AlertManager fires P1.

The hourly rhythm matches the checkpoint cadence (every 64 ledgers
≈ 5 min; hourly catches any inconsistency within 12 checkpoints).

---

## 5. Application state — TimescaleDB (the single-writer layer)

### 5.1 Topology

- **Patroni** (3-node cluster across R1, R2, R3) manages failover.
- **etcd DCS** — 5 nodes across the 3 regions (R1×2, R2×2, R3×1)
  to tolerate one region loss without losing quorum.
- **PostgreSQL 15 + TimescaleDB 2.x** on each node.
- **Replication:**
  - R1 (primary) → R2: `synchronous_commit = remote_apply`,
    `synchronous_standby_names = 'FIRST 1 (r2_replica)'`.
  - R1 → R3: async streaming replication.
- **Writes:** always to R1 primary. No writes in R2 or R3 while R1
  is healthy (reads only).

### 5.2 Why sync to R2 only

Writing commits across a ~200 ms WAN link (R1↔R3) would cap our
ingest throughput at ~5 trade writes/sec, which is below the Stellar
trade rate. Sync to R2 gives us durability across two regions
without the throughput collapse. R3 is a warm read replica and a
cold DR target.

Trade-off: if R1 **and** R2 both fail simultaneously (asteroid
strike on eastern North America + EU backbone failure on the same
day), R3 is promoted and we lose ≤ 5 seconds of writes (the async
replication lag). Acceptable given we're a pricing service, not a
bank.

### 5.3 Failover sequence (R1 dies)

```
  t+0       R1 primary dies. Patroni notices within 10 s.
  t+10s     Patroni votes via etcd. R2 wins (sync replica → zero data loss).
  t+15s     R2 promotes: `pg_ctl promote`. PgBouncer pools in all regions flip writes to R2.
  t+20s     Aggregator leader-election (Redis lock): active in R2.
  t+25s     R3 re-parents to R2: `pg_rewind` + streaming follow. Was ~200ms lag; now catching up.
  t+30s     Indexers in R1 halt (they can't write to a dead primary); R2+R3 indexers carry on.
  t+60s     Freshness alert clears — cluster is healthy from two regions.
```

User impact during the window:

- `GET /v1/price`: served from local Redis cache everywhere,
  `stale=true` flag set from t+15 through t+60.
- `POST /v1/account/keys`: 502 responses for ~15 s (write failure);
  retryable idempotently.
- `/v1/price/stream` SSE: disconnect at t+0, clients reconnect to
  local pod, see `stale=true` events for 30 s, then resume.

### 5.4 Failback to R1

Not automatic. When R1 returns:

1. R1 starts as an unprivileged replica following R2.
2. Operator confirms via runbook that R1 state is clean.
3. Manually planned switchback during a low-traffic window.
4. Same 30-s window of writes-stale flag.

"Never fail back without a human review" is intentional — a split-
brain that auto-fails-back causes worse damage than a few extra
days with R2 as primary.

---

## 6. Redis — region-local, not cross-region

Redis is a cache. It is **not** replicated cross-region. Each
region has its own Redis cluster (per HA plan §3.4).

- After a failover, the new primary's Redis is empty on keys the old
  primary had been refreshing. Those keys re-hydrate from Timescale
  within seconds as requests hit them — `/v1/price` experiences a
  brief latency bump (cold cache) but no incorrectness.
- Redis in a dead region is abandoned. The region's api pods stop
  serving traffic; DNS routes users elsewhere. When the region
  returns, Redis starts from cold and re-hydrates.

Why not replicate Redis:

1. It's a cache. If it's wrong, we refetch. Cross-region-replicated
   cache is a consistency hazard (stale cross-region writes can
   undo a correct local read).
2. Redis geo-replication (CRDT / Redis Enterprise) is either an
   expensive license or complex to operate in a small team.

---

## 7. Storage — per-region, three shapes (per ADR-0016)

Originally this section assumed every region ran the same 9-node
MinIO cluster (per HA plan §3.5). [ADR-0016](../../adr/0016-per-region-storage-strategy.md)
revises that: each region picks the storage shape that fits its
provider's natural strengths and its role in the fleet. The
consistency property (ADR-0015) is preserved — what the API serves
is closed-bucket VWAP/TWAP/OHLC, byte-equivalent across regions —
so different storage shapes don't break the cross-region rate
agreement.

### 7.1 R1 (Frankfurt, Hetzner) — full local mirror

- Local MinIO single-node on raidz2 across 4 × 7.68 TB NVMe.
- `galexie-live/`: ingested by R1's own captive-core galexie.
- `galexie-archive/`: full local mirror, ~4.76 TB, sourced
  initially from `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`
  via the per-partition `galexie-archive-fill` recipe.
- `/srv/history-archive/`: full SDF mirror, ~7 TB, on a separate ZFS
  dataset (not in MinIO).
- **R1 is the integrity leader** — runs all four verify-archive
  tiers (A + B + D + E) on a schedule. R2 and R3 trust R1's
  primary verification.

### 7.2 R2 (US-East-1, AWS) — AWS-hybrid, no galexie mirror

- No local MinIO for galexie-archive.
- `galexie-archive`: read direct from
  `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`. Sub-15 ms
  intra-region S3 latency, free egress (AWS Open Data Sponsorship).
- `galexie-live/`: small EBS-backed bucket if R2 runs its own
  captive-core for redundancy; otherwise read from AWS public's
  near-tip range (subject to that bucket's catch-up lag).
- postgres + OS: EBS gp3, ~1-2 TB.
- `/srv/history-archive`: NOT mirrored locally. R2 trusts R1's
  Tier B + E. Runs its own Tier A (chain integrity, no external
  data) + Tier D (multi-peer cross-validation) on a weekly cron
  for defence-in-depth.

### 7.3 R3 (Singapore, Vultr) — bare-metal + object-storage hybrid

- Bare metal Intel Xeon E-2388G + 128 GB DDR4 ECC + 2 × 1.92 TB
  NVMe RAID-1 (Vultr's standard SG SKU at ~$350/mo).
- `galexie-archive`: Vultr Object Storage (S3-compatible) at
  ~$25/mo for 5 TB. Region-local to the Singapore facility
  (~5-10 ms latency).
- postgres + galexie-live (rolling ~30 days) + captive-core state +
  OS: local NVMe.
- `/srv/history-archive`: NOT mirrored locally — same trust model
  as R2. Tier A + D run on weekly cron.
- **RAID-1, not raidz2** — single-drive failure tolerance. Acceptable
  for an async DR replica because multi-drive failures are
  recoverable via the bring-up recipe (~half day).

### 7.4 Trust model + drift detection (defence-in-depth)

R2/R3 don't run Tier B + E (which need a local SDF history mirror),
so they rely on:

1. **Local Tier A (weekly cron):** walks each region's own ingested
   ledgers; confirms `header.PreviousLedgerHash == prev.LedgerHash`.
   Catches local bit-rot AND any internally-inconsistent stream from
   upstream.
2. **Local Tier D (weekly cron):** HTTP fetches checkpoint hashes
   from ~6 tier-1 validator archives (LOBSTR, SatoshiPay, SDF,
   PublicNode, Blockdaemon, Franklin Templeton); compares against
   the local chain. Catches **forks** (internally-consistent chains
   that don't match the network's signed reality).
3. **Cross-region CAGG consistency check:** monitoring job samples
   `(pair, window, from_ts)` triples across R1/R2/R3 and asserts
   the closed-bucket VWAP rows match. Tests the actual API outcome
   rather than intermediate bytes — strongest detection for
   indexer-output divergence.

Optional belt-and-braces (defer until needed): per-region
`(ledger_seq → sha256(LCM bytes))` hash database to catch
upstream byte rewrites. ~2 GB per region. Implement only if a
real drift event surfaces.

### 7.5 Backups + cross-region replication (unchanged from original plan)

- `backups/`: per-region MinIO bucket. Cross-region replicated for
  Postgres pgBackRest repos (sync lag 5 min).
- Postgres WAL replication handles trade-row durability across
  regions; pgBackRest is the secondary (point-in-time-recovery)
  layer.
- `history-archive` content (SCP-published): per-region in R1,
  not present in R2/R3 (per the trust model above).

### 7.6 Failure-mode handoffs

| Scenario | Effect | Mitigation |
|---|---|---|
| R2 loses access to `aws-public-blockchain` | R2 ingest stalls | Switch to mirroring from R1's MinIO over WAN (slower but works); document in r2 runbook |
| R3 Vultr Object Storage outage | R3 indexer can't read archive ledgers | Indexer pauses backfill; live-tail continues from local NVMe; resumes when storage is back |
| R3 RAID-1 single-drive failure | local NVMe degraded but operational | Replace drive via Vultr support; resilver |
| R1 disk loss | R1 down for ~half day | Promote R2 to writer; rebuild R1 from bring-up recipe |
| Tier D detects divergence on R2 or R3 | Indexer alerted; potential fork | Investigate; failover writer to a region whose local chain matches the validator quorum |

---

## 8. DNS + routing

### 8.1 Public API

- `api.stellarindex.io` is a Cloudflare-managed anycast record.
- Cloudflare Load Balancer with:
  - GeoIP steering → closest healthy region.
  - Health checks per-region at `/readyz` every 15 s.
  - Automatic removal of a region on 3 consecutive failed checks
    (`steering_policy = geo`, `ttl = 60`).
- TTL 60 s: balances fast failover against cache-churn. No DNS TTL
  below 30 s — we don't trust upstream resolvers.

### 8.2 Admin / internal

- `admin.stellarindex.io`: A-record to the ops jump host in the
  primary region. Failover manual.
- Region-specific hostnames (`r1.api.internal`, `r2.api.internal`,
  `r3.api.internal`) for cross-region pod-to-pod comms and
  diagnostics.

### 8.3 "Region down" user experience

- Region loses internet: Cloudflare health check fails within 45 s.
- GeoIP-next-closest region absorbs load. Users in APAC (normally
  R3) now hit R1 (~160 ms RTT) — higher latency, same correctness.
- No client-side work needed. SSE reconnects happen at the TCP layer.

---

## 9. Master-slave glossary (because "master" is overloaded)

We deliberately use three different words to avoid confusion:

| Term | Scope | Semantics |
| ---- | ----- | --------- |
| **Primary region** | Application state | The single region that is allowed to write to Timescale. Elected by Patroni. Fails over automatically. |
| **Leader** | Aggregator process | The single aggregator process (across all regions) that is actively computing VWAP/TWAP + refreshing Redis + Timescale precompute tables. Elected by Redis advisory lock; follows the primary region. |
| **Peer** | Stellar-core nodes | All three archival nodes are peers in the SCP sense. No master. |

When we say "the primary is down" we mean the primary region's
Timescale is down, not the stellar-core node or the region as a
whole.

---

## 10. Cost impact

This topology is ~3× the single-region cost envelope. HA plan §12's
$5–8 k/month baseline now becomes **$15–24 k/month** at steady
state, minus whatever cloud DR we drop in favour of a full third
physical region. Hardware CapEx is 3× the per-node BOM in
[archival-node-spec.md](archival-node-spec.md) §4 = ~$55 k one-time.

Trade we're making: **3× cost for ~100× availability improvement**
over single-region + cloud DR. A single region colo outage causes
hours of downtime; three peer regions ride it out with seconds of
write-stale.

Whether the SDF grant + customer revenue covers the recurring bill
is an operational decision outside this doc's scope. The
architecture supports both the lean (single-region-primary + cloud-
DR) shape and the 3-region shape with the same code — the
difference is purely in how Patroni is configured and how many
physical nodes we run.

---

## 11. Failure matrix — 3 regions

Extends HA plan §5 with multi-region scenarios.

| Scenario | Blast radius | User experience | RTO |
| -------- | ------------ | --------------- | --- |
| 1 region loses internet | Traffic drains from that region | GeoIP routes to next-closest; ~80 ms extra RTT for displaced users | 45 s |
| 1 region's Timescale primary dies | Writes in that region fail; primary fails over | If affected region was primary → 30 s write-stale; else transparent | 30 s |
| 1 region entire colo outage | Region unavailable; others carry full load | GeoIP + capacity headroom (§12) absorbs 1.5× normal load per surviving region | 45 s |
| 2 regions down | Survivor reads from local Timescale replica (now stale); writes either halt or go to survivor | Clients see `stale=true` + write-endpoint 503s for some endpoints | minutes to hours |
| Cross-region sync replication breaks (R1↔R2 link) | Writes in R1 hold momentarily waiting for sync | `synchronous_commit` falls back to local durability with alert; writes resume | seconds |
| stellar-core hash divergence between regions | `archive-cross-check` alert fires P1 | No user-visible impact immediately; investigation required | varies |
| Split-brain Patroni | Patroni's etcd quorum prevents two primaries | Impossible by design — the 5-node etcd spanning 3 regions requires 3 votes; partition leaves at most one side with a majority | n/a |
| All 3 regions down | Full outage | Return 503 with a status page pointer | depends on root cause |

---

## 12. Capacity per region under 1-region-down load

HA plan §4 sized a single region for 500 rps sustained / 2 000 rps
burst. With 3-region active/active, each region normally carries
~1/3 of traffic (~165 rps steady). When a region dies, the survivors
pick up an extra 1/2 of its share each — so each survivor goes from
165 rps → ~250 rps. Still inside the 500 rps sustained envelope we
sized for single-region.

In other words: **each region is sized for 2x its steady-state
load**, which gives us 1-region-failure headroom without over-
provisioning.

---

## 13. Bring-up order

**One region at launch, three ratified within 10 weeks.** Detailed
per-phase rollout (validator vs archival, key ceremony, quorum set
per phase) lives in
[validator-rollout.md](validator-rollout.md); this section is the
topology-layer summary.

1. **R1 (Hetzner FSN1) first.** Archival node + full
   application stack; runs **solo** for shake-out. Patroni is a
   single-node "cluster"; etcd is a single-node DCS. The code path
   is the multi-region path — there just happens to be only one
   member. This is deliberate: no "single-region-mode" flag, no
   special case to remove later.
2. **R2 (AWS us-east-1) joins.** Sync replica joins; Patroni grows
   from 1 → 2 nodes; etcd grows from 1 → 3 nodes. Application-layer
   replication kicks in. Tracked as **L4.14** in
   [`launch-readiness-backlog.md`](../launch-readiness-backlog.md).
   Validator 2 promotes at the same time (per validator-rollout
   Phase C).
3. **R3 (Vultr Singapore) joins.** Async replica joins; etcd grows
   to 5 nodes. Validator 3 promotes (Phase D). Tracked as
   **L4.15**. We are now a
   T1-eligible org.
4. **Cross-region drills:** once all three are up, we run a
   scheduled primary-failover drill every Wednesday at 03:00 UTC
   for the first month, then monthly.

Running with a single region for several weeks is safe — the code
is identical, and we get real observability data to calibrate
Patroni thresholds before scaling out. Specifically, what we learn
in R1-solo that de-risks R2/R3:

- Real memory/CPU/NVMe numbers vs
  [archival-node-spec.md §3](archival-node-spec.md#3-hardware-spec--per-node)
  extrapolations.
- Real catchup duration; adjusts the R2/R3 seed-from-R1 plan.
- Stellar-core + Galexie + stellar-rpc co-resident behaviour (the
  unmeasured adversarial-audit §6d risk).
- Archive cross-check against SDF + 2 other T1 orgs, proving our
  archive is correct before we replicate it.

---

## 14. What multi-region does NOT buy us

Honestly called out:

- **Doesn't protect against application-layer bugs.** A bug in
  `internal/aggregate` that miscomputes VWAP gets replicated to all
  three regions. Same for a poisoned upstream (Reflector serves bad
  data).
- **Doesn't protect against coordinated outages.** If Cloudflare is
  down globally, all three regions are unreachable. We do not
  multi-CDN at launch (add DNS + CDN redundancy post-launch if a
  Cloudflare outage actually bites).
- **Doesn't give us write-anywhere semantics.** `/v1/account/keys`
  and any other writing endpoint is momentarily read-only during
  primary failover. This is intentional.
- **Doesn't shorten catchup times.** Each region does its own
  initial stellar-core catchup unless seeded from a sibling.

---

## 15. Open questions — closed

The Week-2 plan called for these to land before the design review.
They have:

1. **Validators in all 3 regions from day 1?** No.
   [ADR-0004](../../adr/0004-tier1-validator-aspiration.md) ratifies
   the *post-launch* aspiration; v1 ships archival-only across all
   regions. See [validator-rollout.md](validator-rollout.md) for the
   phased path to Tier-1.
2. **Patroni vs Stolon.** Patroni — landed as
   `configs/ansible/roles/patroni/`.
3. **etcd 5-node placement.** Per ADR-0008 §3 the etcd cluster sits
   inside the patroni role; topology is R1-major (the region hosting
   the Postgres primary) with R2 + R3 contributing one member each.
4. **Cloudflare Load Balancer vs self-hosted GSLB.** Cloudflare for
   the public edge (TLS, WAF, geo-routing); HAProxy + keepalived for
   per-region L4 (`configs/ansible/roles/haproxy/`).
5. **Regional failover alerting** — no page during the 30 s Patroni
   window per design (the system heals). A P2 ticket fires on
   sustained replica-promotion churn (`replica-lag.md`).

---

## 16. References

- [HA plan](../ha-plan.md) — per-component rules apply within each
  region.
- [Archival-node spec](archival-node-spec.md) — per-node hardware.
- [Validator rollout](validator-rollout.md) — the 1 → 3 phased
  bring-up.
- [ADR-0004 Tier-1 validator aspiration](../../adr/0004-tier1-validator-aspiration.md)
- [ADR-0002 MinIO / S3-compat storage](../../adr/0002-minio-s3-compat-storage.md)
