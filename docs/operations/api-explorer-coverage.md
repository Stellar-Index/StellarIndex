---
title: API ↔ explorer coverage — every endpoint, and whether a reader can reach it
last_verified: 2026-09-24
status: active
---

# API ↔ explorer coverage

Every path in `openapi/stellar-index.v1.yaml` (the authority) against what
`web/explorer` calls and what a reader can navigate to. Network assessed:
pubnet, where all five capability flags in `src/lib/networks.ts` are true;
`src/lib/network-routes.ts` hides capability-gated routes per network, so a
route absent on a test net is correct, not a gap.

## The three levels

| Level | Meaning |
|:-:|---|
| **1** | **Not consumed.** No explorer code calls the endpoint: data served, nothing renders it. |
| **2** | **Consumed but unreachable.** A page calls it, but no nav, footer or in-page link leads there. The sitemap does not count. |
| **3** | **Reachable.** A reader starting at `/` can get there by following links (a hub in the nav with its children linked from it is level 3). |

## Result

| | Count |
|---|---:|
| Paths in the OpenAPI contract | **140** |
| Level 3 — reachable | **111** |
| Level 2 — consumed but unreachable | **0** |
| Level 1 — not consumed | **25** |
| Deliberately excluded (operational) | **4** |

Level 2 is empty and enforced: `src/lib/route-reachability.test.ts` walks the
link graph from `/` and fails on any page without a click path. All 88
`page.tsx` routes are reachable except nine exempt with a stated reason
(iframe widgets, legacy redirect shims, the magic-link landing, the
design-system reference). Counts and both guard tests were last re-derived
2026-09-24; the live probes below were not re-run since 2026-09-09.

## How to re-derive this

```
cd web/explorer && pnpm vitest run src/lib/route-reachability.test.ts src/app/crawl-surface.test.ts
cd web/explorer && pnpm vitest run src/lib/api-explorer-coverage-doc.test.ts
```

`route-reachability.test.ts` owns level 2. `crawl-surface.test.ts` owns
discoverability (sitemap and nav agree both ways; no shell page is
indexable). Both share `src/lib/route-graph.ts`. `api-explorer-coverage-doc.test.ts`
pins the table and Result counts to the spec. Level 1 has no test: an
endpoint the explorer does not call is a product decision, not a defect.

To re-derive the endpoint table:

```
# every path in the contract
grep -n '^  /' openapi/stellar-index.v1.yaml

# every endpoint the explorer references, comments stripped
cd web/explorer && grep -rno '/v1/[A-Za-z0-9/_.{}$-]*' \
  --include='*.ts' --include='*.tsx' src functions
```

Two traps: `src/api/types.ts` is generated from the spec and names every
endpoint in prose, so exclude it; `src/api/account.ts` strips the `/v1`
prefix (`accountFetch` adds it), so also match `accountFetch('/dashboard/keys')`.
Strip comments before matching; a comment is not a call site.

## The table

Level `ex` = deliberately excluded (last section). "Consumed in" is the first
call site; `hooks.ts:useX` is a shared hook.

