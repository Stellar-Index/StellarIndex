---
title: API ↔ explorer coverage — every endpoint, and whether a reader can reach it
last_verified: 2026-09-09
status: active
---

# API ↔ explorer coverage

Every path in `openapi/stellar-index.v1.yaml` — the contract, and the
authority for this document — against what `web/explorer` actually calls
and what a person can actually navigate to.

## Why this exists

`/v1/accounts/sponsors` and `/v1/accounts/creators` both return real data
(2.58 M sponsorships on the top account). Pages exist. They are in the
sitemap. And the operator still reported *"I don't see pages on the
explorer for the sponsors / creators"* — because the only way in is the
`/insights` hub. That pair turns out to be fine (the hub is in the rail
and cards both children, so it is level 3 below), but the report is the
right question asked of the whole surface, and the repo has been bitten
by the real version of it before: `components/nav/Footer.tsx` still
carries finding S-020, *"these pages were an orphaned island — they
linked only to each other; nothing linked in."*

So "exposed" is not one property. It is three, and they fail
independently.

## The three levels

| Level | Meaning |
|:-:|---|
| **1** | **Not consumed.** No explorer code calls the endpoint. A real coverage gap: the data is served and nothing renders it. |
| **2** | **Consumed but unreachable.** A page calls it, but no nav, footer or in-page link leads there. The sitemap does *not* count — a sitemap is for crawlers, not readers. |
| **3** | **Reachable.** A reader starting at `/` can get there by following links. |

A hub that is itself in the nav, with its children linked from it, is
level 3. The chain to `/` is what matters, not the hop count.

## Result

| | Count |
|---|---:|
| Paths in the OpenAPI contract | **127** |
| Level 3 — reachable | **102** |
| Level 2 — consumed but unreachable | **0** |
| Level 1 — not consumed | **21** |
| Deliberately excluded (operational) | **4** |

**Level 2 is empty, and that is enforced, not lucky.**
`src/lib/route-reachability.test.ts` already walks the link graph from
`/` and fails on any page without a click path. All 85 `page.tsx` routes
are reachable except nine that are exempt with a stated reason (iframe
widgets, legacy redirect shims, the magic-link landing, the design-system
reference). So there was no "just add a footer link" fix to make — the
gaps this audit found are all discoverability, not reachability.

**Network assessed: pubnet/mainnet**, where all five capability flags in
`src/lib/networks.ts` are true. `src/lib/network-routes.ts` hides
capability-gated routes per network, so a route absent on a test net is
correct behaviour, not a gap; the reachability walk is static and
therefore describes the mainnet shape.

## How to re-derive this

The guards are in the test suite — run these and they fail if any of the
above stops being true:

```
cd web/explorer && pnpm vitest run src/lib/route-reachability.test.ts src/app/crawl-surface.test.ts
```

`route-reachability.test.ts` owns level 2 (every page has a click path
from `/`). `crawl-surface.test.ts` owns discoverability (the sitemap and
the nav agree in both directions, and no shell page is indexable). Both
share one link-graph implementation, `src/lib/route-graph.ts`, so they
cannot drift into two different notions of "reachable".

Level 1 has no test — an endpoint the explorer does not call is a product
decision, not a defect. To re-derive the endpoint table:

```
# every path in the contract
grep -n '^  /' openapi/stellar-index.v1.yaml

# every endpoint the explorer references, comments stripped
cd web/explorer && grep -rno '/v1/[A-Za-z0-9/_.{}$-]*' \
  --include='*.ts' --include='*.tsx' src functions
```

Two traps make a naive grep wrong, and both bit the first pass of this
audit:

1. **`src/api/types.ts` is generated** from the spec and names every
   endpoint in its prose descriptions. It is never evidence of
   consumption. Exclude it.
2. **`src/api/account.ts` strips the `/v1` prefix** — `accountFetch`
   adds it — so the whole dashboard/auth surface is invisible to a
   `/v1/` grep. Match `accountFetch('/dashboard/keys')` too.

Comments must be stripped before matching. Prose names paths constantly,
and a comment is not a call site — the same reason the reachability walk
strips them before looking for links.

## The table

Level `ex` = deliberately excluded (see the end of this document).
"Consumed in" names the call site; a shared hook is shown as
`hooks.ts:useX`. "Reachable from `/`" lists the routes that render it —
`global nav chrome` means the rail, footer or search modal, which every
page carries.

