---
title: Implementation sketch — split fiat & non-Stellar coins out of the asset directory
status: design (ready to implement)
audit: Audit 2 / LC-001 (flagship)
---

# Fiat / external split — implementation sketch

Goal: make `/v1/assets` and the explorer `/assets` directory **Stellar-only**, and
give fiat currencies (and the `reference_only` coins) their own home under
`/external/*`. Fiat keeps its correct, protected role as a price **quote-unit**
(`fiat:USD`, `XLM/EUR`) and as the divergence/aggregator reference set — only its
mis-framing as a *browseable Stellar asset ranked by M2* is removed.

This is the concrete plan behind LC-001. It is sequenced so the high-value,
non-breaking parts ship now and the wire-shape parts wait for the reserved Unit-D
pre-v1 break.

## Verified premises (reproduced against code, 2026-06-30)

- `internal/currency/verified.go::Browseable()` excludes ONLY `ReferenceOnly` —
  **fiat passes through** (its own comment calls it "the Stellar-focus filter").
- `seed.yaml` = 11 browseable Stellar assets + 15 `reference_only` coins + 19
  `class: fiat` (M2 as `circulating_supply`).
- `web/explorer/public/_redirects` names `/external/forex` as an unbuilt
  "follow-up content migration"; `/external` currently 301s to `/sources`.

## Target information architecture

```
/assets                         ← Stellar assets ONLY (classic + Soroban + XLM + stablecoins)
/assets/{asset_id|slug}         ← Stellar asset detail
/external                       ← new section landing
/external/fiat-currencies       ← the FX board (19 sovereign currencies)
/external/fiat-currencies/{slug}← fiat detail (FX chart + USD rate)  [moved from /assets/{slug}]
/external/reference-prices      ← (optional) the 15 reference coins (BTC/ETH/…) that back pricing
/external/reference-prices/{slug}
```

API mirror:
```
GET /v1/assets                       ← Stellar-only listing (no fiat, no reference coins)
GET /v1/external/fiat-currencies      ← fiat FX board (replaces asset_class=fiat as the "directory")
GET /v1/external/fiat-currencies/{slug}
GET /v1/external/reference-prices[/{slug}]   ← optional; reference coins
```

## Part A — API (Go), non-breaking

1. **`internal/currency/verified.go`** — split the accessor:
   - Keep `Browseable()` but exclude **both** `ReferenceOnly` AND `Class == fiat`
     (it becomes the true Stellar-only filter). Add `FiatCurrencies() []*VerifiedCurrency`
     and `ReferenceCoins() []*VerifiedCurrency` accessors. `CoinGeckoIDs()` /
     `CoinMarketCapIDs()` are unchanged (reference coins still drive pricing).
   - `LookupBySlug` keeps resolving all entries (the detail handlers decide routing).
2. **`internal/api/v1/assets_global.go::handleAssetsVerified`** — now returns only
   Stellar assets (via the narrowed `Browseable()`); drop `attachFiatMarketCaps`
   from this path.
3. **`internal/api/v1/assets.go::handleAssetListUnified`** — `asset_class=all` no
   longer emits fiat rows; remove the fiat-first `market_cap_usd desc` mixed sort
   (rank Stellar assets only). `asset_class=fiat` either (a) 301/410s to the new
   endpoint, or (b) stays as a documented alias of `/v1/external/fiat-currencies`.
4. **New handler `internal/api/v1/external_fiat.go`** — `GET /v1/external/fiat-currencies`
   (+ `/{slug}`): returns the fiat board (ticker, name, FX rate vs USD, optionally
   M2 *clearly labeled as money supply, not market cap*). Reuse the existing fiat
   FX-rate plumbing (`tryFiatCrossRate`, the forex snapshot).
5. **`reference_only` detail** (`/v1/assets/btc`) — `handleAssetGet`/`tryServeGlobalAsset`:
   when the matched catalogue entry is `ReferenceOnly`, either 404 from the Stellar
   namespace or 301 to `/v1/external/reference-prices/{slug}` (decide with product).
