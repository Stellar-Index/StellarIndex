---
title: Explorer — what each page serves, from where, and what is still open
last_verified: 2026-10-05
status: reference — the shipped explorer plus the open gap list
related:
  - docs/adr/0015-last-closed-bucket-rate-serving.md
  - docs/adr/0033-completeness-verification-model.md
  - docs/adr/0035-factory-anchored-contract-gating.md
  - docs/adr/0044-explorer-edge-rendering.md
---

# Explorer — data, IA and gaps

The explorer is `web/explorer/` (stellarindex.io, Cloudflare Pages). The API contract is `openapi/stellar-index.v1.yaml`; this page does not restate it. It records the intent, which entity page serves what from which endpoint or table, and the **gap list**: things the 2026-05 plan promised that the spec still does not contain.

Section numbers are cited from migrations and Go comments (§3, §6.1, §7, §9.1, §9.6, §11); keep them stable.

The plan's `/coins/*` tree never shipped: routes are `/assets/*` and there is no `/v1/coins` (see [supply-pipeline.md § Asset identity](supply-pipeline.md#asset-identity)); `/coins` and `/coins/` 301 to `/assets` in `web/explorer/public/_redirects`. Old `/v1/coins...` paths map to `/v1/assets...` or are gaps below.

## 1. Mission

Two goals, weighted equally. (1) Showcase the API: every visible number is one copy-pasteable public call, exposed by the `<>` reveal. (2) Be useful: holders, researchers, integrators and operators should each get real answers. A feature that serves only one goal is a v2 candidate.

Product shape (UX design of record, 2026-06): the index in the literal sense. Every entity has one canonical page, built from verified data, linking to the API call that produced it. Four lenses:

- **Prices**: what is anything worth (Stellar assets plus global crypto, fiat, RWA).
- **Protocols**: per-protocol deep dives backed by per-source tables and the ADR-0035 verified contract registries.
- **Network**: ledgers, transactions, accounts, contracts and events from the certified lake.
- **Coverage**: why to believe it (ADR-0033 verdicts, rendered as product).

Differentiators: protocol-aware attribution (`/contract` is the hinge, §7.10), provable completeness (coverage badges backed by `completeness_snapshots`) and independent cross-venue pricing (VWAP with confidence, oracle cross-checks).

## 2. Audiences

Audiences: traders and holders, DeFi users, builders, issuers and protocol teams, researchers and journalists, operators and auditors.

One site, no mode toggle: the answer is above the fold, deep data one click below.

## 3. Design principles

1. **Panels are API queries, 1:1.** No client-side joins; a UI gap becomes a gap-list entry. If a panel needs three things, add an endpoint that returns three things. Aggregations read per-protocol hourly rollup tables, never live OLAP against the lake.
2. **`<>` reveals the request** on every card (exact `curl` or `wscat`). Shipped as `web/explorer/src/components/reveal/Panel.tsx`.
3. **URL state is config.** Every selection lives in the query string so pages are reload-safe, shareable and deep-linkable (`useSearchParams` + `router.replace`).
4. **Anonymous-readable.** The free tier (anonymous floor 60/min, keyed tier 1000/min by default; config `anon_rate_limit_per_min`) covers browsing; only the dashboard needs sign-in.
5. **No mode toggle.** Same site for everyone.
6. **Desktop-first, dark default**; mobile gets search, asset pages and the pulse.
7. **Performance is a feature**: LCP < 1.5 s on 3G (p95), < 100 KB gzipped JS per route, API p95 < 200 ms, skeleton states, no third-party trackers. Not verified as achieved.
8. **Sortable by default**, sort reflected in the URL (`sort=<col>:asc|desc`).
9. **Open Graph card on every page** (see §14).
10. **Never render an in-progress bucket** (ADR-0015): live where it is real (SSE price tip, trade tape, ledger tip), everything else closed-bucket with an explicit timestamp. Confidence is always visible.

## 4. URL scheme

Shipped route tree: every `page.tsx` under `web/explorer/src/app/` (~80); the lens groups in §5 name the main entry points. Dynamic segments are keyed `(code, issuer)`, SAC, `native`, contract id, G-strkey, tx hash or ledger sequence.