| Endpoint | Methods | Level | Consumed in | Reachable from `/` via |
|---|---|:-:|---|---|
| `/healthz` | GET | ex | app/status/StatusPageClient.tsx | /status |
| `/readyz` | GET | ex | app/status/StatusPageClient.tsx | /status |
| `/livez/lake` | GET | ex | — | — |
| `/version` | GET | ex | — | — |
| `/status` | GET | 3 | hooks.ts:useStatus | /status |
| `/status/notices` | GET | 3 | app/status/StatusPageClient.tsx | /status |
| `/assets` | GET | 3 | app/HomeTopAssets.tsx, app/HomeTopMovers.tsx | /, /assets, /assets/[slug], /docs, /external/assets +2 |
| `/external/assets` | GET | 3 | app/external/assets/[slug]/page.tsx, app/external/assets/page.tsx | /external/assets, /external/assets/[slug] |
| `/external/assets/{slug}` | GET | 3 | app/HomeTryAPI.tsx, app/external/assets/[slug]/page.tsx | /, /external/assets/[slug] |
| `/assets/verified` | GET | 3 | app/HomeTryAPI.tsx, app/assets/AssetsTable.tsx | global nav chrome; /, /accounts, /accounts/[g] |
| `/assets/{asset_id}` | GET | 3 | app/HomeTryAPI.tsx, app/assets/[slug]/AssetClientFallback.tsx | /, /assets/[slug], /convert/[from]/[to], /docs, /network +1 |
| `/assets/{asset_id}/metadata` | GET | 1 | — | — |
| `/assets/{asset_id}/supply` | GET | 3 | hooks.ts:useAssetSupply | /assets/[slug] |
| `/assets/{asset_id}/holders` | GET | 3 | app/assets/[slug]/HoldersTabPanel.tsx | /assets/[slug] |
| `/price` | GET | 3 | ../functions/og/[[path]].js, app/HomeTryAPI.tsx | /, /accounts, /accounts/[g], /aggregators, /amm +71 |
| `/price/at` | GET | 1 | — | — |
| `/price/changes` | GET | 1 | — | — |
| `/price/tip` | GET | 3 | app/docs/page.tsx, app/methodology/page.tsx | /, /docs, /methodology, /sla, /status |
| `/price/tip/stream` | GET | 3 | app/docs/page.tsx, lib/live/hooks.ts | /, /accounts, /accounts/[g], /aggregators, /amm +71 |
| `/price/batch` | GET, POST | 3 | app/HomeCurrencies.tsx, app/accounts/AccountPositions.tsx | /, /accounts, /accounts/[g], /assets/[slug], /convert/[from]/[to] +2 |
| `/observations` | GET | 3 | app/status/StatusPageClient.tsx | /status |
| `/observations/stream` | GET | 3 | app/docs/page.tsx, lib/live/hooks.ts | /, /accounts, /accounts/[g], /aggregators, /amm +71 |
| `/price/stream` | GET | 3 | app/docs/page.tsx, app/page.tsx | /, /docs, /sdk, /status |
| `/history` | GET | 3 | app/HomeRecentTrades.tsx, app/HomeTryAPI.tsx | /, /assets/[slug], /docs, /markets/[pair], /status |
| `/history/since-inception` | GET | 1 | — | — |
| `/chart` | GET | 3 | app/assets/[slug]/ChartPanel.tsx, app/assets/[slug]/SupplyTabPanel.tsx | /, /assets/[slug], /convert/[from]/[to], /docs, /markets/[pair] +2 |
| `/ohlc` | GET | 3 | app/assets/[slug]/ChartPanel.tsx, app/docs/page.tsx | /, /assets/[slug], /dexes/[source], /docs, /exchanges/[name] +3 |
| `/vwap` | GET | 3 | app/docs/page.tsx, app/status/StatusPageClient.tsx | /docs, /status |
| `/twap` | GET | 3 | app/docs/page.tsx, app/status/StatusPageClient.tsx | /docs, /status |
| `/oracle/latest` | GET | 3 | app/assets/[slug]/AssetOraclesPanel.tsx, app/status/StatusPageClient.tsx | /assets/[slug], /status |
| `/pools` | GET | 3 | app/HomeTryAPI.tsx, app/assets/[slug]/LiquidityTabPanel.tsx | /, /assets/[slug], /dexes, /docs, /markets/[pair] +1 |
| `/pools/reserves` | GET | 3 | app/dexes/[source]/PairReservesPanel.tsx | /dexes/[source] |
| `/liquidity-pools` | GET | 3 | app/liquidity-pools/NativePoolsPanel.tsx | /liquidity-pools |
| `/lending/pools` | GET | 3 | app/HomeTryAPI.tsx, app/docs/page.tsx | /, /docs, /lending, /lending/[pool] |
| `/lending/pools/{pool}/reserves` | GET | 3 | app/lending/LendingPoolsTable.tsx, app/lending/[pool]/PoolReserves.tsx | /lending, /lending/[pool] |
| `/mev` | GET | 3 | app/docs/page.tsx, app/mev/MevFeed.tsx | /docs, /mev |
| `/anomalies` | GET | 3 | app/anomalies/AnomaliesFeed.tsx, app/anomalies/page.tsx | /anomalies, /docs |
| `/divergence` | GET | 3 | app/divergences/DivergenceFeed.tsx, app/docs/page.tsx | /divergences, /docs |
| `/divergence/series` | GET | 3 | app/divergences/DivergenceFeed.tsx | /divergences |
| `/oracle/streams` | GET | 3 | app/HomeTryAPI.tsx, app/assets/[slug]/AssetOraclesPanel.tsx | /, /assets/[slug], /docs, /oracles |
| `/markets` | GET | 3 | app/HomeTopMarkets.tsx, app/HomeTryAPI.tsx | /, /assets/[slug], /dexes/[source], /docs, /exchanges +5 |
| `/markets/sources` | GET | 3 | app/docs/page.tsx, app/markets/[pair]/SourceBreakdown.tsx | /assets/[slug], /docs, /markets/[pair] |
| `/issuers` | GET | 3 | app/docs/page.tsx, app/issuers/IssuersTable.tsx | /, /accounts, /assets/[slug], /dexes, /docs +4 |
| `/issuers/{g_strkey}` | GET | 3 | app/assets/[slug]/IssuerPanel.tsx, app/assets/[slug]/page.tsx | /accounts, /assets/[slug], /docs, /issuers, /issuers/[g_strkey] |
| `/contracts/{contract_id}/transfers` | GET | 3 | app/contract/ContractView.tsx | /contracts/[id] |
| `/changes/{entity_type}/{id}` | GET | 3 | hooks.ts:useChangeSummary, hooks.ts:useNetworkStats | /, /assets/[slug] |
| `/diagnostics/cursors` | GET | 3 | app/HomeLivePanels.tsx, app/HomeTryAPI.tsx | /, /diagnostics, /network, /sources, /sources/[name] |
| `/diagnostics/ingestion` | GET | 3 | app/status/StatusPageClient.tsx | /status |
| `/diagnostics/archive` | GET | 3 | app/diagnostics/ArchivePanel.tsx, app/diagnostics/page.tsx | /diagnostics |
| `/diagnostics/backups` | GET | 3 | hooks.ts:useBackupsDiagnostics | /status |
| `/incidents` | GET | 3 | app/HomeTryAPI.tsx, app/status/StatusPageClient.tsx | /, /status |
| `/incidents.atom` | GET | 3 | app/contact/page.tsx, app/methodology/page.tsx | /contact, /methodology, /status |
| `/coverage` | GET | 3 | app/diagnostics/CoveragePanel.tsx, app/diagnostics/page.tsx | /, /diagnostics, /sources |
| `/protocols` | GET | 3 | app/bridges/page.tsx, app/contracts/ContractsView.tsx | /bridges, /contracts, /dexes, /dexes/[source], /docs +5 |
| `/protocols/{name}` | GET | 3 | app/dexes/[source]/DexAnalyticsSection.tsx, app/dexes/[source]/SourceVolumeHistory.tsx | /dexes/[source], /docs, /protocols/[name], /sdex |
| `/protocols/{name}/tvl` | GET | 1 | — | — |
| `/sdex/orderbook` | GET | 3 | app/dexes/[source]/page.tsx, app/markets/[pair]/OrderBookPanel.tsx | /dexes/[source], /markets/[pair], /sdex |
| `/ledger/tip` | GET | 3 | components/nav/NetworkSwitcher.tsx | global nav chrome; /, /accounts, /accounts/[g] |
| `/ledger/stream` | GET | 3 | app/docs/page.tsx, lib/live/hooks.ts | /, /accounts, /accounts/[g], /aggregators, /amm +71 |
| `/network/stats` | GET | 3 | app/HomeLivePanels.tsx, app/HomeTryAPI.tsx | /, /dashboard, /docs, /network, /status |
| `/network/throughput` | GET | 3 | app/diagnostics/IngestThroughputChart.tsx, app/docs/page.tsx | /diagnostics, /docs, /ledgers, /network, /operations +1 |
| `/methodology` | GET | 1 | — | — |
| `/sources` | GET | 3 | app/HomeTryAPI.tsx, app/aggregators/ReferencePriceAggregators.tsx | /, /aggregators, /assets/[slug], /bridges, /dexes +10 |
| `/sources/{name}/health` | GET | 3 | app/sources/[name]/SourceHealthPanel.tsx | /sources/[name] |
| `/aggregators` | GET | 3 | app/aggregators/RoutedVolumePanel.tsx | /aggregators |
| `/sac-wrappers` | GET | 3 | hooks.ts:useSACWrappers | /accounts, /anomalies, /assets/[slug], /contracts, /dexes +6 |
| `/rwa/assets` | GET | 3 | app/rwa/RWAView.tsx | /rwa |
| `/pairs` | GET | 1 | — | — |
| `/oracle/lastprice` | GET | 3 | app/oracles/OraclesView.tsx, app/status/StatusPageClient.tsx | /oracles, /status |
| `/oracle/prices` | GET | 3 | app/oracles/OraclesView.tsx | /oracles |
| `/oracle/x_last_price` | GET | 3 | app/oracles/OraclesView.tsx | /oracles |
| `/account/me` | GET | 3 | hooks.ts:useMe | /dashboard |
| `/account/usage` | GET | 3 | account.ts:fetchUsage | /dashboard/usage |
| `/account/keys` | GET, POST | 1 | — | — |
| `/account/keys/{keyID}` | DELETE | 1 | — | — |
| `/account/admin/lookup` | POST | 3 | account.ts:adminLookup | /dashboard/admin |
| `/admin/keys` | POST | 1 | — | — |
| `/admin/keys/{keyID}` | DELETE | 1 | — | — |
| `/admin/accounts/{id}` | GET, PATCH | 1 | — | — |
| `/admin/status-notices` | GET, POST | 1 | — | — |
| `/admin/status-notices/{id}/resolve` | POST | 1 | — | — |
| `/register` | POST | 3 | app/pricing/page.tsx, app/signup/page.tsx | /pricing, /signup |
| `/signup` | POST | 1 | — | — |
| `/signup/verify` | GET | 1 | — | — |
| `/dashboard/keys` | GET, POST | 3 | account.ts:createKey, account.ts:listKeys | /dashboard, /dashboard/keys, /dashboard/usage |
| `/dashboard/keys/{id}` | DELETE | 3 | account.ts:revokeKey | /dashboard/keys |
| `/dashboard/webhooks` | GET, POST | 1 | — | — |
| `/dashboard/webhooks/{id}` | PATCH, DELETE | 1 | — | — |
| `/dashboard/webhooks/{id}/deliveries` | GET | 1 | — | — |
| `/dashboard/price-alerts` | GET, POST | 3 | account.ts:createPriceAlert, account.ts:listPriceAlerts | /dashboard/price-alerts |
| `/dashboard/price-alerts/{id}` | PATCH, DELETE | 3 | account.ts:deletePriceAlert, account.ts:updatePriceAlert | /dashboard/price-alerts |
| `/auth/login` | POST | 3 | app/signin/SignInForm.tsx, app/status/StatusPageClient.tsx | /signin, /signup, /status |
| `/auth/callback` | GET | 3 | app/auth/callback/CallbackHandler.tsx, app/status/StatusPageClient.tsx | /status |
| `/auth/verify-code` | POST | 3 | account.ts:verifyCode | /signin |
| `/auth/logout` | POST | 3 | account.ts:logout, components/nav/Sidebar.tsx | global nav chrome; /, /accounts, /accounts/[g] |
| `/auth/passkey/begin-login` | POST | 3 | account.ts:beginPasskeyLogin | /signin |
| `/auth/passkey/finish-login` | POST | 3 | account.ts:finishPasskeyLogin | /signin |
| `/auth/passkey/begin-register` | POST | 3 | account.ts:beginPasskeyRegister | /dashboard/settings |
| `/auth/passkey/finish-register` | POST | 3 | account.ts:finishPasskeyRegister | /dashboard/settings |
| `/auth/passkey/credentials` | GET | 3 | account.ts:listPasskeys | /dashboard/settings |
| `/auth/passkey/credentials/{id}` | DELETE | 3 | account.ts:deletePasskey | /dashboard/settings |
| `/auth/sep10/challenge` | GET | 3 | app/status/StatusPageClient.tsx | /status |
| `/auth/sep10/token` | POST | 1 | — | — |
| `/ledgers` | GET | 3 | app/ledgers/LedgersTable.tsx, app/network/NetworkView.tsx | /ledgers, /network, /transactions |
| `/ledgers/{seq}` | GET | 3 | app/ledger/LedgerView.tsx | /ledgers/[seq] |
| `/ledgers/{seq}/transactions` | GET | 3 | app/ledger/LedgerView.tsx, app/transactions/TransactionsView.tsx | /ledgers/[seq], /transactions |
| `/tx/{hash}` | GET | 3 | app/operation/OperationView.tsx, app/tx/TxView.tsx | /operation, /transactions/[hash] |
| `/operations` | GET | 3 | app/operations/OperationsView.tsx, components/NetworkInsight.tsx | /ledgers, /network, /operations, /transactions |
| `/contracts` | GET | 3 | app/contracts/ContractsView.tsx, app/contracts/page.tsx | /contracts |
| `/contracts/{contract_id}` | GET | 3 | app/contract/ContractView.tsx | /contracts/[id] |
| `/contracts/{contract_id}/wasm` | GET | 3 | app/contract/ContractView.tsx | /contracts/[id] |
| `/contracts/{contract_id}/interactions` | GET | 3 | app/contract/ContractView.tsx | /contracts/[id] |
| `/contracts/{contract_id}/code-history` | GET | 3 | app/contract/ContractView.tsx | /contracts/[id] |
| `/accounts` | GET | 3 | app/accounts/AccountView.tsx | /accounts, /accounts/[g] |
| `/directory` | GET | 1 | — | — |
| `/accounts/stats` | GET | 3 | app/accounts/AccountsAnalytics.tsx | /accounts, /accounts/[g] |
| `/accounts/creators` | GET | 3 | app/insights/creators/CreatorBoard.tsx | /insights/creators |
| `/accounts/sponsors` | GET | 3 | app/insights/sponsors/SponsorBoard.tsx | /insights/sponsors |
| `/accounts/{g_strkey}` | GET | 3 | app/accounts/AccountActivitySummary.tsx, app/accounts/AccountDefiPositions.tsx | /accounts, /accounts/[g] |
| `/accounts/{g_strkey}/transactions` | GET | 3 | app/accounts/AccountView.tsx | /accounts, /accounts/[g] |
| `/accounts/{g_strkey}/operations` | GET | 3 | app/accounts/AccountView.tsx | /accounts, /accounts/[g] |
| `/accounts/{g_strkey}/movements` | GET | 3 | app/accounts/AccountMovements.tsx | /accounts, /accounts/[g] |
| `/accounts/{g_strkey}/positions` | GET | 3 | app/accounts/AccountDefiPositions.tsx | /accounts, /accounts/[g] |
| `/accounts/{g_strkey}/trades` | GET | 3 | app/accounts/AccountTrades.tsx | /accounts, /accounts/[g] |
| `/accounts/{g_strkey}/activity` | GET | 3 | app/accounts/AccountActivitySummary.tsx | /accounts, /accounts/[g] |
| `/accounts/{g_strkey}/graph` | GET | 3 | app/accounts/AccountGraph.tsx | /accounts, /accounts/[g] |
| `/search` | GET | 3 | components/nav/SearchModal.tsx | global nav chrome; /, /accounts, /accounts/[g] |

