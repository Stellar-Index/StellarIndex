---
title: D2 — Semantic naming & consistency — lexicon + rename map
---

# D2 — Semantic naming & consistency

**Headline:** verbs + type-suffixes + route-plurality are already disciplined; the
real drift is **one concept with three vocabularies (asset/coin/currency)** and
**one identity with two encodings (dash vs colon, native/XLM/crypto:XLM)**.

## Lexicon (concept → canonical → terms in use)
asset (asset/**coin**/**currency**) · asset_id dash-form (dash vs **colon** `AssetKey`;
native/XLM/crypto:XLM) · pair (pair/**market**) · price (price / rate=FX-only) · source
(source/venue/exchange) · ledger (clean, no `block` leak) · transaction (Tx vs
Transaction) · operation (Op vs Operation) · tier (tier/plan) · issuer (issuer/anchor).

## M0 — causes real confusion / rework
- **M0-1 — asset / coin / currency triple vocabulary.** The HTTP surface unified on
  **asset**, but two internal vocabularies survive and BOTH feed `/v1/assets`: the
  storage read-layer `coin` (`storage/timescale/coins.go`: `CoinRow`, `GetCoinBySlug`,
  `ListCoinsExt`, `CoinsOrder`, +`api/v1/coins.go` `CoinsReader`, `coins_cache.go`,
  `assets_coin_extension.go`) and the catalogue `currency` (`internal/currency/`,
  `api/v1/currencies.go`). Wire leaks: `/v1/changes/coin/{slug}` still accepted;
  `pkg/client/doc.go:81` advertises removed `Coins/Currencies` methods. An agent reading
  `CoinsReader`/`GetCoinBySlug` can't tell it's the `/v1/assets` backing store.
- **M0-2 — asset-id encoding drift.** Canonical wire = dash `CODE-ISSUER` + `native`;
  `supply.AssetKey` = **colon** `CODE:ISSUER` + **`XLM`**. A translator exists at the seam
  (`usd_volume_quote_spec.go:78`). Net: native has THREE ids, every classic asset has TWO
  → a standing "why did the join return zero rows" bug source.

## M1 — real but localized
pair vs market (two overlapping routes /pairs+/markets) · Tx vs Transaction (field/view
abbrev vs type/XDR full; +route `/v1/tx` vs explorer `/transactions`) · Op vs Operation
(`OpIndex` ×421 vs `OperationIndex` ×119) · `/v1/ledger/*` (singular) vs `/v1/ledgers/*`
(plural) same resource · price vs rate (FX-legit but a synonym + brand echo — note
`RateLimit*` is UNRELATED, don't sweep) · venue (only in external config) vs source ·
issuer vs anchor (anchor⊂issuer, partly legit).

## M2 — cosmetic
`MarketSourcesResp` (lone abbrev vs 10 `*Response`) · `AssetDetail` breaks the `*View`
convention (but public/SemVer — accept-with-doc) · package plurals (events/incidents/sources
vs currency/supply singular).

## Rename map (worst first)
1. **`Coin*` read-layer → `Asset*`** (pure internal, zero wire impact — biggest win) +
   `entity_type="coin"` → add `asset` (alias coin) + delete stale `pkg/client/doc.go:81`.
2. `supply.AssetKey` colon/XLM → converge on canonical dash/native (or rename to `SupplyKey`
   + document it as a distinct encoding).
3. Pick one of `OpIndex`/`OperationIndex`; document the Tx/Transaction boundary + route alias.
4. Pick one public noun for /markets vs /pairs (API-version decision). 5. `Venue`→`Source`.

## Already CONSISTENT (don't touch)
Verbs clean (`Get` keyed / `List` slices / `…Batch` multi / `New` universal ctor / `Load`
for embedded — NO Fetch/Make/Enumerate) · zero `block` leak · coherent type-suffix system
(`*View` wire / `*Row` storage / `Envelope[T]` / `*Snapshot`) · `Source`/`SourceName` single
canonical · off-chain prefixes (fiat:/crypto:/rwa:) consistent · collection routes plural
except the 2 documented cases.

**Highest-leverage:** the `Coin*`→`Asset*` internal rename (all in `coins.go`+`coins*.go`+
`assets_coin_extension.go`, no wire impact) + delete the stale client doc — collapses the
asset/coin half of the triple-vocabulary with no external contract change.