Not shipped from the plan's route tree: `/anchors/{home_domain}`, `/path-payments`, `/pairs/{base}/{quote}` (the pair view is `/markets/[pair]`), `/oracles/{name}`, `/search` as a page (the endpoint exists), `/coverage` and `/coverage/{source}` as pages (data is `GET /v1/coverage`; the verdict table renders on `/diagnostics`).

The omnibox design routes by input shape: `G...` (56 chars) to the account, `C...` to the contract, 64-hex to the tx, integer to the ledger, asset code or name to asset results (verified first, then by class), `XLM/USD`-shaped to the pair, protocol name to `/protocols/{name}`, anything else to grouped full-text. Backed by `GET /v1/search`.

URL state parameters (plan; each is honoured only where the page implements it): `from`/`to` (ISO 8601), `granularity` (1m|15m|1h|4h|1d|1w|1mo), `timeframe` (1h|24h|7d|30d|1y|all), `sources` (csv), `compare` (csv), `sort`, `q`, `quote`, `tab`, `panel` (anchored sub-view, `#confidence-card`). `as_of_ledger` / `as_of` are NOT a point-in-time pin today (§8).

## 5. Site map by lens

- **Prices**: `/assets` (one faceted table: network, class, verified-only, venue coverage, 24h volume/change; Stellar assets and BTC/EUR/BENJI share one namespace), `/assets/[slug]`, `/markets/[pair]`, `/convert`, `/oracles`, `/divergences`, `/anomalies`.
- **Protocols**: `/protocols` directory, `/protocols/[name]`, instance pages (`/lending/[pool]`, `/liquidity-pools`, `/dexes/[source]`).
- **Network**: `/network`, `/ledgers/[seq]`, `/tx`, `/accounts/[g]`, `/contracts/[id]`.
- **Coverage**: `GET /v1/coverage` verdicts plus `docs/protocols/` verification pages rendered as product.

Home is mission control: omnibox, network pulse strip, price board (XLM, majors, movers, confidence-aware), protocol leaderboard, bridge net flow, trust strip (sources verified, coverage %, last audit run), recent blog.

Global patterns: API transparency on every panel; export CSV/JSON on every table and embed on every chart (partial; embeds exist); watchlist in localStorage (no account); consistent badges (verified asset, coverage tier, frozen/anomaly state, stale oracle).

**Coverage badge system**: green = verified complete (reconciled vs lake); blue = verified within window (retention-scoped, see AGENTS.md invariant 8); yellow = enumerated, pending verification; grey = best-effort (external venue data). Badges link to the per-source verdict.

## 6. Cross-cutting view primitives

Server-shaped data everywhere; no client transform beyond what the API returns, which keeps the reveal honest and forces API completeness.

| § | Primitive | Source | Status |
|---|---|---|---|
| 6.1 | Multi-window delta strip | `GET /v1/changes/{entity_type}/{id}`, backed by `change_summary_5m` (§9.6) | shipped |
| 6.3-6.6 | Direction pill, streak indicator, rank-change badge, acceleration arrow (first plus second derivative) | UI only, from the `change_summary_5m` columns | UI only |
| 6.8 | Confidence decomposition card | `confidence_factors` in `GET /v1/price` | shipped |
| 6.10 | `<>` reveal | `Panel.tsx` | shipped |

Gaps: sparkline (6.2), source contribution donut (6.7; rows exist in `price_source_contributions`, migration 0026, but no endpoint) and the TVL chart with event annotations (6.9).

## 7. Page inventory

Endpoints below are verified present in `openapi/stellar-index.v1.yaml`. Plan-era panels whose endpoint is absent are collected in §10.

### 7.1 Landing (`/`)
Pulse banner, trade tape (`GET /v1/observations/stream`), movers and volume leaders (`/v1/assets`, `/v1/markets`), protocol leaderboard (`/v1/protocols`), live anomaly banner (`/v1/anomalies`), network strip (`/v1/network/stats`, `/v1/ledger/tip`). Gaps: `/v1/diagnostics/pulse`, `/v1/tvl`, `/v1/tvl/flow` (Sankey), `/v1/network/volume`, `/v1/network/health`, wildcard `observations/stream?asset=*`, `/v1/discovered` (new SEP-41 listings), `/v1/contracts/wasm-upgrades`, sortable `delta_24h` on the asset list.