## Level 1 — the 21 stranded endpoints

Every one of these was probed live on 2026-09-09. **All 21 exist and
answer** — none 404s at the route level. This is served data with no
reader.

Building pages for them is a product decision and is deliberately not
made here. What follows is what is stranded and roughly what it would
take.

### Public data with no surface (7)

| Endpoint | What is stranded | Rough cost |
|---|---|---|
| `/price/changes` | Multi-horizon deltas for any asset — 1h / 24h / 7d / 30d, each with `reference_at` + `resolution`, in one request. Live: XLM returned all four horizons populated. The explorer today recomputes a 24h change per surface. | Small. It is a strictly better source for a strip the asset pages already render — a swap, not a new page. |
| `/price/at` | Point-in-time price at any timestamp: the cost-basis / PnL / tax lookup. Resolves to the finest CAGG bar covering the instant, back to 2018. | Small as an input to an existing page; a real feature as a date-picker UI. |
| `/history/since-inception` | Full-history series. XLM returned **3 341 daily points, 2017-01-17 → 2026-09-08**, with the 2017-08 → 2018-02 gap explicitly flagged (`discontinuous`). The charts today are bounded windows, so the whole "since inception" view is unreachable. | Medium — a range control on the existing asset chart. Note the param is `asset`, **not** `base`. |
| `/assets/{asset_id}/metadata` | The full SEP-1 CURRENCIES block per asset: `sep1_status`, `home_domain`, and the issuer's declared metadata. Richer than what asset pages render now. | Small — a panel on `/assets/[slug]`. |
| `/protocols/{name}/tvl` | Per-protocol TVL with per-leg reserves and pricing basis. Live: soroswap returned 125 pools, $1.25 M TVL, **11 priced / 114 unpriced**. That priced-vs-unpriced split is a real completeness signal nothing surfaces. | Small — `/protocols/[name]` already exists. Returns a typed 404 (`protocol-tvl-not-derived`) for lending protocols like blend; handle that, don't treat it as an error. |
| `/pairs` | Per-pair trade stats (`trade_count_24h`, `volume_24h_usd`) for an explicit base/quote. Both params required. | Small — overlaps what `/markets` already gives; likely redundant rather than missing. |
| `/directory` | Curated address labels with tags and provenance (`source: stellar-expert`). `src/components/DirectoryLabel.tsx` exists and renders the `directory` field that comes back *embedded in other responses* — but the standalone bulk endpoint is never called. | Small. The rendering component is already built. |

