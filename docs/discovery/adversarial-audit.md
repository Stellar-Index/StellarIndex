# Adversarial audit of Phase 1 discovery

**Purpose:** a deliberately hostile review of my own work before we move
into Phase 2. Written after the user asked for "a full adversarial
audit against your declared findings before we move forwards too."

Nothing in this doc is new discovery. It is a catalogue of:

1. **Gaps** — things the RFP and our proposal require a source for,
   that I have not audited at all.
2. **Weakly-sourced claims** — statements I presented as facts but
   that rest on a WebSearch summary, a WebFetch round-trip that a
   small model summarised for me, or second-hand documentation —
   not a file I read.
3. **Unverified addresses and numbers** — specific mainnet contract
   addresses, hardware numbers, or CAP activation points that I
   reproduced from our proposal or from stellar-docs without
   checking against a live source or the underlying spec.
4. **Inferred-but-not-tested behaviour** — places where I reasoned
   about something from its type definitions or docs but did not run
   a test that would falsify the claim.
5. **Internal inconsistencies** — places where two of my own docs
   disagree, or where my docs and the proposal disagree without my
   reconciling them.
6. **Promised-but-not-executed work** — fixtures, benchmarks,
   cross-checks, and traceability I wrote as "open items" and then
   never started.

Unless a claim appears here, I stand behind it. The purpose of this
doc is to cap that list and make the residual risk visible.

## 1. Sources we have NOT audited at all

The RFP / proposal promises to ingest from these categories. None of
them have a discovery doc today.

### 1a. Centralized Exchanges

Proposal: "Trade and ticker data are ingested from selected
centralized exchanges using WebSocket streams where available, with
REST polling as a fallback."

Zero discovery done. No audit of:

- Which specific CEXes (Binance, Coinbase, Kraken, Bitfinex, OKX,
  Bybit, …).
- Per-venue API endpoints, auth model, rate limits.
- Per-venue symbol conventions (XLM-USDT vs XLMUSD vs XLM/USDT …).
- Pair coverage per venue for Stellar-listed assets.
- WebSocket vs REST trade-offs per venue.
- Historical-data availability (some CEXes limit historical trades
  heavily).
- Cost (free public endpoints vs. paid tiers).

### 1b. Forex Providers

Proposal: "Foreign exchange data is sourced from institutional-grade
FX feeds."

Zero discovery done. No audit of:

- Which providers (OANDA, Polygon.io, Alpha Vantage, Xignite,
  Fixer.io, ECB-derived feeds, …).
- Licensing / redistribution rights.
- Cost tier needed for our use case.
- Symbol coverage for the fiat pairs the RFP example mentions
  (XLM/EUR, XLM/GBP, AQUA/BRL).
- Update frequency / latency profile.

### 1c. Industry reference providers

Proposal: "Reference pricing from industry aggregators such as
CoinGecko and CoinMarketCap."

Zero discovery done. No audit of:

- CoinGecko API endpoints, rate limits, pricing. Especially the
  `/coins/{id}/history`, `/simple/price`, and
  `/coins/{id}/market_chart` endpoints we'd actually call.
- CoinMarketCap Pro API tier requirements.
- How to map Stellar asset codes to CoinGecko IDs (`stellar` for
  XLM, token-specific IDs for others).
- Redistribution licensing — material concern, the RFP requires us
  to expose these sources in our aggregated output.

### 1d. Supply data for the Freighter V2 scope

Freighter RFP V2 explicitly lists `Circulating Supply`, `Total Supply`,
`Max Supply`, `Market Cap`, `FDV`, `24h Trading Volume`. The
spec says "V2 supply data source: Provider-supplied" — i.e. **we
provide it**.

Zero discovery done on:

- How we derive circulating supply for classic assets (issuer balance
  vs. distributed? trustline enumeration?).
- How we derive circulating supply for SEP-41 Soroban tokens (SAC
  `total_supply()` + admin exclusions?).
- How we determine the "max supply" field, which is asset-specific
  and often metadata-only.
- Whether we source these from on-chain, CoinGecko, or a mix.