### 7.2-7.3 Assets (`/assets`, `/assets/[slug]`; plan: coins)
Directory and detail serve from `/v1/assets*` (metadata, supply, holders, verified), `/v1/price*`, `/v1/chart`, `/v1/ohlc`, `/v1/vwap`, `/v1/twap`, `/v1/history*` (`?source=` per-source overlay, on-chain venues only), `/v1/changes/{entity_type}/{id}`, `/v1/pairs`, `/v1/markets`, `/v1/sdex/orderbook`. Tabs: overview, chart, markets, history, supply, issuer (classic only), liquidity, oracles, on-chain, API. The volatility band is computed client-side from `/v1/ohlc` bars (INV-1087); per-source overlay is INV-1086. Detail pages are keyed `(code, issuer)`, SAC or `native`, never code alone; the bare-code slug belongs to a hand-vetted `internal/currency/data/seed.yaml` entry (§18 q4).
Asset-page slices under INV-2138: A1 (INV-2140) and A2 (INV-2141), movements and entry-changes views, open pending Ash's v1 call and the sizing brief (operation inventory: [classicmovements README](../../internal/sources/classicmovements/README.md)).

### 7.4 Pair (`/markets/[pair]`)
VWAP line, per-venue candles, live tape (`observations/stream?asset=&quote=`), VWAP/TWAP on a chosen window, triangulation path with bucket timestamp for indirect pairs (the convert engine, `/convert/{from}/{to}`). Why per-venue: one aggregated view is insufficient for researchers. Gaps: `/v1/pairs/{base}/{quote}/venues`, `/spread` (arbitrage signal), `/liquidity-flow`, a "routed via Soroswap" tape badge (§7.9.1).

### 7.5 Markets (`/markets`, `/dexes/[source]`, `/exchanges/[name]`)
`GET /v1/markets`, `/v1/markets/sources`, `/v1/pools`, `/v1/pools/reserves`. Gap: base-by-quote 24h-change heatmap (`/v1/markets/heatmap`).

### 7.6-7.7 Sources (`/sources`, `/sources/[name]`)
Health sits beside static metadata so weak sources are identified operationally, not only by exclusion. Serves `GET /v1/sources`, `/v1/sources/{name}/health`, `/v1/diagnostics/cursors`. Gaps: `GET /v1/sources/{name}` is mostly served by `/v1/sources/{name}/health`; only weight, paid and contracts remain, `/race` (publish-latency profile), `/reliability?window=30d`, `/weight-history`, per-source WASM history, per-source `/v1/diagnostics/decoders` rows (decode errors, orphans, unmatched hits).

### 7.8-7.9 Protocols (`/protocols`, `/protocols/[name]`)
Serves `GET /v1/protocols` (directory, KPIs, coverage tier), `/v1/protocols/{name}`, `/v1/protocols/{name}/tvl`, `/v1/lending/pools`, `/v1/lending/pools/{pool}/reserves`, `/v1/liquidity-pools`. Template: header (identity, verified factories as ADR-0035 trust roots, genesis ledger, coverage badge), KPIs (24h/7d volume, events/day, active instances, TVL where derivable), instances table, decoded-event tape, coverage tab. Signature panels per protocol (Blend liquidation terminal, pool reserves and swap flow, DeFindex vaults, CCTP/Rozo bridge flow, SDEX books, oracle boards, SEP-41 transfers).

### 7.9.1 Router and aggregator attribution
Decision: post-hoc additive tagging, not a separate dimension. `trades.routed_via` (nullable) carries the router name; the underlying venue's own volume is unchanged (Phoenix volume stays Phoenix volume) and `routed_via` is an extra grouping axis. Mechanism: a `routers` registry `(contract_id, name, kind, protocol_slug)`; the dispatcher's ContractCallDecoder fires on a router invocation and pushes the tag into per-tx context; every trade in that tx batch gets it (multi-hop shares one tag; for nested routers the outermost wins, `internal/pipeline/routedvia.go`). The registry and tag are live; `GET /v1/aggregators` serves the registry with a 24h routed-via rollup; the four per-protocol endpoints are gaps (§7.9).
Open: per-WASM-hash decoder audit for router WASM versions (gate backfill against an unaudited router WASM); DeFindex vault discovery is a curated allowlist at v1 (INV-1088, blocked: auto-discovery heuristic is a follow-up, candidates are contracts invoking two or more underlying-protocol contracts in single tx batches, promoted by an operator); arbitrary router nesting is unsupported until seen in the wild. The DeFindex exposure tracker (periodic per-vault on-chain state into `aggregator_exposures`, 1-minute cadence) was never built and the table was dropped (§9.9).