### `/methodology` — the one with a page that ignores it

`/v1/methodology` returns a live ~6.9 KB machine-readable document: 29
sources, 4 source classes, 6 ADR references, the stablecoin proxy list,
`version: "1.0"`. The explorer's `/methodology` page is **hand-written
prose that does not call it.** So the page and the endpoint can disagree
about how the index works and nothing notices. Worth wiring — it is the
kind of drift that is invisible until it is embarrassing.

### Account/admin surfaces with no UI (13)

Consistent gaps, all behind auth, all returning a correct `401` when
probed unauthenticated:

- **Webhooks (3)** — `/dashboard/webhooks`, `/dashboard/webhooks/{id}`,
  `/dashboard/webhooks/{id}/deliveries`. There is **no webhooks UI at
  all**, yet `/dashboard/price-alerts` exists and a firing alert enqueues
  a `price.alert` webhook. So a user can create an alert whose delivery
  mechanism they cannot see, configure, or debug. This is the largest and
  most user-visible gap in the list.
- **Staff admin (5)** — `/admin/keys`, `/admin/keys/{keyID}`,
  `/admin/accounts/{id}`, `/admin/status-notices`,
  `/admin/status-notices/{id}/resolve`. `/dashboard/admin` exists and
  uses `/account/admin/lookup` only, so staff can look an account up but
  not act on it; status notices are posted out-of-band.
