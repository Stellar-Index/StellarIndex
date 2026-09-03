---
title: RFP + proposal compliance audit
last_verified: 2026-09-03
status: point-in-time audit (2026-07-03) — re-audited 2026-09-03 (#345); six rows regraded, thirteen ungraded claims added. Read §0 before any row below.
---

# RFP + proposal compliance audit (2026-07-03)

Audited against the three commitments, **letter and intent**, with every
row verified against the LIVE system (api.stellarindex.io) on
2026-07-03 — not against memory or docs:

1. `docs/archive/stellar-rfp.md` — the Prices API RFP
2. `docs/archive/freighter-rfp.md` — Freighter's asset-detail data RFP
3. `docs/archive/ctx-proposal.md` — our proposal (what we PROMISED)

> ## 0. Re-audit — 2026-09-03 (#345): the 2026-07-03 headline verdict is WITHDRAWN
>
> A cold re-audit against the live system and the R1 host on
> 2026-09-02/03 falsified the headline below and six of the rows that
> supported it. **This file is a dated record of what was believed on
> 2026-07-03**; the grades in §1–§3 have been corrected in place, each
> marked `re-audited 2026-09-03`, and the original grade is preserved
> in the same cell so the drift is visible rather than erased. The
> thirteen proposal claims this table never graded at all are added as
> §3b — nine of them are false.
>
> The corrected headline: **most requirements are met and several are
> exceeded; availability, multi-zone, read replicas and all-time
> coverage are not, and two of those were graded ✅ on the strength of
> evidence that does not exist.** In particular the row that claimed
> "99.99% uptime record … upheld by status page history" was never
> supportable: the status page publishes incidents, not an uptime
> figure, and all five `HEALTHCHECKS_URL_*` heartbeats on R1 are empty.
>
> **The 2026-07-03 verdict, verbatim, for the record:** *"every
> requirement of all three documents is met, most exceeded."*

**The 2026-07-03 record.** The five wallet-visible gaps the morning
audit found (board #40-44) were all closed the same day — merged on
main, live at the next release; the deep-history backfill ran against
production directly (see §0 for what that backfill did *not* cover).
§4 records the security incident this audit's own fact-checking
surfaced and the same-hour remediation.

## 1. Stellar RFP (Prices API) matrix

| Requirement | Status | Evidence (live, 2026-07-03) |
|---|---|---|
| All native Stellar assets + SEP-41 tokens | ✅ | Any `CODE-G...`, `C...`, `native` resolves on `/v1/assets/{id}` + priced when traded; SEP-41 via the projector pipeline (**17** on-chain sources — re-audited 2026-09-03; said 16, drift upward) |
| Oracles: Chainlink, Redstone, Band, Reflector, others | ✅ **exceeds** | `/v1/sources`: reflector-dex/cex/fx, redstone, band, chainlink + **28** total (re-audited 2026-09-03; said 27). Band via ContractCall hook (emits no events); Reflector's 3 contracts integrated separately. **Caveat added 2026-09-03:** `/v1/sources` is a *static registry listing* with no enabled/live discriminator, so the 28 include `coinmarketcap` (which §3 of this same file calls "deferred, never built"), `cryptocompare`, `chainlink` and `exchangeratesapi` — all wired-but-disabled. 11 carry `include_in_vwap:true`. Citing the total as evidence of "exceeds" counts a never-built source as delivered |
| Weighted average across Soroswap/Aquarius/SDEX/Blend etc. | ✅ **exceeds** | VWAP across sdex, soroswap(+router), aquarius, phoenix, comet, defindex + 5 CEX; blend as signal source (per proposal: lending ≠ trade feed) |
| Adjustable USD volume threshold | ✅ | `min_usd_volume = 10000` in config (operator-adjustable); class-based inclusion in `external.Registry` |
| Real-time + historical endpoints (24h/7d/30d/1yr) | ✅ | `/v1/price`, `/v1/price/tip`, `/v1/ohlc`, `/v1/chart` with from/to |
| Base AND quote volume | ✅ | OHLC rows carry `v_base` + `v_quote` (verified live) |
| OHLC for candlesticks | ✅ | `/v1/ohlc`; explorer renders candles from it |
| Timeframes 1h→All-Time; granularity 1min→1month; 1h+ indefinite | ✅ **exceeds** | Full ladder 1m→1mo (board #43 shipped 2026-07-03); retention indefinite at every granularity |
| All-Time = since asset inception | ❌ **regraded 2026-09-03** (was ✅) | The backfill reached 2018-07, but it left a **61-month hole**. Measured 2026-09-03: `/v1/ohlc?base=native&quote=fiat:USD&interval=1mo&from=2014-01-01` returns **37 monthly bars — 2018-07 … 2021-01, then nothing until 2026-03**. Retention is indefinite (see the row above), so this is a *coverage* gap, not a drop, and no product surface marks it: a client charting "all time" sees a continuous line across a 61-month void. Second, independent failure: **`/v1/chart` is XLM-alias-blind.** `?asset=native&quote=fiat:USD&timeframe=all&granularity=1mo` returns **6 points from 2026-03** with `triangulated:true`; `?asset=crypto:XLM` returns **35 from 2018-07**. `chart.go` reads the literal pair `native/fiat:USD` (zero rows — the depth is stored under `crypto:XLM`) and falls through to the stablecoin-proxy fallback, serving the SDEX `native/USDC-G…` market instead. `/v1/ohlc` aliases correctly, so two of our own endpoints disagree about XLM's history. This is a **live product defect**, not a wording error — the `assetAliases` loop seven other read paths already run is the fix. Per-market inception via `/v1/markets?include=inception` is real |
| HA, low-latency, high query volume | ❌ **regraded 2026-09-03** (was ✅) | **Low latency: TRUE.** Measured 2026-09-03: p50 120 ms / p95 145 ms / p99 166 ms client-side from a residential connection; 19.8 ms p95 server-side. **HA: FALSE** — one Hetzner FSN1 box, `pg_stat_replication` / `pg_replication_slots` / `pg_publication` all 0, no second region (see §3 multi-zone). **"CDN in front": FALSE and was never true** — `dig api.stellarindex.io` → `136.243.90.96`, the origin itself, answering `via: 1.1 Caddy` with no Cloudflare headers; only `stellarindex.io` / `docs.` / `status.` are behind Cloudflare, so the `s-maxage=60` we set on `/v1/price` has no shared cache to act on. **"99.99% uptime record … upheld by status page history": FALSE and unsupportable** — the status page publishes incidents and postmortems, never an uptime figure, and there is no external availability record of any kind: all five `HEALTHCHECKS_URL_*` on R1 are empty. Also note 99.99% contradicts our own published SLA of ≥ 99.9% (stellarindex.io/sla) by a factor of ten |
| Explain unavailable/diverging prices | ✅ **exceeds** | `flags{stale, reduced_redundancy, triangulated, divergence_warning, divergence_checked}` on every price + confidence scoring + divergence workers vs CoinGecko/Chainlink + public methodology docs |
| **Completely open source (Tranche I & II)** | ✅ | github.com/Stellar-Index/StellarIndex is PUBLIC (verified `gh repo view` 2026-07-03 — the audit's first draft wrongly said private from stale memory; see §4 for the incident that correction triggered) |
| Asset metadata (code/price/type/issuer/contract/home_domain) | ✅ | All fields incl. `contract_id` on classic + native (derived SAC address, board #40 shipped 2026-07-03) |
| Production API ~10 weeks | ✅ | Live since deliverable claim 2026-06-13 (AC1-7 evidenced) |
| API reference docs + self-service onboarding | ✅ | docs.stellarindex.io (generated from OpenAPI), dashboard signup → API key (`sip_` prefix), Postman collection, curl examples |

## 2. Freighter RFP matrix

| Requirement | Status | Evidence |
|---|---|---|
| V1 asset metadata fields | ✅ | All present incl. `contract_id`; `home_domain` SEP-1 resolved, org-verified two-way |
| V1 chart timeframes/granularities | ✅ | All five timeframe/granularity rows servable (1min→1d) |
| V2: market cap | ✅ | `market_cap_usd` live (supply pipeline, CS-010-verified circulating) |
| V2: FDV | ✅ | `fdv_usd` served when a max supply exists; correctly null-omitted for uncapped assets (the audit's first pass mistook null-omission for absence) |
| V2: 24h volume | ✅ | `volume_24h_usd` live |
| V2: circulating/total supply | ✅ **exceeds** | Live + continuously reconciled vs SDF + Stellar Expert (`verify-served-values`, all green) |
| V2: max supply (nullable) | ✅ | `max_supply` served, null-omitted when uncapped — exactly the RFP's nullable semantics |
| p95 ≤200ms / p99 ≤500ms | ✅ (provenance corrected 2026-09-03) | The numbers hold: measured 2026-09-03, p95 145 ms client-side over 200 samples, 19.8 ms server-side. **But the provenance in the original cell was wrong twice.** The deployed sla-probe measures `http://localhost:3000/v1` (`configs/healthchecks/sla-probe.sh:21`, unset on R1) — the API's own listener, bypassing Caddy, TLS, DNS and the network — so it is an *application-time* figure, not an edge one. And "CDN-cached lower" describes a CDN that does not exist in front of the API |
| Responsiveness ≥99.9% | ❌ **regraded 2026-09-03** (was ✅) | **No such history exists.** Every heartbeat URL in `/etc/default/stellarindex-healthchecks` on R1 is blank — indexer, aggregator, api, smoke, sla-probe (5 of 5). The only live Healthchecks check is the Alertmanager dead-man's-switch, an outage detector that produces no percentage. The status page publishes incidents, not availability. Nothing on R1 or in the repo can produce a 99.9% figure for any period, so this is an **objective we have not instrumented**, not a met requirement. Closing it needs an off-host probe |
| Freshness ≤30s | ✅ on `/v1/price/tip`; ❌ on `/v1/price` — **scoped 2026-09-03** (was an unqualified ✅) | Tip meets it: measured `observed_at` age ≈ 0 s at read time, R1 probe median 18.1 s. `/v1/price` does **not** and structurally cannot: it serves the last *closed* one-minute bucket per ADR-0015, so its `observed_at` is 30–150 s old by construction. Measured 2026-09-03 over repeated samples: **85–94 s, with `flags.stale: false` every time** (the flag tracks a different, 15-minute rule); R1's own `stellarindex_sla_probe_freshness_sec{endpoint="price"}` reads 95.2. The probe binary already encodes the split (`defaultClosedBucketFreshTarget = 150s`) and so does the alert rule. **The claim is only true of the endpoint the SLA page did not name** — which a wallet integrator testing `/v1/price` first will discover in one curl. The public SLA page was corrected in the same pass (#345) |
| SEV-1 detect ≤15m, respond ≤30m | ✅ | Alertmanager severity routing + SEV playbook + deadman switch; SEV-1 drill PASS 90s (2026-06-13) |
| Lookup by contract address | ✅ | SAC addresses resolve to the wrapped classic asset WITH its price (board #40; core-minted-executable trust anchor + derivation cross-check, adversarially tested) |
| Retention ≥1yr (ideally inception) | ✅ **exceeds** | Indefinite at every granularity |
| REST, 1000 req/min | ✅ **exceeds** on the anonymous tier; second clause ❌ **regraded 2026-09-03** | 6000/min anonymous is verified (`x-ratelimit-limit: 6000`, `/etc/stellarindex.toml:334`). "Higher per-key" is **backwards**: `key_rate_limit_per_min = 6000` merely *equals* the anon tier, and the per-key path passes the key row's own `RateLimitPerMin` as a **replacement** ceiling (`middleware/ratelimit.go` → `ratelimit/bucket.go`: `if limit > 0 { effectiveMax = limit }`), while signup mints keys at **1000** (`internal/api/v1/signup.go`). Net: **holding a free API key drops you from 6000/min to 1000/min — a 6× penalty for authenticating.** That is a deliberate design (keys are for attribution and usage reporting, not headroom — see `docs/getting-started.md`), but it must never be published as "higher per-key". The public pricing, signup and contact pages were corrected in the same pass (#345) |
| Bulk: current price + **24h % change** | ✅ | batch rows carry `change_24h_pct` for USD quotes (board #41, shipped 2026-07-03) |
| VWAP > TWAP > last-trade w/ timestamp | ✅ | `price_type` on every response; exact fallback chain per aggregation-plan |
| USD quote / DEX scope / since-inception=first trade | ✅/⚠️ | USD + arbitrary quotes (exceeds); DEX + CEX + FX (exceeds, disclosed); inception see above |

## 3. Proposal commitments (what WE promised beyond the RFPs)

| Promise | Status |
|---|---|
| 16+ sources, CEX + FX + reference + oracles | ✅ **28** registry entries (re-audited 2026-09-03; said 27), of which 4 are wired-but-disabled and 11 contribute to VWAP — see the §1 caveat |
| Arbitrary pairs + triangulation (XLM/EUR, AQUA/BRL) | ✅ live (`triangulated` flag) |
| SSE streaming | ✅ `/v1/price/stream` + tip stream (verified live) |
| Batch queries | ✅ (minus the 24h-change field) |
| Methodology labeling, staleness, degradation flags | ✅ every response |
| Confidence indicators | ✅ confidence scoring (wave-102) |
| CoinGecko + **CMC** cross-checks | ⚠️ CG integrated (free-tier dead since 2026-06-19; Pro purchase pending — operator). **CMC deferred, never built** |
| Public status page + status endpoint + callback alerts | ✅ / ⚠️ status.stellarindex.io + `/v1/healthz`; customer webhooks exist; Discord/Slack incident callbacks pend operator accounts |
| Per-IP + per-key limits, elevated tiers | ✅ |
| Versioned API, SemVer | ✅ (ADR-0042 wire-shape policy) |
| Open source + self-host templates (compose, IaC) | ⚠️ Ansible IaC now PROVEN (2026-07-03 it converged live r1); docker-compose is dev-stack only; a full self-host guide is now unblocked (repo public) — worth writing |
| Multi-zone deployment | ❌ **regraded 2026-09-03** (was ⚠️). This is a flat no, not a partial. The Patroni / HAProxy / Redis-Sentinel / Prometheus-pair roles each hard-gate on inventory groups (`postgres_cluster`, `haproxy_lb`, `redis_cluster`, `prometheus_pair`) that appear in **zero** inventories — every inventory (`r1.yml`, `r2.example.yml`, `r3.example.yml`, `testnet.yml`, `futurenet.yml`) defines only `archival_nodes` — and **no playbook invokes any of them**; `redis-sentinel/tasks/01-preflight.yml` asserts exactly 3 hosts, which nothing can satisfy. "Mechanical" overstated that: `docs/architecture/r2-r3-bringup.md` is itself banner-superseded as describing "rejected architectures … and dead scaffolding". R1 is one box holding the only copy of the ~9 TiB ClickHouse lake and the ~2.5 TiB MinIO galexie-archive. [ADR-0050](../adr/0050-multi-region-ha-architecture.md) (2026-08-21) ratifies multi-region and **defers implementation until after v1.0**. The DR evidence (drill PASS) is real and is a separate, met commitment — it is not multi-zone |
| Read replicas | ❌ not deployed (not needed at current load; noted) |
| Wash-trading mitigations: volume floors, outliers, medianization | ✅ volume floor + outlier filtering + oracle exclusion-by-class |
| **Min trade-count + spread constraints per window** | ⚠️ (corrected 2026-09-03 — half of this was wrong in our favour). A trade-count floor **does** exist: `GlobalPriceOptions.VWAPMinTradeCount = 5` (`internal/aggregate/global.go`), though it gates the global-price *tier chooser* rather than per-window VWAP eligibility, so the original "not implemented" reading understated us. **Spread constraints are genuinely absent** — the only "spread" in `internal/aggregate/` is the MAD served-guard band and the router's post-omission divergence spread, neither of which is a per-market bid/ask gate. Volume floor (`min_usd_volume = 10000`) and MAD outlier filtering are real (documented divergence — methodology docs describe what IS done) |
| Circular-path detection in triangulation | ✅ anchor-set design prevents cycles (USD-anchored paths only) |
| Configurable current-price window **via query** | ✅ `?window=60\|300\|3600\|86400` on /v1/price (board #43); sub-minute rolling = /v1/price/tip |
| GraphQL (optional) | N/A — "may be provided"; REST + SSE cover stated use cases |
| Backups versioned + restore-TESTED | ✅ **exceeds** (2026-07-03 drill: restored, recovered, bit-identical window) |
| RBAC, secrets isolation, audit logging | ⚠️ **regraded 2026-09-03** (was ✅). RBAC ✅ (`internal/platform/user.go` `Role` + `IsStaff`, tier ladder in `account.go`). Secrets isolation ✅ (`EnvironmentFile=`, ansible-vault, non-root service users). **Audit logging of *configuration* changes — which is what the proposal promised — does not exist.** `audit_log` (migration 0027) is customer-platform scoped: `internal/platform/audit.go` enumerates `key.mint`, `plan.upgrade`, `session.revoke` with actors `user/staff/system/webhook`. No aggregation-configuration change is audit-logged; thresholds are TOML edits on R1, and codified-is-not-applied drift is a known recorded failure class |

## 3b. Claims the 2026-07-03 table never graded (added 2026-09-03, #345)

Thirteen proposal claims were absent from §3 entirely. An omitted claim
reads as an un-flagged claim, so they are graded here. **Nine are
false.** Where the reality is *better* than the claim it is said so
explicitly — three of these are cases where we under-described a
stronger architecture.

| # | Proposal claim | Grade | Verified reality (2026-09-02/03) |
|---|---|---|---|
| 1 | "All services are containerized and deployed behind load-balanced entry points." | ❌ | Neither half. R1 has **no container runtime** (`docker` and `podman` both `command not found`); services are `deploy/systemd/*.service` units running bare binaries. The entry point is a single Caddy reverse proxy on the same box with **one** upstream — a reverse proxy, not a load balancer. `configs/ansible/roles/haproxy/` targets an inventory group (`haproxy_lb`) that exists in no inventory and is referenced by no playbook. Dockerfiles do exist and CI builds them; they are for self-hosters, and the ghcr.io push was dropped for want of a consumer |
| 2 | "Core services are deployed as stateless containers: Ingestion workers / Aggregation engine / API servers / Streaming gateway" | ❌ | Same as row 1. Additionally there is no separate "streaming gateway" — SSE is served by the API binary |
| 3 | "Services scale horizontally. API nodes and ingestion workers can be scaled independently based on load." | ⚠️ split three ways | **API:** fleet-safe by construction with Redis (webhook drain uses `FOR UPDATE SKIP LOCKED`, usage rollup a `GREATEST(...)` upsert, fx_quotes `ON CONFLICT DO UPDATE`) — supported by design, **one instance deployed**. **Indexer:** `enabled_sources` shards by *source*, not by load; overlapping source sets would double-write, with no lock. **Aggregator:** [ADR-0008](../adr/0008-ha-topology.md) §4 promises "one active + one standby, leader-elected via a Redis lease" — **no leader election exists anywhere in the tree** (`grep -rniE "leader.?elect"` → 0 hits; the only advisory locks are per-row). A second aggregator would publish concurrently with no arbitration. That is an **ADR-vs-code gap**, not just a claim gap, and it is the reason "the aggregator scales horizontally" must not be said today |
| 4 | "Multi-zone deployment is supported to eliminate single points of failure." / "Multi-zone deployment configuration" (Phase 6 deliverable) | ❌ | See the multi-zone row in §3 |
| 5 | "The target availability is 99.99 percent or greater." | ❌ | The **published** SLA is **≥ 99.9 %** (stellarindex.io/sla, and `docs/operations/sla-probe.md`) — an error budget of ~43 min/month against 99.99 %'s ~4 min 19 s. The proposal promises a target 10× tighter than the one we publish, and **neither is externally measured** (all five R1 heartbeat URLs are blank). 99.99 % survives in [ADR-0008](../adr/0008-ha-topology.md) and the coverage matrix as the *internal design target the HA topology was sized against*; `docs/architecture/ha-plan.md` was corrected in this pass to say so rather than "we commit to 99.99 %" |
| 6 | "All Soroban event indexing uses self-hosted RPC nodes rather than third-party providers, with the same multi-instance redundancy applied to the Classic DEX path." | ❌ (**reality is stronger**) | We use **no RPC at all** for ingest — stellar-rpc was removed from R1 on 2026-04-23. Ingest is Galexie MinIO → `internal/ledgerstream` → `internal/dispatcher` (CLAUDE.md invariant 6). Not depending on any RPC node is a *better* property than self-hosting one; the sentence is still false, and there is no multi-instance redundancy on either path |
| 7 | "Prices are derived from … pool reserve ratios via `get_reserves() -> (i128, i128)` for continuous implied pricing" (and "reserve-based implied prices are continuously cross-validated") | ❌ (**reality is stronger**) | `grep -rn "get_reserves" internal/ cmd/` → **0 hits**. Reserves are reconstructed from the protocol's own following `SyncEvent`, never read from contract state — which is the correct design and is what the proposal describes correctly two paragraphs earlier. There is no reserve-derived price series, so nothing is cross-validated against one |
| 8 | "OHLC bars are generated from aggregated window prices rather than raw trades." | ❌ (**reality is the industry-standard one**) | `migrations/0002_create_price_aggregates.up.sql` computes O/H/L/C as `first/last/max/min(quote_amount / base_amount)` over raw `trades` in a continuous aggregate, at every grain 1m→1mo |
| 9 | "The maximum allowed staleness for current price endpoints is 30 seconds." | ❌ on `/v1/price`, ✅ on `/v1/price/tip` | See the freshness row in §2 |
| 10 | "Streaming … does not expose raw trade feeds." | ❌ | We expose raw trade feeds **publicly and unauthenticated**: `/v1/history` returns individual Coinbase / Kraken / Bitstamp fills with per-trade timestamp, size, price and venue, and `/v1/observations/stream` is an SSE stream of raw per-source observations. This is over-delivery against the proposal, **but it is unreviewed redistribution of CEX venue data** — venue terms commonly restrict it. Needs a legal/commercial read before launch, independently of any wording fix (**open, for @ash**) |
| 11 | "Unexpected schema changes or anomalous values trigger automatic source quarantine until reviewed." | ❌ | No per-source quarantine exists. The only `Quarantine` in the tree is `internal/projector/sinkfault.go` — a **per-row** retry budget inside the projector. A decoder meeting an unexpected event shape fails closed and surfaces as an ADR-0033 recognition gap plus an alert; it does not quarantine its source. This is a *security-section control claim*, which makes asserting it worse than never having |
| 12 | "API servers are deployed behind load balancers … CDN caching is used for historical endpoints" / "DDoS mitigation at the network edge" | ❌ / ⚠️ | `dig api.stellarindex.io` → `136.243.90.96`, the origin, unproxied, `via: 1.1 Caddy`, no `cf-ray`. There is no CDN and no WAF in front of the API; the only edge protection is Hetzner's included volumetric filtering. The anonymous tier is 6000 req/min per IP and the same box holds the only copy of the lake |
| 13 | "Audit logging of configuration changes" | ❌ | See the RBAC row in §3 |

**Two live product defects surfaced by this re-audit** (they are code, not
copy, and are not fixed by #345):

1. **`/v1/chart` is XLM-alias-blind** — `?asset=native` returns 6 monthly
   points from 2026-03 with `triangulated:true`; `?asset=crypto:XLM`
   returns 35 from 2018-07. Fix is the `assetAliases` loop that
   `history.go`, `ohlc_series.go`, `price_at.go`, `price_changes.go`,
   `observations_stream.go`, `ohlc_fiat_combine.go` and
   `market_sources.go` already run. Whether `handleChartTWAP` /
   `handleChartMarketCapCrypto` share the defect was not traced.
2. **A 61-month hole in XLM/USD history (2021-02 → 2026-02)** that no
   product surface marks. `/v1/coverage` reports `sdex complete:true`
   because completeness is scoped to the lake substrate and the
   projected window, not to CEX backfill extent. Whether the Kraken
   backfill stopped, was never run for that window, or was lost was not
   determined.

**Process defect.** This file sat at `status: current` with
`last_verified: 2026-07-03` for 62 days without CI noticing, because
`scripts/ci/lint-docs.sh` §6 walks only `docs/architecture`,
`docs/operations`, `docs/adr`, `docs/contributing`, `docs/protocols` and
`docs/methodology` — `docs/audit-*` is outside every scan root, so a
compliance table can rot indefinitely. That is the mechanism by which
"keep the table in-repo so the next claim change is reviewed like code"
silently fails. Widening the lint roots was **not** done in this pass
(it is a CI change with its own blast radius across four `docs/audit*`
trees); the file is instead re-stamped and re-labelled a point-in-time
audit. **Open, for @ash.**


## 4. Open source: MET — and the correction that mattered

The repo is PUBLIC, satisfying the RFP's hardest requirement. The
audit's first draft claimed it was private (stale session memory,
unverified) — and checking that claim surfaced that the morning's
drift work had committed the ENCRYPTED ansible vault to the public
repo. Response (same hour): vault removed from the repo, **every
infrastructure secret rotated** (postgres, MinIO root + all three
S3 users, vault password), workflows now materialize the vault from
an Actions secret, services verified healthy on the new credentials.
Vendor keys (Resend, Alchemy, Healthchecks, Massive, CoinGecko demo)
need operator-side rotation — they were in the exposed vault.
Residual: the encrypted blob remains in git history (commit
9c8afc61); with all contents rotated it is inert, but GitHub's
sensitive-data removal process can purge it if desired.

## 5. Fix backlog (board #40–44, wallet-impact order)

1. **#40 SAC price resolution + contract_id in metadata** — a wallet
   looking up `CCW67…` (USDC's SAC) must get USDC's price and the
   classic detail must carry its C-address. Freighter's core lookup
   path.
2. **#41 Freighter V2/bulk completeness** — `change_24h_pct` in batch
   rows; `fdv_usd` + `max_supply` (nullable) + `change_24h_pct` on
   asset detail.
3. **#42 Freshness hardening** — tip publish cadence/jitter so edge
   samples stay ≤30s; document the tip-vs-closed-bucket split
   prominently for wallet integrators.
4. **#43 1-month OHLC granularity + optional `window` param on
   /v1/price** — closes the last RFP-text gaps.
5. **#44 CEX backfill extension for XLM/USD pre-2021** — kraken
   history to listing (~2018); document per-market inception honestly
   in `/v1/markets`.

## 6. Beyond the documents — wallet-builder accommodations

Already-live things wallets get that no document asked for: scam/spam
flags (`issuer_scam_reason`, unverified-collision warnings, two-way
org verification), a verified-asset catalogue, multi-fiat quotes,
SSE streams, per-source provenance on every price, an explorer with
per-protocol verification pages, embeddable price widgets, and a
public completeness verdict (`/v1/coverage`) proving data integrity.

Recommended next accommodations (not started):
- **Asset icons/logos** in metadata (SEP-1 `image` resolution +
  caching + CDN serving) — every wallet needs these; nobody serves
  them reliably.
- **Point-in-time price** (`/v1/price/at?ts=`) for portfolio
  cost-basis/PnL and tax tooling.
- **Batch multi-horizon changes** (1h/24h/7d in one call) for
  portfolio screens.
- **SEP-40 oracle adapter** publishing our aggregate on-chain — makes
  the API consumable by Soroban contracts (Blend-compatible), a
  natural Tranche-III direction the proposal hinted at.
- **Webhook price alerts** for wallets (threshold crossings) — the
  customer-webhook infrastructure already exists; this is a feature,
  not new plumbing.