### 7.10 Contract (`/contracts/[id]`): the hinge
Every other page links here. Three states: protocol-attributed (in `protocol_contracts` or the known registry: "this is a Blend V2 pool, deployed by factory X at ledger N, verified" plus the embedded instance panel), recognised but unattributed (shape known to the recognition audit, raw decoded events), unknown (raw events and invocations from the lake). Converts the ADR-0035 gating work into visible product. Serves `GET /v1/contracts`, `/v1/contracts/{contract_id}`, `/wasm`, `/code-history`, `/interactions`, `/transfers`, `/v1/sac-wrappers`. WASM history ships over the ClickHouse lake (`internal/storage/clickhouse/wasm_lake_reader.go`), not Postgres tables (§9.1).

### 7.11-7.12 Oracles (`/oracles`)
Oracle terminal: all feeds by all oracles, freshness heatmap, divergence vs our VWAP (Reflector three contracts, RedStone, Band, Chainlink side by side). Serves `GET /v1/oracle/streams`, `/v1/oracle/latest`, `/v1/oracle/lastprice`, `/v1/oracle/prices`, `/v1/oracle/x_last_price` (SEP-40 compatible), `/v1/sources`. Gaps: `/v1/oracles` and `/v1/oracles/{name}` directory and detail, `/v1/oracles/compare?asset=`, per-oracle divergence-vs-VWAP chart.

### 7.13-7.14 Issuers and anchors (`/issuers`, `/issuers/[g_strkey]`)
Serves `GET /v1/issuers`, `/v1/issuers/{g_strkey}` from `issuers` and `classic_assets` (§9.7), SEP-1 payload from `internal/metadata`. Gaps: `/issuers/{g}/assets`, `/issuers/{g}/auth-history` (auth flags over time; only current flags are stored), and the whole anchor page `/anchors/{home_domain}` (`/v1/anchors/{domain}`, `/issuers`, `/assets`, `/flow?window=24h`; the `anchors` table was dropped, SEP-1 is served from `internal/metadata`).

### 7.15 Transaction (`/tx`, `/transactions/[hash]`)
`GET /v1/tx/{hash}` (header, per-op breakdown, status). Gaps: `/tx/{hash}/trades`, `/events`, `/changes` (LedgerEntry diff), path-payment route diagram.

### 7.16 Account (`/accounts/[g]`)
The account page is the lifetime authority on an account (programme INV-2128; coverage in [coverage-matrix.md](coverage-matrix.md)). Serves `GET /v1/accounts`, `/v1/accounts/{g}`, `/transactions`, `/operations`, `/movements`, `/positions` (cross-protocol DeFi positions: Blend, LP shares, vault shares), `/trades`, `/activity`, `/graph`, `/graph/history`, `/graph/cohort`, plus `/v1/accounts/stats|creators|sponsors`, `/v1/directory`. Pre-P23 classic movements come from the lake reconstruction of [ADR-0047](../adr/0047-pre-p23-classic-movement-reconstruction.md). `/v1/accounts/{g}` also serves the account's trustlines and offers inline, so a separate `/trustlines` is not needed.

### 7.17-7.19 Path payments, anomalies, divergences
- `/path-payments` (heatmap, recent, success rate): gap, no endpoint and no `path_payments` observer.
- `/anomalies`: `GET /v1/anomalies` (active freezes and timeline from `freeze_events`, §9.2).
- `/divergences`: `GET /v1/divergence`, `/v1/divergence/series` (from `divergence_observations`, §9.3; plan name `/v1/divergences...`).

### 7.20 MEV (`/mev`)
`GET /v1/mev` over `mev_events` (§9.5), worker `internal/aggregate/mev/`. Candidates, not verdicts. Rule (INV-1090, discarded as already honoured): never promote MEV to the home page or top of a page until the p95 false-positive rate is under 5%; today `/mev` is a separate page with link cards only.