- **Legacy key surface (2)** — `/account/keys`, `/account/keys/{keyID}`.
  Superseded by `/dashboard/keys` (the richer Postgres-backed store the
  UI uses). Probably wants deprecating rather than building.
- **Signup (2)** — `POST /signup`, `/signup/verify`. The UI uses the
  `/auth/login` magic-link flow instead. Dead path, or an unshipped one.
- **SEP-10 (1)** — `POST /auth/sep10/token`. See the disagreement note
  below: SEP-10 is not wired on this deployment.

## Discoverability

Reachability answers "can a reader get there from `/`". It does not
answer "can anyone find the site in the first place". Assessed
separately, and this is where the actual defects were.

### What was wrong, and is now fixed

**Two hub pages were missing from `sitemap.ts`.** Both are linked from
the nav, indexable and canonical-tagged — they were simply never added:

- `/bridges` — the newest member of the category-hub family
  (`/dexes`, `/lending`, `/amm`, `/yield`, …), every other member of
  which is listed.
- `/external/assets` — the rail's "External → Assets" entry. Its
  per-currency **children** were sitemapped while the hub that indexes
  them was not.

This is the same omission the file already records for the seven
chain-explorer hubs, made again by the two newest pages.

**Four query-param entity shells were indexable.** `/contract?id=`,
`/ledger?seq=`, `/tx?hash=` and `/operation?tx=&i=` render *entirely*
from their query string, and each tags itself `canonical: '/<route>'`.
So the only URL a crawler can construct — and the one every
parameterised hit is consolidated onto — is the bare path, which renders
an empty shell. That is a soft-404 of exactly the class
`crawl-surface.test.ts` was written to catch for the `/assets/shell` and
`/markets/shell` sentinels.

