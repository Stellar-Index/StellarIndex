---
title: /v1/coins → /v1/assets consumer migration plan
last_verified: 2026-05-11
status: in progress
---

# /v1/coins → /v1/assets consumer migration

Tracking the assets-unification endgame: removing the explorer's
`/v1/coins` dependency in favour of `/v1/assets` (R-018 final).

The API side is **complete** as of rc.46:
- `/v1/assets/{id}` returns AssetDetail as a superset of CoinSummary
  (added `price_usd`, `change_1h_pct`, `change_7d_pct`,
  `top_markets`, `price_history_24h`, `price_history_7d`,
  `markets_count`, `trade_count_24h`, `ath`, `issuer_scam_reason`).
- `/v1/assets?network=<chain>` lists assets per blockchain.
- `/v1/assets/verified` lists the cross-chain catalogue.

The explorer side is **partial** — `/assets/{slug}` page is fully
migrated; ~20 other consumers still call `/v1/coins`. Migration
deliberately split into chunks so a single bug surfaces narrowly.

## Why this isn't a one-commit job

`/v1/coins?limit=N` returns
`{data: {coins: [Coin], next_cursor}}` — a rich listing with
price + volume + sparkline data baked into each row.

`/v1/assets?limit=N` returns `{data: [AssetDetail], pagination}`
— a thinner catalogue listing. The coin-extension overlay fires
only on the per-asset `/v1/assets/{id}` detail call, not on the
list endpoint.

So a naive substitution breaks every consumer that renders a
listing column for price/volume/change. Three resolutions:

1. **Extend the listing endpoint** to include the same fields the
   detail endpoint does. ~half-day, biggest change.
2. **Two-call pattern**: listing endpoint for IDs, parallel
   detail calls per visible row. Doubles request count.
3. **Keep listings on `/v1/coins`**, migrate only the detail
   consumers. Acceptable interim — `/v1/coins` is deprecated, not
   removed.

This doc captures the per-consumer plan so a future commit can
work through them in priority order without re-investigating.

## Consumer inventory

20 files reference `/v1/coins` or `/coins/`. Grouped by migration
shape:

### Group A — Listing consumers (need price + volume in one call)

These iterate over coin rows and render columns. Migrating any
one of these requires resolving the listing-shape question above.

- `web/explorer/src/app/HomeTopMovers.tsx`
- `web/explorer/src/app/HomeTopAssets.tsx`
- `web/explorer/src/app/HomeNetworkStrip.tsx`
- `web/explorer/src/app/HomeTryAPI.tsx`
- `web/explorer/src/app/assets/AssetsTable.tsx`
- `web/explorer/src/app/sitemap.ts` (only needs slugs — easy)

### Group B — Detail / route consumers (per-asset)

These fetch one coin at a time. Migration is mechanical — change
the URL, accept the new envelope shape.

- `web/explorer/src/app/assets/[slug]/page.tsx` — ✅ done
- `web/explorer/src/app/assets/[slug]/HistoryTabPanel.tsx`
- `web/explorer/src/app/assets/[slug]/MarketsTabPanel.tsx`
- `web/explorer/src/app/assets/[slug]/AssetClientFallback.tsx`
- `web/explorer/src/app/embed/asset/[slug]/page.tsx`
- `web/explorer/src/app/embed/pair/[pair]/page.tsx`
- `web/explorer/src/app/issuers/[g_strkey]/page.tsx`
- `web/explorer/src/app/widgets/page.tsx`

### Group C — Search / global utility

- `web/explorer/src/components/nav/SearchModal.tsx` — uses
  `useCoins` for autocomplete.
- `web/explorer/src/api/hooks.ts` — defines `useCoins`. Either
  keep (legacy compat) or rename to `useAssets` once Group A is
  done.
- `web/explorer/src/api/types.ts` — generated from OpenAPI;
  follows automatically.

## Recommended sequence

1. **Decide the listing-shape question** (extend `/v1/assets`
   listing vs two-call pattern vs keep on `/v1/coins`). The other
   work blocks on this.
2. Ship the listing extension if (1) chose extension.
3. Migrate Group A consumers one at a time.
4. Migrate Group B consumers in a batch (mechanical).
5. Migrate Group C — search modal + delete `useCoins` if no
   callers remain.
6. Drop `/v1/coins` handler when consumer count hits zero.

## Risk

The explorer build has been fragile around static-params
duplication (rc.45 / rc.46 both shipped fixes). Migrating consumers
in one big commit risks reintroducing slug-shape bugs. Each chunk
should land in its own commit + deploy so a CF Pages build failure
narrows the blame surface.

The `/v1/coins` endpoint stays operational throughout the
migration — there's no urgency to drop it, and removing it
requires zero downstream consumers (incl. external API users).