6. **OpenAPI + generated artifacts** — add the new paths; regenerate
   `make docs-api docs-postman web-generate-api`. (New endpoints = additive/minor.)
7. **Tests** — `assets_global_test.go`/`assets_test.go`: assert fiat absent from
   `/v1/assets*`, present at `/v1/external/fiat-currencies`; reference coin not at
   `/v1/assets/{slug}`.

## Part B — Explorer (Next.js), non-breaking

1. **New routes** under `web/explorer/src/app/external/`:
   - `fiat-currencies/page.tsx` — the "World currencies" board (move the content
     from `HomeCurrencies`/`VerifiedCurrenciesStrip`'s fiat rows here).
   - `fiat-currencies/[slug]/page.tsx` — move `VerifiedCurrencyView` here.
   - `page.tsx` — `/external` landing linking to fiat-currencies (+ reference-prices).
2. **`/assets` table** (`AssetsTable.tsx`) — remove the "Fiat Currency" tab from
   `ASSET_CLASS_OPTIONS`; stop requesting `asset_class=all` (use the Stellar-only
   default). **`VerifiedCurrenciesStrip.tsx`** — drop fiat rows (or repoint the
   whole strip to Stellar verified assets only).
3. **`lib/fiat-slugs.ts`** — repoint ISO-ticker → `/external/fiat-currencies/{slug}`.
4. **Nav** (`components/nav/Sidebar.tsx`) — add an **External** section: "Fiat
   Currencies" (+ "Reference Prices"). Fix LC-020 in the same pass (point
   `/account/*` hrefs at `/dashboard/*`). Update `SearchModal` hint copy
   ("every asset — crypto, fiat, stablecoins" → "every Stellar asset").
5. **Home copy** — hero "alongside live world fiat rates" + `HomeCurrencies` link
   → `/external/fiat-currencies` (not `/assets`).

## Part C — Redirects (mind the CF 100-rule cap — LC-043)

`web/explorer/public/_redirects` is near Cloudflare's ~100-rule free-plan cap
(~50 legacy `/currencies/*` rules already). Adding 19 fiat-slug redirects could
blow it. Options, in order of preference:
1. **One splat rule** if fiat slugs share a prefix — unlikely (they're plain
   names like `/assets/us-dollar`), so:
2. **A CF Pages Function** (`web/explorer/functions/assets/[slug].js`) that 301s the
   known fiat slugs to `/external/fiat-currencies/{slug}` and passes through Stellar
   slugs — avoids consuming `_redirects` budget (and folds into the A30/A26 review).
3. Replace the `/external → /sources` stub rule with the real section.
4. First **consolidate the ~50 `/currencies/*` rules** (LC-043) to reclaim budget.

## Sequencing

- **Ship now (non-breaking):** Part A (1-5,7), Part B, Part C. All additive or
  listing-filter changes; no wire-shape break. This delivers the user's ask.
- **Unit-D-gated (pre-v1 wire break, separate PR):** the dual-shape
  `/v1/assets/{slug}` split + `NetworkView`/`PerNetworkAssetView` collapse
  (LC-040/LC-031) — these touch `pkg/client` + OpenAPI compat.

## Acceptance

- `/v1/assets`, `/v1/assets/verified`, `/v1/assets?asset_class=all` return **zero**
  `class: fiat` rows; no `reference_only` coin reachable at `/v1/assets/{slug}`.
- `/v1/external/fiat-currencies` returns the 19 fiats; M2 labeled as money supply.
- Explorer `/assets` shows only Stellar assets; `/external/fiat-currencies` renders
  the board; `/assets/{fiat-slug}` 301s to the new home; nav has an External section.
- No mixed-unit `market_cap_usd` ranking of fiat M2 against crypto caps anywhere.