### 7.21 Network (`/network`)
Serves `GET /v1/network/stats`, `/v1/network/throughput`, `/v1/ledger/tip`, `/v1/ledger/stream`, `/v1/ledgers`, `/v1/ledgers/{seq}` (+ `/transactions`, `/operations`), `/v1/operations`.
Honest phasing: the PG served tier holds the recent window, the CH lake everything to genesis. N1 = point lookups plus recent-window browsing; N2 = history-scale browse and filter (INV-1093, blocked, post-launch). A range not yet servable says so; never fake it.

### 7.22 Diagnostics (`/diagnostics`)
Public, no PII. Serves `GET /v1/diagnostics/cursors`, `/ingestion`, `/archive`, `/backups`, `/v1/coverage`, `/v1/status`, `/v1/incidents`. Gaps: per-source `/v1/coverage/{source}` and the extra `/v1/diagnostics/*` panels (§10); `/decoders` would read `decoder_stats_5m` (§9.4).

### 7.23 Research and blog (`/research`, `/blog`)
`/research` renders ADRs and architecture/operations docs; `/blog` renders `docs/blog/YYYY-MM-DD-<slug>.md` via `web/explorer/src/lib/blog.ts` and `src/lib/markdown.tsx` (plain Markdown, no MDX pipeline; decision, in-tree and no separate content repo). Publishing = git commit plus CI rebuild.

### 7.24 Customer account (`/dashboard/*`, `/signin`)
Not part of the public-data surface: keys, usage, settings, price alerts, webhooks, staff admin (`/v1/dashboard/*`, `/v1/account/*`, `/v1/auth/*`). Explorer sign-in is email code plus passkey (`web/explorer/src/lib/webauthn.ts`); there is no wallet sign-in in the explorer. SEP-10 (`/v1/auth/sep10/challenge`, `/token`) is a programmatic API auth path. Freighter/Albedo/Lobstr wallet sign-in is INV-1092 (blocked, post-v1). Usage is a live trailing-30-day count from the Redis-backed `usage.Counter`.

### 7.25 Search
`GET /v1/search`. Top-of-page categorised type-ahead (assets, issuers, anchors, contracts, tx, accounts, articles) and Cmd-K on every page. Ranking: exact match first, then trigram similarity, recency boost, popularity boost. Index design in §11.7; target p95 < 100 ms.

## 8. Time machine (not built)

`as_of_ledger` in responses is the lake watermark freshness stamp (ADR-0041), not a point-in-time query; no `timepin` helper exists in `internal/api/v1/`. If built: one `pinTime(ctx, asOfLedger)` helper every handler inherits (lint-enforced), `as_of` resolved to a ledger via `/v1/ledgers/at?ts=` (gap); point and range reads use the closed bucket containing ledger N, lists filter `first_seen_ledger <= N`. Why not now: it multiplies every handler's test surface, and users may confuse live with historical. Open post-v1 (§18 q10).

## 9. Schema

Created by migrations 0017-0026 from this plan. Migration 0152 dropped six tables whose writer was never built (`wasm_versions`, `contract_wasm_history`, `tvl_observations`, `anchors`, `classic_asset_stats_5m`, `aggregator_exposures`); its `.down.sql` carries the original DDL. Lesson: ship the table and its writer together, or not at all. A reader seeing a table in `\dt` assumes the capability exists.

### 9.1 `wasm_versions` + `contract_wasm_history` (dropped, 0152)
WASM history ships over the ClickHouse lake (`wasm_lake_reader.go`) rather than Postgres tables.

### 9.2 `freeze_events` (live, 0018)
Hypertable, 30-day chunks: one row per freeze with its `reason` and recovery stamps, written by the freeze `EventSink` on each clear/firing transition. Columns: migration 0018.

### 9.3 `divergence_observations` (live, 0019)
Hypertable, 7-day chunks: one row per comparison against a reference feed, written by `internal/divergence/worker.go`; the Redis firing flag alone loses the deltas a post-mortem needs. Columns: migration 0019.

### 9.4 `decoder_stats_5m` (live, 0020)
`bucket`, `source`, `events_seen`, `decode_errors`, `orphan_events`, `last_ledger`. 5-minute rollup flushed from `dispatcher.Stats()` (`internal/dispatcher/statsflush`). Hypertable, 7-day chunks.