| Endpoint | Methods | Level | Consumed in |
|---|---|:-:|---|
| `/healthz` | GET | ex | app/status/StatusPageClient.tsx |
| `/readyz` | GET | ex | app/status/StatusPageClient.tsx |
| `/livez/lake` | GET | ex | — |
| `/version` | GET | ex | — |
| `/status` | GET | 3 | hooks.ts:useStatus |
| `/status/notices` | GET | 3 | app/status/StatusPageClient.tsx |
| `/assets` | GET | 3 | app/HomeTopAssets.tsx |
| `/external/assets` | GET | 3 | app/external/assets/[slug]/page.tsx |
| `/external/assets/{slug}` | GET | 3 | app/HomeTryAPI.tsx |
| `/assets/verified` | GET | 3 | app/HomeTryAPI.tsx |
| `/assets/{asset_id}` | GET | 3 | app/HomeTryAPI.tsx |
| `/assets/{asset_id}/metadata` | GET | 1 | — |
| `/assets/{asset_id}/supply` | GET | 3 | hooks.ts:useAssetSupply |
| `/assets/{asset_id}/supply/flows` | GET | 1 | — |
| `/assets/{asset_id}/movements` | GET | 1 | — |
| `/assets/{asset_id}/holders` | GET | 3 | app/assets/[slug]/HoldersTabPanel.tsx |
| `/price` | GET | 3 | ../functions/og/[[path]].js |
| `/price/at` | GET | 1 | — |
| `/price/changes` | GET | 1 | — |
| `/price/tip` | GET | 3 | app/docs/page.tsx |
| `/price/tip/stream` | GET | 3 | app/docs/page.tsx |
| `/price/batch` | GET, POST | 3 | app/HomeCurrencies.tsx |
| `/observations` | GET | 3 | app/status/StatusPageClient.tsx |
| `/observations/stream` | GET | 3 | app/docs/page.tsx |
| `/price/stream` | GET | 3 | app/docs/page.tsx |
| `/history` | GET | 3 | app/HomeRecentTrades.tsx |
| `/history/since-inception` | GET | 1 | — |
| `/chart` | GET | 3 | app/assets/[slug]/ChartPanel.tsx |
| `/ohlc` | GET | 3 | app/assets/[slug]/ChartPanel.tsx |
| `/vwap` | GET | 3 | app/docs/page.tsx |
| `/twap` | GET | 3 | app/docs/page.tsx |
| `/oracle/latest` | GET | 3 | app/assets/[slug]/AssetOraclesPanel.tsx |
| `/pools` | GET | 3 | app/HomeTryAPI.tsx |
| `/pools/reserves` | GET | 3 | app/dexes/[source]/PairReservesPanel.tsx |
| `/liquidity-pools` | GET | 3 | app/liquidity-pools/NativePoolsPanel.tsx |
| `/lending/pools` | GET | 3 | app/HomeTryAPI.tsx |
| `/lending/pools/{pool}/reserves` | GET | 3 | app/lending/LendingPoolsTable.tsx |
| `/mev` | GET | 3 | app/docs/page.tsx |
| `/anomalies` | GET | 3 | app/anomalies/AnomaliesFeed.tsx |
| `/divergence` | GET | 3 | app/divergences/DivergenceFeed.tsx |
| `/divergence/series` | GET | 3 | app/divergences/DivergenceFeed.tsx |
| `/oracle/streams` | GET | 3 | app/HomeTryAPI.tsx |
| `/markets` | GET | 3 | app/HomeTopMarkets.tsx |
| `/markets/sources` | GET | 3 | app/docs/page.tsx |
| `/issuers` | GET | 3 | app/docs/page.tsx |
| `/issuers/{g_strkey}` | GET | 3 | app/assets/[slug]/IssuerPanel.tsx |
| `/contracts/{contract_id}/transfers` | GET | 3 | app/contract/ContractView.tsx |
| `/changes/{entity_type}/{id}` | GET | 3 | hooks.ts:useChangeSummary |
| `/diagnostics/cursors` | GET | 3 | app/HomeLivePanels.tsx |
| `/diagnostics/ingestion` | GET | 3 | app/status/StatusPageClient.tsx |
| `/diagnostics/archive` | GET | 3 | app/diagnostics/ArchivePanel.tsx |
| `/diagnostics/backups` | GET | 3 | hooks.ts:useBackupsDiagnostics |
| `/incidents` | GET | 3 | app/HomeTryAPI.tsx |
| `/incidents.atom` | GET | 3 | app/contact/page.tsx |
| `/coverage` | GET | 3 | app/diagnostics/CoveragePanel.tsx |
| `/protocols` | GET | 3 | app/bridges/page.tsx |
| `/protocols/{name}` | GET | 3 | app/dexes/[source]/DexAnalyticsSection.tsx |
| `/protocols/{name}/tvl` | GET | 1 | — |
| `/sdex/orderbook` | GET | 3 | app/dexes/[source]/page.tsx |
| `/ledger/tip` | GET | 3 | components/nav/NetworkSwitcher.tsx |
| `/ledger/stream` | GET | 3 | app/docs/page.tsx |
| `/network/stats` | GET | 3 | app/HomeLivePanels.tsx |
| `/network/throughput` | GET | 3 | app/diagnostics/IngestThroughputChart.tsx |
| `/methodology` | GET | 1 | — |
| `/sources` | GET | 3 | app/HomeTryAPI.tsx |
| `/sources/{name}/health` | GET | 3 | app/sources/[name]/SourceHealthPanel.tsx |
| `/aggregators` | GET | 3 | app/aggregators/RoutedVolumePanel.tsx |
| `/sac-wrappers` | GET | 3 | hooks.ts:useSACWrappers |
| `/rwa/assets` | GET | 3 | app/rwa/RWAView.tsx |
| `/stablecoins` | GET | 1 | — |
| `/rwa/history` | GET | 3 | app/rwa/RWAHistoryPanel.tsx |
| `/rwa/premium` | GET | 3 | app/rwa/RWAPremiumPanel.tsx |
| `/pairs` | GET | 1 | — |
| `/oracle/lastprice` | GET | 3 | app/oracles/OraclesView.tsx |
| `/oracle/prices` | GET | 3 | app/oracles/OraclesView.tsx |
| `/oracle/x_last_price` | GET | 3 | app/oracles/OraclesView.tsx |
| `/account/me` | GET | 3 | hooks.ts:useMe |
| `/account/usage` | GET | 3 | account.ts:fetchUsage |
| `/account/keys` | GET, POST | 1 | — |
| `/account/keys/{keyID}` | DELETE | 1 | — |
| `/account/admin/lookup` | POST | 3 | account.ts:adminLookup |
| `/admin/keys` | POST | 1 | — |
| `/admin/keys/{keyID}` | DELETE | 1 | — |
| `/admin/accounts/{id}` | GET, PATCH | 1 | — |
| `/admin/status-notices` | GET, POST | 1 | — |
| `/admin/status-notices/{id}/resolve` | POST | 1 | — |
| `/register` | POST | 3 | app/pricing/page.tsx |
| `/signup` | POST | 1 | — |
| `/signup/verify` | GET | 1 | — |
| `/dashboard/account` | DELETE | 1 | — |
| `/dashboard/account/export` | GET | 1 | — |
| `/dashboard/keys` | GET, POST | 3 | account.ts:createKey |
| `/dashboard/keys/{id}` | DELETE | 3 | account.ts:revokeKey |
| `/dashboard/webhooks` | GET, POST | 3 | account.ts:createDashboardWebhook |
| `/dashboard/webhooks/{id}` | PATCH, DELETE | 3 | account.ts:deleteDashboardWebhook |
| `/dashboard/webhooks/{id}/deliveries` | GET | 1 | account.ts:listWebhookDeliveries |
| `/dashboard/webhooks/{id}/rotate-secret` | POST | 3 | account.ts:rotateDashboardWebhookSecret |
| `/dashboard/price-alerts` | GET, POST | 3 | account.ts:createPriceAlert |
| `/dashboard/price-alerts/{id}` | PATCH, DELETE | 3 | account.ts:deletePriceAlert |
| `/auth/login` | POST | 3 | app/signin/SignInForm.tsx |
| `/auth/callback` | GET | 3 | app/auth/callback/CallbackHandler.tsx |
| `/auth/verify-code` | POST | 3 | account.ts:verifyCode |
| `/auth/logout` | POST | 3 | account.ts:logout |
| `/auth/passkey/begin-login` | POST | 3 | account.ts:beginPasskeyLogin |
| `/auth/passkey/finish-login` | POST | 3 | account.ts:finishPasskeyLogin |
| `/auth/passkey/begin-signup` | POST | 3 | account.ts:beginPasskeySignup |
| `/auth/passkey/finish-signup` | POST | 3 | account.ts:finishPasskeySignup |
| `/auth/passkey/begin-register` | POST | 3 | account.ts:beginPasskeyRegister |
| `/auth/passkey/finish-register` | POST | 3 | account.ts:finishPasskeyRegister |
| `/auth/passkey/credentials` | GET | 3 | account.ts:listPasskeys |
| `/auth/passkey/credentials/{id}` | DELETE | 3 | account.ts:deletePasskey |
| `/auth/sep10/challenge` | GET | 3 | app/status/StatusPageClient.tsx |
| `/auth/sep10/token` | POST | 1 | — |
| `/ledgers` | GET | 3 | app/ledgers/LedgersTable.tsx |
| `/ledgers/{seq}` | GET | 3 | app/ledger/LedgerView.tsx |
| `/ledgers/{seq}/transactions` | GET | 3 | app/ledger/LedgerView.tsx |
| `/ledgers/{seq}/operations` | GET | 1 | — |
| `/tx/{hash}` | GET | 3 | app/operation/OperationView.tsx |
| `/operations` | GET | 3 | app/operations/OperationsView.tsx |
| `/contracts` | GET | 3 | app/contracts/ContractsView.tsx |
| `/contracts/{contract_id}` | GET | 3 | app/contract/ContractView.tsx |
| `/contracts/{contract_id}/wasm` | GET | 3 | app/contract/ContractView.tsx |
| `/contracts/{contract_id}/interactions` | GET | 3 | app/contract/ContractView.tsx |
| `/contracts/{contract_id}/code-history` | GET | 3 | app/contract/ContractView.tsx |
| `/accounts` | GET | 3 | app/accounts/AccountView.tsx |
| `/directory` | GET | 1 | — |
| `/accounts/stats` | GET | 3 | app/accounts/AccountsAnalytics.tsx |
| `/accounts/creators` | GET | 3 | app/insights/creators/CreatorBoard.tsx |
| `/accounts/sponsors` | GET | 3 | app/insights/sponsors/SponsorBoard.tsx |
| `/accounts/{g_strkey}` | GET | 3 | app/accounts/AccountActivitySummary.tsx |
| `/accounts/{g_strkey}/transactions` | GET | 3 | app/accounts/AccountView.tsx |
| `/accounts/{g_strkey}/operations` | GET | 3 | app/accounts/AccountView.tsx |
| `/accounts/{g_strkey}/movements` | GET | 3 | app/accounts/AccountMovements.tsx |
| `/accounts/{g_strkey}/positions` | GET | 3 | app/accounts/AccountDefiPositions.tsx |
| `/accounts/{g_strkey}/trades` | GET | 3 | app/accounts/AccountTrades.tsx |
| `/accounts/{g_strkey}/activity` | GET | 3 | app/accounts/AccountActivitySummary.tsx |
| `/accounts/{g_strkey}/graph` | GET | 3 | app/accounts/AccountGraph.tsx |
| `/accounts/{g_strkey}/graph/history` | GET | 3 | app/insights/AccountRelationHistory.tsx |
| `/accounts/{g_strkey}/graph/cohort` | GET | 3 | app/insights/AccountRelationCohort.tsx |
| `/search` | GET | 3 | components/nav/SearchModal.tsx |