The first three exist *specifically to catch inbound legacy links*, so
they are the most likely of all these pages to actually be crawled. Every
canonical counterpart — `/contracts/[id]`, `/ledgers/[seq]`,
`/transactions/[hash]`, `/accounts/[g]` — already carried
`robots: { index: false, follow: true }`. These four did not. They do
now, `follow: true` throughout so outbound links keep flowing.

### What was already right

- **`robots.ts`** disallows `/dev/`, `/embed/`, `/auth/`, `/dashboard`,
  `/signin`, `/signup` — all correct, nothing valuable blocked. Origin
  and sitemap URL are per-network, so a test-net build does not point
  crawlers at mainnet.
- **Titles and descriptions: complete.** All 82 content pages supply
  both, via `metadata`, `generateMetadata`, or an inherited layout. No
  duplicates among indexable pages.
- **Canonicals: complete** on every indexable page.
- **JSON-LD** appears on `/assets/[slug]` and `/markets/[pair]`, both
  through `serializeJsonLd`. No hand-rolled `application/ld+json`
  anywhere — the stored-XSS rule holds.
- The sitemap already filters through `routeAvailable`, so a test net
  does not submit pages that are structurally empty there.

### Per-page table

`n/a` in the sitemap column means the page is `noindex`, where absence
from the sitemap is correct (a noindex URL in a sitemap is a Search
Console error). 24 pages are noindex: the auth and dashboard surfaces,
the iframe widgets, the design-system reference, and the unbounded
per-entity shells.