### 9.5 `mev_events` (live, 0021); `tvl_observations` (dropped, 0152)
`mev_events` is written by `internal/aggregate/mev/` (§11.4); columns: migration 0021. `tvl_observations` had no writer and was dropped by 0152.

### 9.6 `change_summary_5m` (live, 0022)
`entity_type` (`coin` | `protocol` | `pair` | `source`), `entity_id`, `refreshed_at`, `current_value`, `h1_value`/`h1_delta_pct`, `h24_*`, `d7_*`, `d30_*`, `ath_value`/`ath_at`, `atl_value`/`atl_at`, `streak_direction`, `streak_days`, `acceleration` (`increasing` | `flat` | `decreasing`). Refreshed every 5 minutes by `internal/aggregate/changesummary`. Decision: one endpoint (`/v1/changes/{entity_type}/{id}`) powers every delta strip on the site. Cost is O(N) per refresh over about 10k assets, 10 protocols, 200 pairs, 30 sources, which is trivial.

### 9.7 `classic_assets`, `issuers` (live, 0023); `anchors` (dropped)
`classic_assets` holds one row per `(code, issuer)` with a UNIQUE `slug` (populated by migration 0134); `issuers` holds auth flags and the SEP-1 payload. Columns: migration 0023. `anchors` had no writer and was dropped by 0152.

### 9.8 `classic_asset_stats_5m` (dropped)
Always empty; the stats were moved onto a `prices_1m` UNION CTE (commit `2f06533a`).

### 9.9 `routers`, `trades.routed_via` (live, 0025); `aggregator_exposures` (dropped)
`routers`: `contract_id` PK, `name`, `kind` (`router` | `aggregator-vault`), `protocol_slug`, `added_at`, `auto_discovered`, `notes`; index `routers_protocol_idx`. `trades.routed_via` text, partial index `trades_routed_via_idx WHERE routed_via IS NOT NULL`. Seed at v1: SoroswapRouter (current and historical WASM versions) and DefindexFactory plus curated vaults from factory `new_vault` events. Backfill: walk trades and contract-call observations, tag same-tx trades. `aggregator_exposures` `(vault_contract_id, underlying_protocol, observed_at, observed_at_ledger, exposure_usd, detail)` was dropped with its never-built writer.

### 9.10 Other plan tables (0026)
`price_source_contributions` is live (written on every successful VWAP compute via `PriceSourceContributionSink`; per-source breakdown of each compute). `sdex_offer_events` has never had a writer (nothing in `internal/` or `cmd/` inserts into it), and the gap detector deliberately has no target for it (`internal/storage/timescale/per_source_gaps.go`); full offer lifecycle capture is the largest unbuilt ingest extension (spike and design note first). If a writer ships, add the gap-detector target with it.

## 10. API gap list (plan endpoints absent from the spec)

Renamed: coins to `/v1/assets*`; `/v1/orderbook` to `/v1/sdex/orderbook`; `/v1/divergences*` to `/v1/divergence*`; WASM history to `/v1/contracts/{id}/wasm`, `/code-history`; `/v1/oracles` to `/v1/oracle/streams`; `/v1/routers` to `/v1/aggregators`.

Not in the spec: the rest of the plan's endpoint families (TVL, volatility, spread, slippage, per-pair and per-source detail, protocol and contract drill-downs, issuers, path payments, anomalies, MEV, network, diagnostics extras, oracle compare, coin sub-resources). Each is a gap, not a decision to skip, unless §18 says so. Add none without a consumer; an absent endpoint's panel says so, never fakes data.

Streams: Last-Event-ID resume is implemented on the SSE streams. Embeds are frontend routes, not API endpoints.

## 11. Decoder and writer extensions

Status of the plan's workers:

- **11.1 Classic-asset registry**: `asset_registry.go` upserts `classic_assets` per `(code, issuer)` and an `issuers` row. SEP-1 fetcher wraps `internal/metadata` (outbound HTTPS is the only new egress dependency; per home domain at most 1 RPS, exponential backoff, weekly stale-cache acceptance, nightly plus on-demand for new issuers; target cache hit >= 90%). Issuer auth-flag change history is not tracked (§7.13).
- **11.2 TVL aggregation**: not built (§9.5).
- **11.3 Persistence wiring**: done. `internal/aggregate/freeze` sinks to `freeze_events`; `internal/divergence/worker.go` to `divergence_observations`; dispatcher stats to `decoder_stats_5m`; the orchestrator's `ContributionSink` to `price_source_contributions` (migration 0026).
- **11.4 MEV detection**: `internal/aggregate/mev/` writes `mev_events` (sandwich: opposite-direction brackets on the same pair within one ledger; algorithmic, no allowlist, no score). Oracle-deviation and liquidation-cascade detectors were planned (oracle updates vs `prices_1m`, Blend auction clusters); one pass for backfill then incremental. Gate in §7.20.
- **11.5 WASM history**: lake reader, not `internal/wasm/` (§9.1). WAT generation is an optional `wabt` exec (§7.10); results are immutable per hash, so cache by hash with an indefinite TTL.
- **11.6 Change-summary rollup**: `internal/aggregate/changesummary` (§9.6).
- **11.7 Search index**: tsvector plus GIN plus `pg_trgm` on tickers and names, ranked by trigram similarity and recency.
- **11.9 Router attribution**: `internal/pipeline/routedvia.go`, `internal/storage/timescale/routed_via.go` (§7.9.1). The router decoder reuses the ContractCallDecoder plumbing the Band decoder uses.

## 12. Forensic articles

Posts are in-tree Markdown (§7.23) built from embedded live-link primitives that open the exact frozen state, so an incident post-mortem is a set of deep links, not screenshots. Status incidents render at `/status/incident/[slug]` (`GET /v1/incidents`).

## 13. URL state, comparison and overlays

State lives in the URL: `/assets/<slug>?tab=chart&granularity=1m&timeframe=1h&sources=binance,kraken&compare=stellar,aqua&panel=confidence-card`. Shipped: per-source overlay (INV-1086) and volatility band (INV-1087); multi-asset `?compare=`, multi-protocol, quadrant and calendar overlays are gaps.

## 14. Embeds, Open Graph, integrations

- Iframe embeds on arbitrary domains: `/embed/asset/[slug]`, `/embed/currency/[ticker]`, `/embed/pair/[pair]`. `/embed/*` sends `frame-ancestors *`; only the `/*` rule omits `frame-ancestors` so non-embed pages are protected by `X-Frame-Options: DENY` (`web/explorer/public/_headers`).
- Open Graph: Satori via `workers-og` in the Pages Function `web/explorer/functions/og/[[path]].js`, linked through `ogImageFor` in `web/explorer/src/lib/seo.ts`; site-wide fallback is the static `/og.png`.
## 15. Stack and hosting (shipped)

Next.js 16 app router with RSC (`web/explorer/package.json`), TypeScript strict, Tailwind 4 with semantic tokens in `src/app/globals.css`, in-house `src/components/ui` (no shadcn/ui; reasoning in [design-system.md](design-system.md)), TradingView Lightweight Charts, TanStack Query v5, lucide-react, `openapi-typescript` generating `src/api/types.ts` from the spec (`make web-generate-api`; CI regenerates and fails on drift, which is the R8 mitigation).

Hosting: static export (`output: 'export'`) on Cloudflare Pages with Pages Functions for dynamic shells; an OpenNext edge-SSR build is selectable with `OPEN_NEXT=1` ([ADR-0044](../adr/0044-explorer-edge-rendering.md)). API origin `api.stellarindex.io`; `docs.stellarindex.io` serves the generated reference; the customer dashboard is in-site (`stellarindex.io/dashboard`; the standalone `app.` SPA was retired, see [cf-pages-setup.md](../operations/cf-pages-setup.md)). Dynamic long-tail routes (`/contracts/{id}`, `/tx/{hash}`, `/accounts/{g}`) render client-side via TanStack Query; the high-traffic set is pre-rendered via `generateStaticParams` for SEO.

Why a monorepo: one PR changes handler, spec and UI, and the generated client stays in lockstep.

## 16. Cross-cutting