## Level 1 — the 25 stranded endpoints

All probed live 2026-09-09; none 404s at the route level. Building pages for
them is a product decision, not made here.

### Public data with no surface (10)

| Endpoint | What is stranded | Rough cost |
|---|---|---|
| `/price/changes` | Multi-horizon deltas (1h / 24h / 7d / 30d, each with `reference_at` + `resolution`) in one request. The explorer recomputes a 24h change per surface. | Small: swap into the asset-page strip. |
| `/price/at` | Point-in-time price at any timestamp (cost-basis / PnL / tax lookup), finest CAGG bar, back to 2018. | Small as input to an existing page; a date-picker is a real feature. |
| `/history/since-inception` | Full-history series; XLM returned 3 341 daily points, 2017-01-17 → 2026-09-08, the 2017-08 → 2018-02 gap flagged `discontinuous`. Charts today are bounded windows. | Medium: range control on the asset chart. Param is `asset`, not `base`. |
| `/assets/{asset_id}/metadata` | Full SEP-1 CURRENCIES block: `sep1_status`, `home_domain`, issuer metadata. | Small: panel on `/assets/[slug]`. |
| `/protocols/{name}/tvl` | Per-protocol TVL with per-leg reserves and pricing basis (soroswap: 125 pools, $1.25 M, 11 priced / 114 unpriced, a completeness signal nothing surfaces). | Small: `/protocols/[name]` exists. Lending protocols (blend) return typed 404 `protocol-tvl-not-derived`; handle it, not an error. |
| `/pairs` | Per-pair `trade_count_24h`, `volume_24h_usd`; both params required. | Small; overlaps `/markets`, likely redundant. |
| `/directory` | Curated address labels with tags and provenance. `src/components/DirectoryLabel.tsx` renders the `directory` field embedded in other responses; the bulk endpoint is never called. | Small. |
| `/ledgers/{seq}/operations` | One ledger's decoded operations with `total`/`truncated`; the only per-ledger operations read (`/operations` refuses `?ledger=`). | Small: tab on `/ledgers/[seq]`. |
| `/assets/{asset_id}/supply/flows` | Daily mint / burn / clawback from the `supply_flows` lake, with `net` and `history_incomplete` (SDK `AssetSupplyFlows`). | Small: chart beside the supply card. |
| `/assets/{asset_id}/movements` | Per-asset movements read from the ClickHouse lake. | Small: tab on `/assets/[slug]`. |

