---
title: API ↔ explorer coverage — every endpoint, and whether a reader can reach it
last_verified: 2026-09-24
status: active
---

# API ↔ explorer coverage

Every path in `openapi/stellar-index.v1.yaml` against what `web/explorer` calls and a reader can reach. Assessed on pubnet (all five capability flags in `src/lib/networks.ts` true); `src/lib/network-routes.ts` hides gated routes elsewhere, which is not a gap.

## The three levels

| Level | Meaning |
|:-:|---|
| **1** | **Not consumed.** No explorer code calls it. |
| **2** | **Consumed but unreachable.** A page calls it, but no link leads there (the sitemap does not count). |
| **3** | **Reachable.** Reachable by links from `/`. |

## Result

| | Count |
|---|---:|
| Paths in the OpenAPI contract | **148** |
| Level 3 — reachable | **115** |
| Level 2 — consumed but unreachable | **0** |
| Level 1 — not consumed | **29** |
| Deliberately excluded (operational) | **4** |

Level 2 is enforced empty: `src/lib/route-reachability.test.ts` fails on any page without a click path from `/`. Level 1 is a product decision, untested.

## How to re-derive this

```
cd web/explorer && pnpm vitest run src/lib/route-reachability.test.ts src/app/crawl-surface.test.ts
cd web/explorer && pnpm vitest run src/lib/api-explorer-coverage-doc.test.ts
grep -n '^  /' openapi/stellar-index.v1.yaml
cd web/explorer && grep -rno '/v1/[A-Za-z0-9/_.{}$-]*' --include='*.ts' --include='*.tsx' src functions
```

`crawl-surface.test.ts` owns discoverability; `api-explorer-coverage-doc.test.ts` pins the table and Result. Exclude generated `src/api/types.ts`; `src/api/account.ts` strips `/v1`, so also match `accountFetch(...)`.

## The table

Level `ex` = deliberately excluded. "Consumed in" is the first call site.

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
| `/assets/{asset_id}/entry-changes` | GET | 1 | — |
| `/assets/{asset_id}/trustline-history` | GET | 3 | app/assets/[slug]/TrustlineHistoryPanel.tsx |
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
| `/ledgers/at` | GET | 1 | — |
| `/ledgers/{seq}` | GET | 3 | app/ledger/LedgerView.tsx |
| `/ledgers/{seq}/transactions` | GET | 3 | app/ledger/LedgerView.tsx |
| `/ledgers/{seq}/operations` | GET | 1 | — |
| `/tx/{hash}` | GET | 3 | app/operation/OperationView.tsx |
| `/operations` | GET | 3 | app/operations/OperationsView.tsx |
| `/contracts` | GET | 3 | app/contracts/ContractsView.tsx |
| `/contracts/stats` | GET | 3 | app/contracts/ContractsView.tsx |
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
| `/accounts/{g_strkey}/entry-changes` | GET | 1 | — |
| `/accounts/{g_strkey}/claimable-balances` | GET | 3 | app/accounts/AccountClaimable.tsx |
| `/accounts/{g_strkey}/signer-on` | GET | 3 | app/accounts/AccountSignerOn.tsx |
| `/accounts/{g_strkey}/sponsorships` | GET | 1 | — |
| `/accounts/{g_strkey}/positions` | GET | 3 | app/accounts/AccountDefiPositions.tsx |
| `/accounts/{g_strkey}/trades` | GET | 3 | app/accounts/AccountTrades.tsx |
| `/accounts/{g_strkey}/activity` | GET | 3 | app/accounts/AccountActivitySummary.tsx |
| `/accounts/{g_strkey}/graph` | GET | 3 | app/accounts/AccountGraph.tsx |
| `/accounts/{g_strkey}/graph/history` | GET | 3 | app/insights/AccountRelationHistory.tsx |
| `/accounts/{g_strkey}/graph/cohort` | GET | 3 | app/insights/AccountRelationCohort.tsx |
| `/search` | GET | 3 | components/nav/SearchModal.tsx |

## Level 1 — stranded endpoints

- **Public data**: the level-1 rows in the table other than the groups below. Notes: `/protocols/{name}/tvl` returns typed 404 `protocol-tvl-not-derived` for lending protocols (handle it); `/pairs` overlaps `/markets`; `/directory` is already rendered via the embedded `directory` field.
- **`/methodology`**: the page is hand-written by design; `internal/api/v1/methodology_explorer_drift_test.go` gates the overlap.
- **Auth-gated, no UI (12)**: staff `/admin/*` (5); legacy `/account/keys*` (2, superseded by `/dashboard/keys`); `POST /signup` and `/signup/verify` (retired, 410); `DELETE /dashboard/account` and `/dashboard/account/export`; `POST /auth/sep10/token`.

## Discoverability

`crawl-surface.test.ts` enforces sitemap and nav agreement, the per-page metadata matrix, and noindex on empty-shell query pages (`/contract?id=`, `/ledger?seq=`, `/tx?hash=`, `/operation?tx=&i=`).

## Deliberately excluded

- `/v1/healthz`, `/v1/readyz`: probes, rendered on `/status`.
- `/v1/livez/lake`: lake-critical probe.
- `/v1/version`: build identity; the footer shows the build SHA.
- `/embed/*` routes: iframe widgets reached from `/widgets`; `noindex`.

## Where the spec and the running API disagree

- The contract is published only at `stellarindex.io/openapi/stellar-index.v1.yaml`; the API host serves no spec.
- SEP-10 is off unless `STELLARINDEX_SEP10_SEED` / `STELLARINDEX_SEP10_JWT_SECRET` are set and `auth_mode` is `sep10`; else the challenge returns 404 `sep10-unavailable`. Enabling it turns `sip_*` keys off.
