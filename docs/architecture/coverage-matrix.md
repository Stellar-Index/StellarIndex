---
title: Coverage and completeness — what /v1/coverage claims, freshness, SLOs, requirement matrix
last_verified: 2026-10-05
status: living doc
---

# Coverage and completeness

What the system claims to cover, how each claim is proven, and the
service objectives it holds itself to. Storage tiers and retention:
[storage-considerations.md](storage-considerations.md). Ingest and
replay: [ingest-pipeline.md](ingest-pipeline.md).

## Completeness: what `/v1/coverage` publishes

The model is [ADR-0033](../adr/0033-completeness-verification-model.md).
A source is COMPLETE through ledger W (its watermark) iff three claims
hold contiguously from the source's genesis to W; coverage is
`(W - genesis) / (tip - genesis)`. A failing ledger pins W and names
what is missing. No threshold, no cursor trust.

| Claim | Field | Check |
|---|---|---|
| 1 substrate continuity | `substrate_ok` | every ledger present, hash chain links (`clickhouse.SubstrateProblem` over the lake) |
| 2a recognition | `recognition_ok` | every `(contract_id, topic)` shape in the lake is matched by a decoder (`Dispatcher.Recognize`) |
| 2b + 3 projection reconcile | `projection_ok` | per ledger, the rows the real decoder re-derives from the lake equal the rows in the served table (`compute-completeness -ch`) |

The verdict is two-axis (ADR-0034):

- **`lake_complete`** = substrate ∧ recognition, genesis to tip: the
  certified ClickHouse archive captured everything in the source's
  domain. "100% coverage" means this.
- **`complete`** is additionally gated by the projection reconcile over
  the projected window. Postgres is the served tier, not the archive,
  so `complete` is retention-scoped: scoped to what has been projected,
  not to a database drop policy. A source can be `lake_complete=true,
  complete=false`.
- `projection_verified_from` is the floor of the projection claim: the
  source's genesis, or the lowest served row where the served tier is a
  declared working-set window (pubnet `sdex` publishes `genesis_ledger`
  2 with a served tier measured from `projection_verified_from` = 61,249,957 on 2026-10-05). Read `complete` together with it.
- Tallies: `complete_sources` / `total_sources` (served axis) and
  `lake_complete_sources` (lake axis).
- A source anchored to pubnet contracts is `not_applicable` on a test
  net: listed with a reason and excluded from every total.
- `flags.stale` has four triggers (`coverage_verdicts.go`): verdict
  `computed_at` older than 26 h (or absent); the network tip (cursor
  extrapolated by wall clock) more than 34,560 ledgers past a verdict's
  tip; a `projection_ok` source whose `projection_evidenced_at` is older
  than 246 h (7 d carry age + 3 x 26 h) or unknown; the `ledgerstream`
  cursor unwritten for 10 minutes. `coverage = 1` means verified to where ingest
  stopped, not to the network tip.
- External CEX/FX sources have no on-chain substrate. Their signal is
  freshness and liveness, reported separately and never folded into the
  on-chain number. `gap_free_pct` and `density_pct` are alerting and
  description only, not coverage claims.

Per-source watermarks live in `completeness_snapshots`, written by
`stellarindex-ops compute-completeness`. Handler:
`internal/api/v1/coverage_verdicts.go`.

## Freshness: what the ≤30 s SLA means

The API serves two freshness contracts on purpose (ADR-0015, ADR-0018):

| Endpoint | Contract | Typical `observed_at` age |
|---|---|---|
| `/v1/price/tip` (+ `/v1/price/tip/stream`) | rolling-window VWAP over the freshest trades, recomputed per request/tick | ≤ 5 s |
| `/v1/price` | last-closed bucket, never an in-progress one, so every region serves the byte-identical answer | 30–150 s by design |

The ≤30 s criterion is met by `/v1/price/tip`: the surface a wallet's
asset page should poll or stream. `/v1/price` trades freshness for
cross-region determinism and cacheability. Integrators choose per use
case.

Evidence: `stellarindex-sla-probe` runs on a 15-min timer on r1
(`configs/healthchecks/stellarindex-sla-probe.timer`,
`OnUnitActiveSec=15min`) and records per-request `observed_at`
staleness against 30 s on `/v1/price/tip`. Both `crypto:XLM` and
`native` hit the rolling window.