### `/methodology`

`/v1/methodology` returns a ~6.9 KB machine-readable document (30 sources, 7
source classes, 6 ADR references, stablecoin proxy list, `version: "1.0"`).
The explorer's `/methodology` page is hand-written and does not call it, by
design: it covers a superset (latency SLOs, numeric precision, freeze policy)
and the explorer is a static export, so API rendering would add a build-time
network dependency. `internal/api/v1/methodology_explorer_drift_test.go` gates
the overlap instead (source-class names and count, outlier default, deferral
on the operator-configured peg map, the formula's price method, the
VWAP-eligibility class, venues each class names). Not gated: a class
description may omit a registered venue (six omitted: cryptocompare,
sushiswap_v3, massive, exchangeratesapi, ecb, blend_emitter), since both
copies describe some venues by category.

### Account/admin surfaces with no UI (12)

All behind auth; unauthenticated probes return a correct `401`.

- **Staff admin (5)**: `/admin/keys`, `/admin/keys/{keyID}`, `/admin/accounts/{id}`, `/admin/status-notices`, `/admin/status-notices/{id}/resolve`. `/dashboard/admin` uses only `/account/admin/lookup`; status notices are posted out-of-band.
- **Legacy key surface (2)**: `/account/keys`, `/account/keys/{keyID}`; superseded by `/dashboard/keys`, wants deprecating.
- **Signup (2)**: `POST /signup`, `/signup/verify`; retired (410 Gone, INV-0907), UI uses the `/auth/login` magic link.
- **Account erasure (2)**: `DELETE /dashboard/account`, `/dashboard/account/export` (#809); API shipped, the dashboard settings page is the follow-up.
- **SEP-10 (1)**: `POST /auth/sep10/token`; not wired here (see below).

## Discoverability

Assessed separately from reachability. Defects found and fixed:

- `sitemap.ts` lacked `/bridges` and `/external/assets` (both nav-linked, indexable); added.
- `/contract?id=`, `/ledger?seq=`, `/tx?hash=`, `/operation?tx=&i=` render entirely from the query string yet canonicalised to the bare path, an empty-shell soft-404 (same class `crawl-surface.test.ts` catches for `/assets/shell`, `/markets/shell`). They now carry `robots: { index: false, follow: true }`, like their canonical counterparts.

Already right: `robots.ts` disallows `/dev/`, `/embed/`, `/auth/`, `/dashboard`, `/signin`, `/signup`, with per-network origin and sitemap URL; all 82 content pages have title and description; canonicals complete on every indexable page; JSON-LD only on `/assets/[slug]` and `/markets/[pair]`, always through `serializeJsonLd`; the sitemap filters through `routeAvailable`. 27 pages are noindex (auth and dashboard surfaces, iframe widgets, design-system reference, unbounded per-entity shells); a noindex URL must not appear in the sitemap. The per-page metadata matrix is enforced by `crawl-surface.test.ts`.

The per-page sitemap/noindex table was removed; `web/explorer/src/app/crawl-surface.test.ts` and `web/explorer/src/lib/route-reachability.test.ts` re-derive it.

## Deliberately excluded

| Surface | Why excluded |
|---|---|
| `/v1/healthz`, `/v1/readyz` | Probes; both are rendered on `/status`, hence `ex` not level 1. |
| `/v1/livez/lake` | Lake-critical probe; monitoring surface. |
| `/v1/version` | Build identity; the explorer shows its own build SHA in the footer. |
| `/embed/*` routes | Chrome-less iframe widgets reached via `src=` from `/widgets`; exempt from the reachability walk, `noindex` by design. |
| `/metrics`-style endpoints | Not in the v1 contract; Prometheus scrape surface. |

## Where the spec and the running API disagree

Probed read-only against `https://api.stellarindex.io` on 2026-09-09; no
contract violations, only drift.

1. **Published spec is stale.** The explorer serves the contract at `stellarindex.io/openapi/stellar-index.v1.yaml` (copied by the `prebuild` script); the published copy was `info.version 1.20.0` vs repo `1.28.0`, missing `/protocols/{name}/tvl`, `/accounts/creators`, `/accounts/sponsors`, `/rwa/assets`. A stale deploy artifact; an explorer rebuild fixes it.
2. **SEP-10 is not wired on this deployment.** `GET /v1/auth/sep10/challenge` returns 404 `sep10-unavailable` for a valid G-strkey. It is the `NoopSEP10Validator` fallback, taken when `STELLARINDEX_SEP10_SEED` / `STELLARINDEX_SEP10_JWT_SECRET` are unset and `auth_mode` is not `sep10`. Docs and the 404 body now say so; enabling it stays a product decision. `middleware.authenticate` is a mutually exclusive switch over the four `auth_mode` values, so enabling SEP-10 turns `sip_*` keys off (the bearer header never accepts both).
3. **Timezone leak (fixed).** Fourteen endpoints (`/v1/price/at`, `/v1/history/since-inception`, `/v1/price`, `/v1/history`, `/v1/chart`, `/v1/observations`, `/v1/lending/pools`, `/v1/coverage`, `/v1/diagnostics/ingestion`, all four `/v1/oracle/*`) emitted a local UTC offset instead of `Z`: any json-tagged `time.Time` from Postgres `timestamptz` decodes into the process's local zone. All 43 such fields now use `WireTime` (UTC); one test scans rendered payloads across twelve endpoints, one fails the build on a new json-tagged `time.Time` in the package.
4. **Stale figure in the spec prose (fixed).** `/history/since-inception` quoted 2 183 points; live is 3 341 back to 2017. `granularity=1m` (50 000 points ending 2018-02-21) still holds.
5. **No spec on the API host.** `/v1/openapi.json`, `/openapi.json`, `/v1/openapi.yaml`, `/v1/spec`, `/v1/docs` all 404; the contract is published only from the explorer origin.