This is **explicitly in scope** for Freighter V2 and we have no
plan.

### 1e. Asset metadata / stellar.toml / SEP-1

Stellar RFP requires `Home Domain` as a field for classic assets.
Populating it means resolving `AccountEntry.HomeDomain` → fetch
`https://<home-domain>/.well-known/stellar.toml` → parse per SEP-1 →
extract token info.

No audit of:

- SEP-1 spec itself.
- Caching strategy / refresh cadence for home-domain metadata.
- Handling failed fetches / redirects.
- How we combine on-chain `HomeDomain` with SEP-1 `[[CURRENCIES]]`
  data (asset name, image, description) for our API response.

### 1f. API layer, auth, rate limit, streaming

Proposal sections 4.5–4.10 (Asset Identification, Current Price,
Historical/OHLC, Batch Queries, Streaming, Rate Limiting, Error
Handling) describe the serving layer. None of these have a design
audit doc. The Freighter RFP specifies:

- p95 ≤ 200 ms, p99 ≤ 500 ms, 99.9% uptime.
- 1000 requests/min per client.
- SEV-1 / SEV-2 incident response.

No plan captured for:

- API gateway choice + auth model (API keys? mTLS? Stellar-keypair
  auth?).
- Rate-limit algorithm + store (Redis bucket? in-process?).
- CDN strategy for cacheable historical endpoints.
- Streaming transport (Server-Sent Events per proposal — verified
  feasibility?).
- Canary / shadow-traffic / load-test plan.

### 1g. Infrastructure (TimescaleDB, Redis, MinIO, hosting)

`docs/discovery/infrastructure/` was created empty in the original
scaffold and never populated. We have [archival-nodes.md](data-sources/archival-nodes.md)
for core/rpc/galexie, but not for:

- **TimescaleDB** — hypertable schema, retention policies, continuous
  aggregates (**the feature set we specifically chose TimescaleDB
  for**, and haven't designed).
- **Redis** — sharding / replication / failover / hot-cache schema.
- **MinIO** — cluster topology, erasure-coding, backup strategy.
- **Hosting** — reverse-proxy (Traefik? nginx?), TLS, logging stack,
  metrics backend (Prometheus per proposal — but no scrape config
  sketch), alerting (Alertmanager? PagerDuty?).

### 1h. DEXes / AMMs I missed

I audited Soroswap, Aquarius, Blend. I did **not** audit:

- **Phoenix** — ✅ RESOLVED 2026-04-22. See
  [dexes-amms/phoenix.md](dexes-amms/phoenix.md). Unusual 8-event-
  per-swap schema, mainnet addrs captured.
- **Comet** — ✅ RESOLVED 2026-04-22. See
  [dexes-amms/comet.md](dexes-amms/comet.md). Structured event
  bodies, Balancer-weighted AMM, `comet.wasm` vendored in Blend
  for backstop.
- **FxDAO, OrbitCDP, EquitX, Laina, Slender, DeFindex** — still
  not audited. Listed in our Reflector audit as integrators; their
  events may add niche signal. Residual open.
- **Aquarius `liquidity_pool_concentrated`** — flagged in my
  Aquarius audit as "WIP on feature branch." Status at Phase 2
  start should be re-verified; may have launched.

### 1i. Soroban token / SAC references

- **SEP-41** — ✅ RESOLVED 2026-04-22. See
  [notes/sep-41-token-events.md](notes/sep-41-token-events.md).
  Read v0.4.1 spec from `stellar-protocol/ecosystem/sep-0041.md`.
  Key finding: `transfer`/`mint` data can be either simple i128 or
  map with `to_muxed_id` — decoder must type-test. Total-supply
  definition locked (`mint+ / burn- / clawback-`).
- **Stellar Asset Contract (SAC)** — the way classic assets get a
  Soroban face. Affects routing: the same underlying asset can trade
  as a classic `code:issuer` pair on SDEX *and* as a Soroban
  contract-token on Soroswap. Deduplicating / unifying the two sides
  is non-trivial and we have no design for it.
- **OpenZeppelin stellar-contracts** — widely used as a reference
  implementation for SEP-41 and governance. Their event conventions
  could inform our decoder's permissiveness.

### 1j. Reference ingestion implementations we haven't read

- **stellar/stellar-etl** — the open-source Go pipeline that powers
  Hubble's BigQuery dataset. I referenced it but never cloned. This
  is a major reference alongside `stellar-extract` for
  ledger→typed-row mapping.
- **stellar/quickstart** (beyond its README) — complete docker
  composition of core+rpc+horizon+galexie. Useful for comparing our
  two-captive-core design against SDF's reference composition.

### 1k. SEPs and CAPs I linked to but never read

Direct links to these in my docs, zero audit of content:

- **SEP-1** (stellar.toml).
- **SEP-10** (Stellar Web Authentication) — potentially our API
  auth mechanism.
- **SEP-20** (validator self-verification) — cited in our Tier-1
  decision.
- **SEP-23** (strkey / address encoding).
- **SEP-41** (Soroban token standard).
- **CAP-27** (muxed accounts) — I asserted "P17 activation" without
  source. See §3.
- **CAP-67** — ✅ RESOLVED 2026-04-22. See
  [notes/cap-67-unified-events.md](notes/cap-67-unified-events.md).
  Read full spec from `stellar-protocol/core/cap-0067.md`. Key
  findings: unified events ship at P23 (mainnet 2025-09-03);
  classic events have 4 topics (extra `sep0011_asset` vs SEP-41's
  3 topics); 3 new `SCAddressType` variants (muxed /
  claimable-balance / liquidity-pool) our decoder must handle;
  post-P23 SDEX trades emit two `transfer` events per filled
  offer.
- **CAP-58** (constructors), **CAP-59** (BLS12-381),
  **CAP-62**/**CAP-66** (state archival) — linked, not read.
  Residual open, lower priority for pricing.
- **CAP-75** (Poseidon), **CAP-79** (BN254).

## 2. Claims that rest on secondary sources, not primary

These went into the audit docs as if I had read the code. In fact I
had only a WebFetch summary, a WebSearch result, or second-hand
stellar-docs phrasing.

| Claim | Where I put it | What I actually had |
| ----- | -------------- | ------------------- |
| Band contract full interface (`get_ref_data`, `get_reference_data`, `relay`, `force_relay` + struct layouts) | [oracles/band.md](oracles/band.md) | **✅ RESOLVED 2026-04-22** — cloned and read `bandprotocol/band-std-reference-contracts-soroban`. Two corrections made: struct name is `ReferenceDatum` not `ReferenceData`; pair rate is E18-scaled (verified from `reference_data.rs:30-42`: `rate = base.rate * E18 / quote.rate`). Also confirmed no events emitted (poll-only). |
| Redstone "Adapter + per-symbol Feed" push model with U256 price | [oracles/redstone.md](oracles/redstone.md) | **✅ RESOLVED 2026-04-22** — cloned `redstone-finance/redstone-public-contracts` and read the source. Architecture confirmed: one Adapter contract + thin per-feed proxies (all 19 share the same WASM hash). U256 price field confirmed in `common/src/lib.rs:12-18`. **Also found: adapter DOES emit events** (topic `"REDSTONE"`). |
| Redstone "10 active feeds BTC/ETH/USDC/PYUSD/BENJI at launch" | [oracles/redstone.md](oracles/redstone.md) | **✅ RESOLVED 2026-04-22** — Full list of 19 mainnet feeds verified via <https://app.redstone.finance/push-feeds?networks=stellar>. All 19 contract addresses captured. Includes a uniquely rich RWA set (BENJI/iBENJI/GILTS/CETES/KTB/TESOURO/USTRY). |
| Chainlink "Stellar joining Chainlink Scale" scope and 2026 timing | [oracles/chainlink.md](oracles/chainlink.md) | WebSearch summary of Stellar's press-release. The primary announcement is a blog post, not a spec. |
| DIA testnet address `CAEDPEZD…5IFP4` is SEP-40 compatible | [oracles/dia.md](oracles/dia.md) | Inferred because stellar-docs groups it with other SEP-40 oracles. Not verified by reading DIA's Soroban contract source. |
| Reflector Pulse has "uniform 5-minute update interval" | [oracles/reflector.md](oracles/reflector.md) | **Actually IS in primary source** (`reflector-contract/README.md`) but I cited it to the WebSearch result instead. Fact is right; citation chain is weak. |

These are all **either true or directionally correct** — but until I
read the primary, they are not safe to bet production code on.

## 3. Unverified mainnet addresses and specific numbers

### Blend — ✅ RESOLVED (2026-04-22, follow-up round)

Via stellar.expert's public contract API, **both addresses verified
as real, deployed, WASM-hash-matched Blend V2 contracts**:

- Pool Factory V2 `CDSY…4QSU`: deployed 2025-04-14, WASM
  `31328050…d755ca9`, verified against
  `blend-capital/blend-contracts-v2` commit
  `c19abee5b9be4f49e0cda9057e87d343e5dcc095`, package
  `pool-factory`.
- Backstop V2 `CAQQ…G3IM7`: deployed 2025-04-14 (same day, seconds
  later), WASM `c1f4502a…faffbc2`, same repo/commit, package
  `backstop`. 43,948 events at audit time — heavy prod usage.

Updated in [dexes-amms/blend.md](dexes-amms/blend.md).

### Aquarius — ✅ RESOLVED (2026-04-22, follow-up round)

Mainnet router:
`CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK`.
Sourced from <https://docs.aqua.network/developers/code-examples/prerequisites-and-basics>
and verified via stellar.expert. Deployed 2024-07-25, 1.8M events,
verified against `AquaToken/soroban-amm` commit `38abab4…`, package
`soroban-liquidity-pool-router-contract`.

Also captured: the canonical **XLM SAC** used by Aquarius docs:
`CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA` — this
is network-wide, not Aquarius-specific.

Factory / plane / calculator / locker_feed / fees_collector
addresses still need to be derived from router reads or asked of
the Aquarius team — residual open item in
[dexes-amms/aquarius.md](dexes-amms/aquarius.md).

### Soroswap — **now verified**

The one address-set I **have** verified: Soroswap factory +
router match
`.discovery-repos/soroswap-core/public/mainnet.contracts.json`:

```json
{
  "ids": {
    "factory": "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2",
    "router":  "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"
  },
  "hashes": {
    "pair":    "18051456816b66f12e773a56f77c5794fac1b1fb7ab6e22d4fad5a412770f73e"
  }
}
```

**Bonus:** that file also yields the **Soroswap pair WASM hash** —
which means we can detect Soroswap pairs by their `contractCodeHash`
on-chain instead of relying solely on walking factory events. I did
not incorporate this into the soroswap audit.

### Reflector three contracts

From `stellar-docs/docs/data/oracles/oracle-providers.mdx`, which is
SDF-maintained. Acceptable as a secondary source but a live call to
each `base()` / `decimals()` / `assets()` would pin the facts.

### Hardware numbers from SDF's validator docs

`c5d.2xlarge` = 8 vCPU / 16 GB / 100 GB NVMe is the **April 2024**
SDF recommendation. ledger size was ~10 GB then. It is **April 2026**
now. These numbers are likely stale; our sizing should not take them
at face value. Plan: run our own catchup on a test node and measure
current bucket + DB sizes.

### Protocol activation ledgers

`protocol-versions.md` gives activation *dates* from
`software-versions.mdx`. Ledger numbers at each boundary are **not**
captured. That's what's actually needed for our protocol-boundary
test fixtures, not the dates. Open.

## 4. Inferred-but-not-tested behaviour

### "Galexie preserves native-epoch XDR, not upcasts"

The strongest architectural claim in
[protocol-versions.md](protocol-versions.md). My evidence:

- The XDR union `LedgerCloseMeta { v: int32; v0/v1/v2 }` exists.
- The SDK's reader knows how to dispatch.

**What I did not verify**: whether modern `stellar-core` during
catchup/replay actually *writes* `v0`-armed meta for an old-protocol
ledger, or whether it writes `v2` with appropriate fields zeroed.

This is an empirical question. The architectural implication if I'm
wrong: we never encounter `v0`/`v1` meta in practice, all our
version-aware switching is dead code, and the SDK reader simply
handles `v2` for everything. That's actually a **simpler** world for
us — the concern is the other direction, that I've under-counted
cases we need to handle. The safe plan is the same either way (keep
the SDK helpers, keep the ClaimAtom switch) but we should
*empirically* replay a pre-P18 ledger through our consumer once and
see what `lcm.V` is.

### "Muxed accounts activated at Protocol 17"

Asserted in `protocol-versions.md` with no citation. Not visible in
the `software-versions.mdx` doc I actually read (which starts at
P20). I took it from general knowledge. **Should verify** against
`stellar-protocol/core/cap-0027.md` or the older release notes
before treating as fact.

### "Every Soroswap swap is followed by a sync in the same op"

I *did* verify this via the `update()` function call chain in
`contracts/pair/src/lib.rs`. Solid.

### "cdp-pipeline-workflow silently truncates i128 to low-64"

Verified from source. Solid.

### "CATCHUP_COMPLETE takes weeks" (for pubnet from genesis)

Direct quote from SDF's `running-node.mdx`. Solid, but the actual
time on modern hardware with a pre-seeded archive mirror may be very
different. We should measure before planning a validator launch.

## 5. Direct contradictions with our proposal that I've surfaced

These are **real corrections** that need to flow back into
`ctx-proposal.md`:

1. **Reflector has no on-chain `twap` / `x_*` methods.** Proposal
   lists these as if they were SEP-40 methods. They aren't, and
   they aren't implemented in Reflector v3.
2. **Soroswap `SwapEvent` does not carry post-state reserves.**
   Proposal says it does.
3. **Band has a native Soroban contract on mainnet** today. Proposal
   says "via the BandChain REST API."
4. **Reflector is three separate contracts**, not one — one per
   data source (DEX, CEX, FX). Proposal describes Reflector as if
   one integration.
5. **cdp-pipeline-workflow has multiple correctness bugs** (i128
   truncation in Soroswap router, offer-body-as-trade in SDEX,
   no cursor persistence in captive-core adapter, row-by-row
   inserts). Proposal does not mention cdp-pipeline-workflow but
   anyone reading the proposal might assume withObsrvr's components
   are batteries-included — they aren't.

None of these have been pushed back into `ctx-proposal.md` yet.

## 6. Open questions that affect our architecture

### 6a. go-stellar-sdk version skew

- `stellar-galexie` pins `go-stellar-sdk v0.4.0`.
- `withObsrvr/stellar-extract` pins `go-stellar-sdk v0.5.0`.

If we import both in one Go module we pick the higher version (MVS
rules). I asserted "likely fine" without actually running `go mod
tidy` in a test module. **Not verified.** If there are breaking
changes between v0.4 and v0.5 we might have to vendor or fork.

### 6b. MinIO + Galexie `endpoint_url`

Proposal relies on MinIO via the S3-compat path. Galexie's
`config.example.toml` explicitly names Cloudflare R2 as an example of
an S3-compat endpoint. MinIO should work analogously — but I haven't
done a smoke test. The `aws-sdk-go-v2/service/s3` dependency in
Galexie's `go.mod` supports custom endpoints; that's the *capability*,
not a verification we've made it work.

### 6c. stellar-rpc full-event-history retention on SQLite

I flagged that `HISTORY_RETENTION_WINDOW` is tunable up to arbitrary
size in ledgers. I asserted that SQLite "almost certainly" won't
sustain 31M ledgers of event history at our p95 target. **Not
benchmarked.** Our plan currently routes historical event reads
through Galexie + our own indexer, but this depends on being right
about the SQLite ceiling.

### 6d. Galexie + stellar-rpc co-resident captive-cores

Two captive-core instances on the same host. I hand-waved "fine on
our R640." **No measurement, no memory-pressure calculation, no
file-descriptor accounting.** Captive-core is cpu+memory intensive
during catchup.

## 7. Fixtures / tests / benchmarks I proposed but haven't executed

- i128 round-trip regression fixtures
- Protocol-boundary fixtures (pre-P18 / post-P18, pre-P20 / post-P20,
  pre-P23 / post-P23)
- Per-ClaimAtom-variant fixtures (V0, OrderBook, LiquidityPool)
- Per-trade-op fixtures (ManageSell/Buy, CreatePassiveSell,
  PathPaymentStrictSend/Receive)
- Soroswap swap+sync pairing fixture
- Aquarius multi-asset (3-asset, 4-asset) trade fixture
- Cross-verify our trade counts vs. Hubble `history_trades`
- MinIO+Galexie smoke test
- stellar-rpc SQLite benchmarks at large retention windows
- rs-stellar-archivist concurrency sweep
- Galexie export throughput on R640
- Live event throughput on pubnet per indexed protocol (Soroswap,
  Aquarius, Blend, Reflector)

All of these are bullet lists in various audits. Zero executed.

## 8. Internal inconsistencies / loose ends

- My [protocol-versions.md](protocol-versions.md) says "muxed
  accounts at P17" without source; my other docs don't mention the
  muxed-account handling detail. These should either both handle it
  (with a fixture) or neither (remove the unsourced claim).
- My [oracles/redstone.md](oracles/redstone.md) says the Redstone
  on-chain storage is `U256` per our proposal, but also notes we
  couldn't find the actual contract. If it turns out to be `i128`
  instead, the "u256 → need 256-bit support" note is misleading.
- [decisions.md](decisions.md) says MinIO is a "working decision"
  while README calls it 🧪 — the actual status is "we committed
  with MinIO in Phase 1 unless proved wrong in load test." Should
  reconcile wording.
- [data-sources/withobsrvr-overview.md](data-sources/withobsrvr-overview.md)
  lists 70 withObsrvr repos but I only audited ~10. I labelled the
  other ~60 as ❓ without distinguishing "obviously irrelevant"
  (e.g. `python-fbas` — Byzantine FBA reasoning) from "plausibly
  relevant but skipped" (e.g. `obsrvr-stellar-components`,
  `stellar-network-monitoring-mcp`). The audit notes don't make the
  distinction.

## 9. Where I explicitly over-hedged or under-hedged

Over-hedging (added uncertainty where there shouldn't be):

- Reflector Pulse 5-minute cadence — I flagged this as "from
  WebSearch" but it IS in the primary README. Fact stands.
- MinIO + S3-compatibility — Galexie's own example explicitly lists
  Cloudflare R2 as supported via `endpoint_url`. MinIO uses the
  same mechanism. The "open item" framing downplays how well-known
  this path is.

Under-hedging (presented as sure when really a judgement call):

- "cdp-pipeline-workflow is not forkable" — I found 6 bugs. That's
  strong. But "not forkable" is a judgement; some teams fork and
  fix. I should have said "we do not fork — the cost of fixing
  exceeds re-implementation with a clean design."
- "DIA is not a real integration yet" — only a testnet contract is
  listed. I treated this as "not on our roadmap." Fine, but the
  RFP doesn't exclude DIA, and if DIA ships a mainnet deployment
  during our 10-week delivery window, we should pick it up.

## 10. Priority-ranked "close this before we code" list

### Blocking for Phase 2 (ingestion)

1. Clone `bandprotocol/band-std-reference-contracts-soroban` and
   read the Rust source. Re-verify our Band audit.
2. Find and clone Redstone's actual Stellar contract / connector.
   Read the Rust source. Capture real mainnet addresses.
3. Verify Blend Pool Factory + Backstop mainnet addresses via
   stellar.expert or Blend's deploy manifest.
4. Find the Aquarius router mainnet address; enumerate at least the
   top-20 deployed pools.
5. **Audit Phoenix DEX.** Clone their contract repo, map event
   schema, capture mainnet addresses.
6. **Audit Comet.** Verify whether there is a public trading
   deployment beyond Blend's backstop usage.
7. Read SEP-41 spec in full.
8. Read CAP-67 spec in full.
9. Run a minimal test module that depends on both
   `stellar-galexie` and `withObsrvr/stellar-extract`; confirm
   their different `go-stellar-sdk` pins resolve together.
10. Empirically replay a pre-P20 pubnet ledger through our consumer;
    observe `lcm.V`. Settle the "Galexie preserves native XDR"
    question.
11. Build the fixture set (per-ClaimAtom, per-op-type, per-protocol-
    boundary). At minimum the 6 happy-path cases + 2 boundary cases.

### Blocking for Phase 3 (aggregation)

12. CEX discovery: pick venues, write a `external-refs/cex-feeds.md`
    with concrete connector choices (`ccxt`? hand-rolled WS?).
13. FX discovery: pick a provider, write
    `external-refs/fx-feeds.md`.
14. CoinGecko / CoinMarketCap audit + licensing check. Write
    `external-refs/coingecko.md` and `external-refs/coinmarketcap.md`.
15. Supply-data audit: where does circulating supply come from?
    How do classic + SEP-41 inventories combine? Write
    `data-sources/supply-data.md`.
16. SEP-1 / home-domain audit. Write `data-sources/sep1-metadata.md`.

### Blocking for Phase 5 (API)

17. API layer design doc: auth, rate limiting, SSE, caching,
    versioning. Write `infrastructure/api-layer.md`.
18. Rate limit store / quota design. Redis-based, per-key + per-IP.
19. Home-domain / SEP-1 resolution and caching strategy.

### Blocking for Phase 6 (SLA validation)

20. TimescaleDB schema + hypertable + retention policy design.
    Write `infrastructure/storage-timescaledb.md`.
21. Redis cache schema + failover design.
    Write `infrastructure/cache-redis.md`.
22. MinIO cluster topology decision.
    Write `infrastructure/storage-minio.md`.
23. Hosting design — reverse proxy, TLS, metrics scrape, alerting.
    Write `infrastructure/hosting.md`.
24. Load-test plan. How do we hit 1000 req/min × many clients and
    prove p95 ≤ 200 ms?
25. SEV-1 / SEV-2 playbook.

### Not blocking, but should do before declaring Phase 1 complete

26. RFP requirements traceability matrix:
    `discovery/rfp-requirements-matrix.md`. Every RFP promise
    (bullet in the scope) → which source(s) fulfil it → which
    audit doc describes those sources → gaps.
27. Push corrections back into `ctx-proposal.md` (Reflector TWAP,
    Soroswap swap-event reserves, Band REST-vs-Soroban, Reflector
    three-contract model, cdp-pipeline-workflow bug disclosures).
28. Clone `stellar/stellar-etl` and read. Second reference
    implementation for ledger→typed-row mapping.
29. Pin a `VERSIONS.md` at repo root listing commit SHAs /
    versions we depend on.

## 11. What I do still stand behind

To avoid an all-or-nothing read of this doc: the following claims
are **solidly sourced** and should not need re-verification.

- **Galexie**: subcommand set, config schema, zstd compression,
  captive-core integration, storage-backend-drops-metadata-on-fs.
  Verified from `stellar-galexie` source and
  `go-stellar-sdk/support/datastore/filesystem.go`.
- **rs-stellar-archivist**: 2 subcommands (scan, mirror); file://
  write-only; multi-backend read; clap-defined flags. Verified from
  `rs-stellar-archivist/src/cli/*.rs` and `src/storage.rs`.
- **stellar-ledger-data-indexer**: Soroban-contract-data-only
  scope, 2 tables. Verified from migrations directory and grep.
- **XDR ClaimAtomType** variants: three of them, amounts are
  `Int64`. Verified from `xdr_generated.go`.
- **XDR LedgerCloseMeta / TransactionMeta** union structure.
  Verified from `xdr_generated.go`.
- **SEP-40** interface (`base`, `assets`, `decimals`, `resolution`,
  `price`, `prices`, `lastprice` only). Verified from SEP-40
  markdown (though via WebFetch — but the SEP is small enough that
  the summary is faithful).
- **Reflector Pulse vs Beam** contract interfaces. Verified from
  `reflector-contract/{pulse-contract,beam-contract,oracle}/src/`.
- **Reflector event**: topic `["REFLECTOR", "update"]` with
  `timestamp` indexed and `Vec<(Val, i128)>` payload. Verified from
  `oracle/src/events.rs`.
- **Soroswap pair events** (deposit, swap, withdraw, sync, skim)
  and the `update()` → `sync` pattern. Verified from
  `pair/src/event.rs` + `pair/src/lib.rs`.
- **Soroswap factory event** (`new_pair`). Verified from
  `factory/src/event.rs`.
- **Soroswap mainnet addresses + pair WASM hash.** Verified from
  `public/mainnet.contracts.json`.
- **Aquarius event schema** (deposit_liquidity, withdraw_liquidity,
  trade, update_reserves, reserves_sync, kill_*). Verified from
  `liquidity_pool_events/src/lib.rs`.
- **Aquarius plane batch-read function signature.** Verified from
  `liquidity_pool_plane/src/interface.rs`.
- **Blend pool event set** including `new_auction`, `fill_auction`,
  `delete_auction`, `supply`, `withdraw`, `borrow`, `repay`,
  `flash_loan`. Verified from `pool/src/events.rs`.
- **Blend pool-factory `deploy` event.** Verified from
  `pool-factory/src/events.rs`.
- **stellar-extract's trade extraction** handles ClaimAtomV0 +
  OrderBook correctly. Verified from `trades.go`.
- **stellar-extract's i128 handling** (big.Int + two's-complement
  sign handling) is correct. Verified from `scval_converter.go`.
- **cdp-pipeline-workflow's i128 low-bits bug** in
  `processor_soroswap_router.go`. Verified from source.
- **cdp-pipeline-workflow's SDEX-offer-as-trade bug** in
  `processor_transform_to_app_trade.go`. Verified from source.
- **Galexie per-object metadata is 9 keys.** Verified from
  `go-stellar-sdk/support/datastore/object_metadata.go`.
- **stellar-rpc uses SQLite only.** Verified from
  `stellar-rpc/cmd/stellar-rpc/internal/db/db.go`.
- **stellar-rpc retention-window config keys and defaults.**
  Verified from `internal/config/options.go`.
- **stellar/go archived 2025-12-16, superseded by
  `stellar/go-stellar-sdk`.** Verified from GitHub org browse.
- **Hardware validator baseline (as-of-April-2024).** Verified
  from `stellar-docs/validators/admin-guide/prerequisites.mdx`.
- **Catchup modes and mutual exclusion of `CATCHUP_COMPLETE` /
  `CATCHUP_RECENT`.** Verified from
  `stellar-docs/validators/admin-guide/running-node.mdx`.
- **Tier-1 recommended three-validator / three-archive pattern.**
  Verified from `stellar-docs/validators/tier-1-orgs.mdx`.
- **Protocol upgrade dates from P20 to P25.** Verified from
  `stellar-docs/networks/software-versions.mdx`.

If you (the reader) find any of the above wrong, that's a bigger
deal than anything listed in sections 1-10.

## 12. Summary

Phase 1 discovery has produced 25 audit docs that are **deep where
they go** — most of the code-verified claims are solid and would
survive a code review. But the discovery is **not complete**:

- ~10 source categories are still blank (CEX, FX, reference APIs,
  supply data, metadata resolution, API layer, infrastructure,
  Phoenix/Comet, SEP specs, stellar-etl).
- ~6 concrete facts were stated on secondary evidence and should be
  re-sourced from primary.
- ~4 mainnet addresses live in docs unverified.
- ~25 tests / benchmarks / fixtures are proposed but unexecuted.

Before Phase 2 coding starts, I want §10 items 1–11 closed. The rest
can close in parallel with Phase 2–3.

The one claim most at risk of being *wrong* (not just unverified)
is the "Galexie preserves native-epoch XDR" inference in
`protocol-versions.md`. Safe to pin with a single empirical test.