- **Caching** (ADR-0018, [cdn-setup.md](../operations/cdn-setup.md)): closed-bucket VWAP, history, OHLC, since-inception: `public, max-age=60, s-maxage=300`; tip prices `public, max-age=1`; catalogues `public, max-age=60, s-maxage=300`; account, auth and OG: `private, no-store`; SSE `no-store`; immutable WASM bytecode `public, max-age=31536000, immutable`.
- **Rate limits**: anonymous 60/min, keyed 1000/min; embeds use the anonymous tier, so embedders should cache.
- **Auth**: only `/dashboard` and `/v1/account/*` require it (SEP-10 challenge to JWT, or API key).
- **Per-region consistency** (ADR-0015/0018): closed-bucket endpoints are identical across regions; tip and raw surfaces are explicitly per-region; the UI shows a freshness badge.
- **i18n and accessibility**: English only at v1 (URLs are i18n-ready); WCAG 2.1 AA target with colour-blind-safe palettes and keyboard navigation.

## 17. Risks still live

- **MEV false positives** damage credibility: published as unverified candidates, tuned per pattern, never promoted (§7.20).
- **DeFindex vault discovery** needs ongoing maintenance: curated allowlist (INV-1088).
- **SDEX offer lifecycle** is the largest unbuilt ingest extension (§9.10).
- **Frontend bundle creep**: the 100 KB per-route budget (§3) is not enforced by any CI step today.
- **Time-machine confusion** if ever built (§8).

## 18. Resolved questions of record

1. Hosting: Cloudflare Pages static export, edge SSR via ADR-0044.
2. Wallet UX: none at v1 (sign-in is email code plus passkey); Freighter/Albedo/Lobstr are INV-1092, post-v1.
3. Repo layout: monorepo, generated typed client. Content: in-tree Markdown. Brand: [design-system.md](design-system.md). Embeds: arbitrary domains.
4. Slug ownership: a bare code slug (`usdc`) is a hand-vetted `internal/currency/data/seed.yaml` entry, never a volume pick (impersonation vector, see AGENTS.md). Other classic assets get a per-`(code, issuer)` slug in `classic_assets.slug` (migration 0134, UNIQUE per 0023), used when the catalogue misses.
5. MEV thresholds: algorithmic, no allowlist, no score; unverified candidates (§11.4).
6. `as_of_ledger` UX: point-in-time mode not built, open post-v1 (§8).

Thresholds of record: WAT diff response capped at 5 MB; SDEX order book 20 levels per side; wildcard observations stream (`asset=*`) load-tested at 100 trades/s; Lighthouse mobile >= 90, CLS < 0.1, axe-core in CI. No CI step enforces the Lighthouse, axe-core or bundle gates today (§17).

UX decisions log (do not re-litigate): one unified asset namespace; canonical entity URLs with `/contract` as the attribution hinge; coverage badges backed by real verdicts; closed-bucket-only price rendering with explicit timestamps and visible confidence; one protocol-page template plus a per-protocol signature panel; no fake full-history network browsing before the CH read path exists; desktop-first, dark-default; `/research` stays as part of the trust story.

## 19. Open work (inventory)

| Item | State |
|---|---|
| INV-1088 DeFindex vault auto-discovery | blocked, follow-up after v1 curated allowlist (§7.9.1) |
| INV-1092 Albedo + Lobstr wallets | blocked, post-v1 (§7.24) |
| INV-1093 history-scale browse/filter (Phase N2) | blocked, post-launch (§7.21) |
| Asset-page slices INV-2140/INV-2141 (A1/A2 movements and entry-changes views, under INV-2138; account-page program is INV-2128) | open, see [classicmovements README](../../internal/sources/classicmovements/README.md) and ADR-0047 |
| API and panel gaps | §6, §7, §10 |

## 20. Cross-references

[openapi/stellar-index.v1.yaml](../../openapi/stellar-index.v1.yaml); ADR-0018 consistency surfaces; ADR-0015 closed-bucket rule; ADR-0020 chart contract (`../adr/0020-chart-api-contract.md`); ADR-0019 anomaly and freeze policy; ADR-0009 latency budget; [aggregation-plan.md](aggregation-plan.md); [coverage-matrix.md](coverage-matrix.md); [ingest-pipeline.md](ingest-pipeline.md); [overview.md](overview.md); [sla-proof-procedure.md](../operations/sla-proof-procedure.md).