## Service objectives and their proof

**Latency: p95 ≤ 200 ms, p99 ≤ 500 ms** (server latency). Two
independent captures agree. The Prometheus histogram
`http_request_success_duration_seconds` under sustained k6 load read
p95 68 ms / p99 98 ms. k6 origin-direct (`00-acceptance-rate.js`,
30 min at 17 req/s against `http://localhost:3000`) read 30,600
requests, p95 54.4 ms, 0 errors, with all thresholds (`p(95)<200`,
`p(99)<500`, `rate<0.001`) green. k6 runs origin-direct because a
single-IP burst trips Cloudflare's anti-abuse layer and then measures
the test source, not the server.

The recurring trail is `docs/operations/sla-proof-<YYYY-MM-DD>.md`:

- Weekly: [`scripts/ops/sla-proof-from-probe.sh`](../../scripts/ops/sla-proof-from-probe.sh),
  committed by `sla-proof-weekly.yml` from the probe's series. It
  measures served latency and availability at concurrency 1 from inside
  the API host, so it excludes DNS, TLS, the proxy and any CDN.
- Load at volume: [`scripts/ci/render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh)
  from a k6 run. It has no target while `K6_TARGET_STAGING` is unset,
  and pointing the 300 rps soak at the single production host is
  refused in code. See [sla-proof-procedure.md](../operations/sla-proof-procedure.md).

**Availability ≥ 99.9 %** over a 30-day month is the published figure
(ADR-0008); see S9.1 below.

**Throughput ≥ 1000 req/min per client.** The origin-direct run held
1031 req/min on one key for 30 min with zero 429s. Shipped defaults:
anonymous 60/min, keys 1000/min (`anon_rate_limit_per_min`,
`key_rate_limit_per_min` in `internal/config/config.go` and
`configs/example.toml`; per key via `mint-key -rate-limit-per-min`).
r1 sets both to 6000 in
`configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`.
Whether 6000/min is the intended public anonymous tier is an open
operator decision.

**Historical depth ≥ 1 year.** Measured on r1 2026-09-08, `min(ts)` per
source: kraken 2017-01-17, soroswap 2024-03-11, aquarius 2024-07-25,
sdex 2026-03-12, coinbase/bitstamp 2026-05-05. `prices_1d` for
`crypto:XLM/fiat:USD` runs from 2017-01-17;
`/v1/chart?timeframe=all&granularity=1d` serves it with
`discontinuous: true` and a declared widest gap of 2017-08-22 →
2018-02-16. SDEX native candles begin 2026-03-12 because the served
tier's SDEX trades do; earlier SDEX history is a post-v1 backfill.
`/v1/ohlc?quote=fiat:USD` combines the USD-pegged constituent pairs per
bucket (`aggregate.ExpandTargetPairWithClassicPegs`, flagged
`triangulated: true`) and reaches XLM/USD 2021-02-01. Migrations 0115
and 0147 recreate the price aggregates `WITH NO DATA`; they lost no
history.

**Open source.** Apache-2.0; builds from a clean checkout (`make build`)
with no proprietary dependencies.

## Requirement coverage matrix

One row per atomic requirement from the product's API requirements,
mapped to the mechanism that meets it. A ❌ row blocks launch. S5.2,
S7.1, F3.4 and X2.2 were re-probed against `api.stellarindex.io` on
2026-10-05; other cells carry their last probe.

#### How to read

Mechanism paths are under `internal/` unless they start with `cmd/`, `docs/`, `pkg/` or `migrations/`.
Status: ✅ verified live, ⚠ shipped with a caveat, ❌ gap (launch blocker), 📦 code/ops-only (not API-testable), ⏳ deferred.
`/v1/coins/*` and `/v1/currencies` are gone: use `/v1/assets/{id|slug}`. `/v1/vwap`/`/v1/twap` `window` needs units (`300s`).

#### Core requirements (S1–S10)

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| S1.1 | Classic asset identity (code+issuer) | `sources/sdex`; `/v1/assets/{id}` | ✅ |
| S1.2 | SEP-41 Soroban token events ingest | `sources/{soroswap,aquarius,…}` | ✅ |
| S1.3 | SAC-wrapped classic (native XLM SAC `CAS3J7…OWMA`) | `canonical` + sources | ✅ |
| S1.4 | Asset enumeration / discovery | `canonical/discovery` | ✅ |
| S1.5 | i128/u128 amounts never truncate | `canonical.Amount`, ADR-0003 | 📦 |
| S2.1 | Reflector (DEX/CEX/FX contracts) | `sources/reflector` | ✅ |
| S2.2 | Redstone (adapter + per-feed proxies) | `sources/redstone` | ✅ |
| S2.3 | Band (native Soroban StandardReference) | `sources/band` | ✅ |
| S2.4 | Chainlink HTTP cross-check (divergence reference, not a VWAP contributor) | `divergence/chainlink.go` | ✅ |
| S2.5 | DIA oracle ("and others"): deferred, blocked on DIA shipping on Stellar mainnet (testnet only); no mainnet integration exists | `sources/dia` (code only) | ⏳ |
| S2.6 | SEP-40-compatible output (others consume our prices) | `api/v1/oracle_sep40.go`; `/v1/oracle/*` | ✅ |
| S3.1 | SDEX trades via ClaimAtom | `sources/sdex` | ✅ |
| S3.2 | Soroswap factory+pair+router events | `sources/soroswap` | ✅ |
| S3.3 | Aquarius (3 pool types) | `sources/aquarius` | ✅ |
| S3.4 | Phoenix (8 events per swap) | `sources/phoenix` | ✅ |
| S3.5 | Comet (Balancer-weighted AMM) | `sources/comet`, ADR-0035 | ✅ |
| S3.6 | Blend auctions as directional signal | `sources/blend`; WASM audit `wasm-audits/blend.md` | ✅ |
| S3.7 | CEX trade ingestion (Binance, Coinbase, Kraken, Bitstamp) | `sources/external/*` | ✅ |
| S3.8 | SushiSwap V3 (concentrated liquidity) | `sources/sushiswap_v3`, ADR-0035; no TVL (tick-based); backfill running, counts a floor | ✅ |
| S3.9 | Upshift vaults (earnUSDC, earnXLM) | `sources/upshift`, ADR-0035/0040; decoder shipped, not in r1 `enabled_sources` | ⏳ |
| S4.1 | Volume-weighted aggregation across venues | `aggregate/orchestrator`, `prices_*` CAGGs | ✅ |
| S4.2 | USD volume on non-USD pairs (triangulation) | `orchestrator/triangulate.go`; `flags.triangulated` | ✅ |
| S4.3 | Per-pair configurable min USD volume | `aggregate.min_usd_volume` in `config` | 📦 |
| S4.4 | TWAP fallback below volume threshold | `/v1/twap` | ✅ |
| S5.1 | Live event ingest | Galexie/MinIO → `ledgerstream` → `dispatcher` → `sources/*` | ✅ |
| S5.2 | ≤ 30 s price staleness | `cmd/stellarindex-sla-probe`; `/v1/price/tip` (see [Freshness](#freshness-what-the-30-s-sla-means)); pager `stellarindex_sla_probe_freshness_breach` ([runbook](../operations/runbooks/sla-probe.md#stellarindex_sla_probe_freshness_breach)) | ✅ |
| S5.3 | SSE streaming | `api/streaming`; `/v1/{price,price/tip,observations}/stream` | ✅ |
| S5.4 | Degradation flags (`stale`, `reduced_redundancy`, `triangulated`, `divergence_warning`) | `api/envelope` | ✅ |
| S6.1 | Since-inception backfill | `stellarindex-ops backfill`; `/v1/history/since-inception` starts 2021-02-01, not 2015 (intent unconfirmed) | ✅ |
| S6.2 | Pre-P20 coverage via ClaimAtom | `sources/sdex` | 📦 |
| S6.3 | Post-P23 unified events | `sources/sdex` | ✅ |
| S6.4 | OHLC continuous aggregates | `prices_{1m,15m,1h,4h,1d,1w,1mo}` CAGGs, ADR-0006, migrations/0002; CAGG `twap` column is an arithmetic mean, `/v1/twap` is the true TWAP | ✅ |
| S6.5 | Retention: 1h+ indefinite, <1h capped | Superseded: raw `trades` and all CAGGs kept forever (AGENTS.md invariant 8, migration 0031); no `drop_after` on `trades` | 📦 |
| S7.1 | Granularities 1m/15m/1h/4h/1d/1w/1mo | CAGGs, ADR-0006; `/v1/ohlc?interval=`, `/v1/chart` | ✅ |
| S7.2 | 1h+ indefinite, <1h capped | Superseded: all indefinite; 0156 90-day `prices_1m` policy ships disabled (`TestRetentionPolicies_AreExactlyTheDeclaredSet`) | ✅ |
| S8.1 | `usd_volume` per trade | `canonical.Trade`, migrations/0001; CAGG `volume_usd` | ✅ |
| S8.2 | FX anchor for USD conversion | `sources/external/{forex,exchangeratesapi}`, `aggregate/stablecoin.go` (USDC/USDT→USD at compute time) | ✅ |
| S9.1 | ≥ 99.9 % availability (published SLA; 99.99 % was the design target) | ADR-0008 HA plan + sla-probe; needs ≥ 30 days off-host measurement; r1 single-region | ⚠ |
| S9.2 | p95 ≤ 200 ms, p99 ≤ 500 ms | ADR-0009, Redis, sla-probe; no PROVEN sla-proof report yet (latest, 2026-10-04, is NOT PROVEN) | ⚠ |
| S9.3 | ≥ 1000 req/min per client | `ratelimit` | ✅ |
| S9.4 | Defined degradation when prices unavailable | `divergence/{coingecko,chainlink}.go`; `flags.divergence_warning` | ✅ |
| S10.1 | Apache-2.0, fully open | `LICENSE` | 📦 |

#### Asset metadata (V1)

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| F1.1 | Asset/Token code | `metadata`; `code` is null on `/v1/assets/native` and soroban assets | ⚠ |
| F1.2 | Current price (USD) | `/v1/price`, closed-bucket per ADR-0015 | ✅ |
| F1.3 | Asset type enum | `canonical.AssetType` | ✅ |
| F1.4 | Issuer address (G…) | `canonical.ClassicAsset` | ✅ |
| F1.5 | Contract address (C…) | `canonical.NewSorobanAsset` | ✅ |
| F1.6 | Home domain (SEP-1) | `metadata` + `applySep1Overlay`, ADR-0007 | ✅ |

#### Historical price chart (V1)

Same as S7. `/v1/chart` accepts `price_type=vwap` only; `twap` is 400 (ADR-0020, reserved). `/v1/twap` single-bar is shipped.

#### Market data V2

Supply runs for operator-watched assets (XLM always; classic/SEP-41 via `[supply].watched_*`); others return null (ADR-0011).

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| F2.1 | Market cap = circulating × price | `assets_f2.go populateMarketCap` + supply pipeline, ADR-0011 | ✅ |
| F2.2 | FDV = max supply × price | same pipeline, ADR-0011; null when `max_supply` null | ✅ |
| F2.3 | 24h volume (USD) | `timescale.Volume24hUSDForAsset` | ✅ |
| F2.4 | Circulating supply | `supply/{xlm,classic,sep41}.go`, ADR-0011 | ✅ |
| F2.5 | Total supply (mint − burn − clawback) | `sep41_supply` observer, ADR-0023 | ✅ |
| F2.6 | Max supply (nullable, off-chain metadata) | `supply/overlay.go`, ADR-0011 | ✅ |

#### Performance SLAs

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| F3.1 | API p95 ≤ 200 ms | `api` + sla-probe `_p95_breach` alert | ✅ |
| F3.2 | API p99 ≤ 500 ms | sla-probe `stellarindex_sla_probe_p99_breach` alert (`deploy/monitoring/rules/sla-probe.yml`) | ✅ |
| F3.3 | Responsiveness ≥ 99.9 % | ADR-0008 + sla-probe; needs ≥ 30 days + multi-region | ⚠ |
| F3.4 | Price freshness ≤ 30 s | `dispatcher` + sla-probe `stellarindex_sla_probe_freshness_breach` pager ([runbook](../operations/runbooks/sla-probe.md#stellarindex_sla_probe_freshness_breach)); as S5.2 | ✅ |
| F3.5 | SEV-1 detect ≤ 15 min / respond ≤ 30 min | `docs/operations/sev-playbook.md`, runbooks, drills | ⚠ |
| F3.6 | SEV-2 detect ≤ 30 min / respond ≤ 60 min | same playbook | ⚠ |

#### Coverage

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| F4.1 | Lookup classic + Soroban by contract address | `canonical.ParseAsset`; `/v1/assets/{id}` | ✅ |
| F4.2 | History ≥ 1 year (ideally since inception) | indefinite retention (invariant 8); `/v1/history/since-inception` reaches 2021-02-01 | ✅ |

#### API characteristics

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| F5.1 | REST or GraphQL | `api/v1` REST; problem+json URIs and OpenAPI host need re-pointing to `stellarindex.io` | ⚠ |
| F5.2 | Rate limits ≥ 1000 req/min | `ratelimit`, `api.key_rate_limit_per_min`; anon 6000/min | ✅ |
| F5.3 | Bulk / batch queries | `GET /v1/price/batch` (≤100), `POST` (≤1000); batch peg rows set `flags.stale=true`, single does not | ✅ |

#### Miscellaneous

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| F6.1 | Price preference VWAP → TWAP → last trade | `/v1/price` + `/v1/twap` | ✅ |
| F6.2 | Quote currency = USD | `defaultPriceQuote`; stablecoin proxy in `aggregate/stablecoin.go` | ✅ |
| F6.3 | Scope = DEXes (Stellar + Soroban) | `sources/*`; 26 sources in `/v1/sources` | ✅ |
| F6.4 | "Since inception" = first recorded trade | backfill orchestrator; served surface starts 2021-02-01 (intent unconfirmed, see S6.1) | ✅ |
| F6.5 | V2 supply provider-supplied | `supply` (3 algorithms) | ✅ |

#### X1. Archive completeness invariants (all ADR-0017, launch-blocking)

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| X1.1 | Primary archive: every closed partition has 64,000 files | `galexie-archive-fill` | 📦 |
| X1.2 | Primary archive: chain-link integrity for every (N, N+1) | `stellarindex-ops verify-archive -tier chain` | 📦 |
| X1.3 | Cross-anchor archive: every checkpoint file present | `/usr/local/bin/cross-anchor-fill` | 📦 |
| X1.4 | Cross-anchor hash matches our LCM at every checkpoint | `verify-archive -tier checkpoint` | 📦 |
| X1.5 | Daily completeness cron | `archive-completeness verify`; `archive-completeness.timer` (4h) active on r1 | ✅ |
| X1.6 | Per-region asymmetric trust (R1 leader, R2/R3 delegate) | `-tier` selection, ADR-0016; R2/R3 do not exist, so not demonstrated | 📦 |
| X1.7 | `checkpointsMissed > 0` is a hard failure | `-fail-on-missed` default on | 📦 |

#### X2. API consistency surfaces (ADR-0018)

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| X2.1 | `/v1/price`: closed-bucket VWAP, cross-region consistent | ADR-0015, `price.go` | ✅ |
| X2.2 | `/v1/price/tip`: rolling-window VWAP + last-good fallback | `price_tip.go`; `native` and `crypto:XLM` both hit the rolling window | ✅ |
| X2.3 | `/v1/observations`: raw per-source data | `observations.go`; `/v1/history` on a triangulated pair still returns silent `[]` with `triangulated=false` | ✅ |
| X2.4 | Query params must not change the consistency contract | OpenAPI lint + per-handler `reject*TierParams` tests | 📦 |
| X2.5 | Forex factor snap rule for chained-fiat consistency | `triangulate.go::legPrice`, `FXQuoteAtOrBefore`, `FXSources` | 📦 |
| X2.6 | Streaming per surface | `price_stream.go`, `price_tip_stream.go`, `observations_stream.go`, `api/streaming`; tip-stream `observed_at` minute-bucketing fixed and deployed (`8fde6c84`; live `native` stream `observed_at` 4 s old on 2026-10-05) | ✅ |
| X2.7 | Per-surface `flags.stale` semantics | `envelope.go`; `/v1/price` sets true on degradation, tip and observations always false | ✅ |

#### X3. Anomaly response and confidence scoring (ADR-0019)

| ID | Requirement | Mechanism | Status |
|---|---|---|---|
| X3.1 | Per-asset-class threshold defaults (Phase 1) | `aggregate/anomaly` | ✅ |
| X3.2 | Per-asset statistical baseline (Phase 2) | `aggregate/baseline`, migration 0007 | 📦 |
| X3.3 | Multi-factor confidence score | `aggregate/confidence`; not on the public API | 📦 |
| X3.4 | Freeze policy (3-signal AND, closed-bucket only) | `aggregate/freeze` | ✅ |
| X3.5 | ADR-0019 Phase 3 cross-oracle confidence factor: built — divergence references (Reflector ×3, RedStone, Band, synthetic USD-cross) feed `CrossOracleFactor` through the `div:` cache; live `/v1/price` reports `cross_oracle_checked` | `orchestrator/confidence.go::lookupCrossOracle`, `TestConfidence_DivergenceWiredFromCache` | ✅ |
| X3.6 | Multi-window anti frog-boiling (1d/7d/30d MAD) | `baseline/multi.go`, migration 0008 | 📦 |
| X3.7 | Bootstrap (warmup) policy for new assets | `baseline/refresh.go` MinSamples gate | 📦 |
| X3.8 | Operator runbook for freeze events | `docs/operations/runbooks/anomaly.md` | 📦 |

#### Gap triage

Every outstanding item is launch-blocking except ⏳ ones. Tracking: `docs/operations/v1-launch-plan.md`. Closed items: git history of this file.

**Open, implementation pending**

- Validation, S9.2 p95 ≤ 200 ms proof report: generator `scripts/ops/sla-proof-from-probe.sh` runs and writes `docs/operations/sla-proof-*.md`, but every report's verdict is NOT PROVEN (an endpoint misses its latency or availability target). Done when a report reads PROVEN (`docs/operations/sla-proof-procedure.md`).
- F-D, SEP-10 challenge returns 503 on r1 ("server signing seed isn't configured"; carry-over since 2026-05-10, R-009). Operator config step, not a code gap; inventory INV-2677. Code-true, production-false.
- Validation, #19 chaos suite Wave 2 (Patroni promotion, Sentinel failover, HAProxy VIP flip) in `test/chaos`. Wave 1 (dev-stack smoke, `scenarios/01–04`) shipped. Needs an HA topology; r1 is single-node, so post-launch.

**Watch (post-launch only, explicitly accepted)**

1. S2.5 DIA mainnet ship: testnet only today; integration conditional on DIA's mainnet launch.
2. S9.1 99.9 % availability measurement: needs ≥ 30 days production; number reported 90 days post-launch.
3. Residual DeFi with no decoder (INV-1130, lake census 2026-07-10). FxDAO's FXG and stablecoins are supply-watched only. Re-audit if activity resumes.
   - FxDAO Vaults `CCUN4RXU5VNDHSF4S4RKV4ZJYMX2YWKOH6L4AKEKVNVDQ7HY5QIAO4UB` (0 events; would need a `ContractCallDecoder` like Band). Slender pool `CCL2KTHYOVMNNOFDT7PEAHACUBYVFLRH2LYWVQB6IPMHHAVUBC7ZUUC2`; dormant since ledger 60,749,975; re-audit if `deposit`/`borrow` resume.
   - EquitX: the documented orchestrator `CCU3FICCTH56KER3YR75NSLXC2BM24RSKNCW6ZT4JGRYYLO5K4FUP24I` has 0 events; the real `CDP` events are on `CCAKOTMHZ63UZIFRCWWABLIX5VP4DST2JANHFJLUR7CK4SGIKUPUDBA6` (3) and `CA3BB35F2EK4ADN4SI3QJAKWG2OQ3S5NKLFRV4H24FEHZ3CA2GHNDGAK` (1), ledgers 59,107,932 to 59,480,808. Confirm with a live RPC check before hard-coding.
   - Orbit CDP and Laina are not on mainnet; re-audit on a mainnet-launch announcement, mainnet addresses in Orbit's `orbit-utils`, or Laina CI/frontend pointing at mainnet.

Availability claim: the proposal's 99.99 % is not the published commitment (99.9 % is); evidence is `test/load/` k6 plus a PROVEN sla-proof report.

#### Verification protocol

If a reviewer disputes a ✅ cell, re-run its method: source read, SEP/CAP spec read, on-chain query (record contract + WASM hash), or a Go test with a fixture (canonical: KALIEN i128 regression, `internal/canonical/amount_test.go`). An external doc is weaker evidence; use it only where it is primary.
(#1271)
R-013 → #1265
(PRs #1261, #1262, #1263, #1268, #1270)
Deferred #1347 — go-stellar-sdk v0.5->v0.6
#1353
#1369