| Route | Title | Desc | Canonical | Indexable | In sitemap |
|---|:-:|:-:|:-:|:-:|:-:|
| `/` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/accounts` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/accounts/[g]` | ✓ | ✓ | — | — | n/a |
| `/aggregators` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/amm` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/anomalies` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/assets` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/assets/[slug]` | ✓ | ✓ | ✓ | — | ✓ |
| `/auth/callback` | ✓ | ✓ | ✓ | — | n/a |
| `/blog` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/blog/[slug]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/bridges` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/careers` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/changelog` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/company` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/contact` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/contract` | ✓ | ✓ | ✓ | — | n/a |
| `/contracts` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/contracts/[id]` | ✓ | ✓ | — | — | n/a |
| `/convert/[from]/[to]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/dashboard` | ✓ | ✓ | ✓ | — | n/a |
| `/dashboard/admin` | ✓ | ✓ | ✓ | — | n/a |
| `/dashboard/keys` | ✓ | ✓ | ✓ | — | n/a |
| `/dashboard/price-alerts` | ✓ | ✓ | ✓ | — | n/a |
| `/dashboard/settings` | ✓ | ✓ | ✓ | — | n/a |
| `/dashboard/usage` | ✓ | ✓ | ✓ | — | n/a |
| `/dev/primitives` | ✓ | ✓ | ✓ | — | n/a |
| `/dev/styleguide` | ✓ | ✓ | ✓ | — | n/a |
| `/dexes` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/dexes/[source]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/diagnostics` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/divergences` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/docs` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/embed/asset/[slug]` | ✓ | ✓ | ✓ | — | n/a |
| `/embed/currency/[ticker]` | ✓ | ✓ | — | — | n/a |
| `/embed/pair/[pair]` | ✓ | ✓ | — | — | n/a |
| `/exchanges` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/exchanges/[name]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/external/assets` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/external/assets/[slug]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/insights` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/insights/creators` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/insights/sponsors` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/issuers` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/issuers/[g_strkey]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/ledger` | ✓ | ✓ | ✓ | — | n/a |
| `/ledgers` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/ledgers/[seq]` | ✓ | ✓ | — | — | n/a |
| `/lending` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/lending/[pool]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/liquidity-pools` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/markets` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/markets/[pair]` | ✓ | ✓ | ✓ | — | ✓ |
| `/methodology` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/mev` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/network` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/network/ledgers` | _redirect shim_ | | | | |
| `/network/operations` | _redirect shim_ | | | | |
| `/operation` | ✓ | ✓ | ✓ | — | n/a |
| `/operations` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/oracles` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/pricing` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/protocols` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/protocols/[name]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/research` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/research/adr/[id]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/research/architecture` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/research/architecture/[slug]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/research/operations` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/research/operations/[slug]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/rwa` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/sdex` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/sdk` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/signin` | ✓ | ✓ | — | — | n/a |
| `/signup` | ✓ | ✓ | — | — | n/a |
| `/sla` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/sources` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/sources/[name]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/status` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/status/incident/[slug]` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/transactions` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/transactions/[hash]` | ✓ | ✓ | — | — | n/a |
| `/tx` | ✓ | ✓ | ✓ | — | n/a |
| `/widgets` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `/yield` | ✓ | ✓ | ✓ | ✓ | ✓ |

## Deliberately excluded

Listed rather than silently dropped, so a future reader can tell absence
from oversight.

| Surface | Why excluded |
|---|---|
| `/v1/healthz`, `/v1/readyz` | Liveness/readiness probes. Answer to monitoring, not to a reader. (Both *are* in fact rendered on `/status`, which is why they are marked `ex` and not level 1.) |
| `/v1/livez/lake` | Lake-critical probe. Not consumed by the explorer; live and green when probed. Monitoring surface. |
| `/v1/version` | Build identity (`v0.65.0`, commit, Go version). Operational. The explorer shows its own build SHA in the footer instead. |
| `/embed/*` routes | Iframe widget endpoints, chrome-less by design. Reached via `src=` on an `<iframe>` from `/widgets`, never an `<a href>`, so they are exempt from the reachability walk and `noindex` by design. |
| `/metrics`-style endpoints | Not in the v1 contract; Prometheus scrape surface. |

## Where the spec and the running API disagree

Probed read-only against `https://api.stellarindex.io` on 2026-09-09.
Nothing here is a contract violation — every documented route exists and
every status code observed is one the spec documents. These are drift and
fitness issues.

1. **The published spec is 8 minor versions stale.** The explorer serves
   the contract at `stellarindex.io/openapi/stellar-index.v1.yaml` (the
   link on `/docs`), copied at build time by the `prebuild` script. The
   published copy is `info.version 1.20.0`; the repo is `1.28.0`. It is
   missing four paths that exist locally **and are live on the API** —
   `/protocols/{name}/tvl`, `/accounts/creators`, `/accounts/sponsors`,
   `/rwa/assets`. Drift is one-directional (nothing published is absent
   locally), so this is a stale deploy artifact, not a contract
   disagreement. Note that `/accounts/creators` and `/accounts/sponsors`
   are the very endpoints that prompted this audit: they are undocumented
   to anyone reading the published spec. **A rebuild of the explorer
   fixes it.**

2. **SEP-10 is not wired on this deployment.**
   `GET /v1/auth/sep10/challenge` validates parameters (missing `account`
   → 400) but returns **503 `sep10-unavailable`** for a valid G-strkey:
   *"no SEP-10 validator wired — typically because the server signing
   seed isn't configured."* The spec documents the 503, so this agrees
   formally. But `/v1/account/keys`'s own 401 detail says it "requires an
   API key or SEP-10 token", and the SEP-10 half of that sentence
   currently cannot be satisfied. `/sdk` documents the flow as available.

   **Resolved in documentation 2026-09-09** (the deployment is
   unchanged — enabling SEP-10 remains a product decision). SEP-10 is
   implemented, not missing: the 503 is the `NoopSEP10Validator`
   fallback taken when `STELLARINDEX_SEP10_SEED` /
   `STELLARINDEX_SEP10_JWT_SECRET` are unset and `auth_mode` is not
   `sep10`. Every doc that promised the flow now says it is unavailable
   here and names that configuration. A second, sharper claim was found
   and corrected alongside it: the OpenAPI security scheme and
   `pkg/client` both said the bearer header accepts API keys **and**
   SEP-10 JWTs. `middleware.authenticate` is a mutually exclusive switch
   over the four `auth_mode` values, so no deployment accepts both —
   enabling SEP-10 turns `sip_*` keys off.

3. **Timezone leak — larger than two endpoints.** `/v1/price/at` and
   `/v1/history/since-inception` emit timestamps with a local UTC offset
   (`2026-09-07T11:00:00+02:00`, `2017-01-17T01:00:00+01:00`) where every
   other endpoint probed — and every spec example — uses `Z`. Valid
   RFC 3339 either way, so no schema violation, but the server's local
   timezone is leaking into those two series and a client doing a naive
   string-prefix comparison would mis-bucket them. Both are level-1
   endpoints, so nothing in the explorer is affected today. Whether this
   is deliberate could not be determined from outside.

   **Fixed 2026-09-09**, and it was not deliberate. A re-probe of every
   reachable GET found **fourteen** leaking endpoints, not two: also
   `/v1/price`, `/v1/history`, `/v1/chart`, `/v1/observations`,
   `/v1/lending/pools`, `/v1/coverage`, `/v1/diagnostics/ingestion` and
   all four `/v1/oracle/*` surfaces. The class is any json-tagged
   `time.Time` whose value came from Postgres, since `timestamptz`
   decodes into the process's local zone; the endpoints that looked
   correct were the ones stamping `time.Now().UTC()` themselves. All 43
   such fields now use a `WireTime` type that renders UTC unconditionally,
   and two tests hold the line — one scans rendered payloads across
   twelve endpoints, one fails the build on a new json-tagged
   `time.Time` anywhere in the package.

4. **Stale illustrative figure in the spec's own prose.** The
   `/history/since-inception` description states a `1d` request measured
   2026-09-07 returned 2 183 points. The live API returns **3 341**, and
   reaches back to 2017 rather than 2021. The route is fine; the
   measurement quoted in the doc is out of date.

   **Fixed 2026-09-09** in the spec and in the handler comment carrying
   the same measurement. Re-measured live: 3 341 points, 2017-01-17 →
   2026-09-08. The `granularity=1m` half of the claim (50 000 points
   ending 2018-02-21) was re-checked and still holds exactly.

5. **No spec served from the API host.** `/v1/openapi.json`,
   `/openapi.json`, `/v1/openapi.yaml`, `/v1/spec` and `/v1/docs` all
   404. The contract is published only from the explorer origin. Not a
   defect — just worth knowing, since it is why item 1 is possible.

## Changes made under this audit

- `sitemap.ts` — added `/bridges` and `/external/assets`.
- `contract/`, `ledger/`, `tx/`, `operation/page.tsx` — added
  `robots: { index: false, follow: true }`.
- `src/lib/route-graph.ts` — new. The link-graph walk, extracted from
  `route-reachability.test.ts` so the sitemap guard shares one
  implementation. Test-only, like `lib/nav-shell`.
- `crawl-surface.test.ts` — four new assertions: the sitemap submits
  nothing unreachable from the nav; every entry names a real page; every
  page the nav offers is submitted; the four query-param shells stay out
  of the index.
