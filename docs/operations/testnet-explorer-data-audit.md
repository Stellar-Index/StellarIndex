# Test-net explorer data audit (testnet + futurenet)

Audit of every data surface the explorer exposes, against what the lean test
nets actually have. **Verdict key: KEEP** (chain-native data present, works),
**FIX** (data exists but the surface is USD/pricing-biased or mis-plumbed so it
looks empty), **CHOP** (irrelevant to a lean test net, or serves wrong mainnet
data — hide/gate network-aware).

> **This revision incorporates an adversarial re-audit (2026-08-27)** that
> probed every claim against the live testnet/futurenet APIs + Postgres. It
> overturned the original "markets FIX" (see §Markets), corrected the
> "identical for both nets" premise (see §Testnet vs futurenet), and surfaced
> several missed surfaces. Volatile row counts from the first pass were dropped
> — they drift tick to tick and must not be cited as fixed evidence.

Method: probed `https://api.testnet.stellarindex.io` and
`https://api.futurenet.stellarindex.io` per surface, cross-read the explorer +
indexer + the two nets' Postgres.

## Core principle

The **chain-native** layer (ledgers, transactions, operations, accounts,
assets, contracts, SDEX trades, classic AMM pools, contract events) is indexed
on the test nets and must be KEPT. The **pricing / USD / aggregator / external
/ verified-catalogue** overlay has no inputs on a lean test net — every asset
is $0 — so it must be CHOPPED (hidden network-aware) or shown without USD.

Two traps beyond the clean KEEP/CHOP split:
1. **Mis-plumbed surfaces that HAVE data but read empty** because they source
   from a table/ordering that is unpopulated on a lean net (markets, accounts
   directory). These need re-plumbing, not chopping.
2. **Mainnet data leaking through un-gated endpoints** (verified catalogue,
   external assets, aggregators) that actively *mislabel* test-net data. The
   nav is hidden but the endpoints still serve mainnet rows — the gate must be
   network-side, not just nav-side.

## Testnet vs futurenet are NOT identical

The original doc claimed the verdicts apply "identically." **False.** Futurenet
is essentially a **contracts-only** net:

| | Testnet | Futurenet |
| --- | --- | --- |
| Ledgers / tx / ops / accounts | present | present |
| Contracts + events | present | **present** (rich — the point of futurenet) |
| Assets (listing/detail/holders/supply) | present (`assets_indexed` ~40s) | **empty** (`assets_indexed: 0`) |
| SDEX trades / pools / markets | present | **empty** (`trades: 0`) |
| Classic AMM liquidity-pools | present | **empty** |

So on **futurenet** the Assets / Markets / SDEX / Liquidity-pool surfaces are
legitimately empty and need a clear **empty-state**, not a "data present"
assertion. KEEP verdicts below are annotated per-net where they diverge.

## KEEP — chain-native, data present

| Surface | Reality | Net |
| --- | --- | --- |
| Ledgers (`/v1/ledgers`, detail, tip, live odometer) | present; tip live | both |
| Transactions + operations | present (per-ledger / per-account) | both |
| Account detail + `…/operations,/trades,/positions,/activity,/transactions` | present; `positions` explicitly applies **no USD** | both |
| Assets listing + detail — **holders, supply, first-seen, trade count** | present | testnet only (futurenet empty) |
| **Classic AMM `/v1/liquidity-pools`** (constant-product pools, native reserves, `mid_price_a_in_b`, depth curve) | present — **MISSED in v1**; core protocol data, not USD-priced, `/liquidity-pools` page already live + not nav-hidden | testnet only |
| **SDEX pairs `/v1/pools`** (per-pair, `source: sdex`, `trade_count_24h`, `last_trade_at`, native `last_price`) | present — **the real working markets surface** (see FIX) | testnet only |
| Contracts (`/v1/contracts`, detail, code-history, interactions, transfers, wasm) + **contract events** (embedded in detail) | present | both |
| Network (`/v1/network/stats`) | present; note `markets_count_24h` reads `prices_1m`, inconsistent with the empty `/v1/markets` (reads `prices_1d`) — see FIX | both |
| SDEX **trades** (per-pair via `/v1/pools`, per-account via `…/trades`) | present in PG | testnet only |

Note: there is **no global trades-listing endpoint** — trades surface per-pair
(`/v1/pools`) and per-account. `/v1/accounts/{g}/movements` is a **data gap**:
it returns `[]` with a coverage note (classic XLM payment history after the
2025-09-03 cutoff "not yet served"), even though `…/activity` shows payments.

## FIX — data exists but the surface is mis-plumbed / USD-biased

| Surface | Problem (corrected) | Fix |
| --- | --- | --- |
| **Markets / SDEX listing** (`/v1/markets`) | **Original diagnosis was wrong.** `order_by=trades_24h_desc` does **not** exist (HTTP 400 — only `pair` / `volume_24h_usd_desc`), and the volume ordering does NOT drop `$0` rows (it `COALESCE`s to 0 and sorts them last). All orderings return `[]`. **Real cause:** `/v1/markets` builds its pair-set from the **`prices_1d`** daily CAGG, which has **0 rows** on the lean nets (its 6h refresh last ran before any trade data existed). `markets_count_24h` (from `prices_1m`, a different table) is non-zero — hence the inconsistency. | On a no-pricing net, **source the markets surface from `/v1/pools`** (`pools_per_source_1h`, materialized, native prices) instead of `/v1/markets`. Point the `/markets` page + homepage `HomeTopMarkets` at pools. (API-side alt: repoint the markets query at `prices_1m` on lean nets, or fix the `prices_1d` materialization lag.) |
| **Accounts directory** (`/v1/accounts`, `/accounts` page) | Returns `{priced_assets:0, accounts:[]}` — **empty because it ranks by USD-priced wealth** (no prices → nothing to rank). **MISSED in v1.** | Rank by **native XLM balance / activity** on a no-pricing net (API-side), or give the page a native-ranked mode. |
| **Homepage "24H VOLUME (USD)"** | `volume_24h_usd: "0"` → shows `—`. | Show a **native measure** (24h trades / markets_count) on test nets, or chop the USD-volume tile. |
| **Pricing overlay is only PARTLY gated** | v1 said "price stream already gated" — insufficient. Only `useTipStream` checks `CURRENT_NETWORK.pricing`. **Un-gated USD paths still fire on test nets:** `useNativeUsdPrice` (`/v1/price?asset=native` → **404**), `/v1/price/batch` (→ `[]`) behind account **"Portfolio value"**, `HomeCurrencies` fiat rates, `AssetConverter`, `/convert`, and every `volume_24h_usd` / `market_cap_usd` column (issuers, exchanges, sources, network, asset detail). | **Broaden the gate:** every USD fetch + every USD column gated on `CURRENT_NETWORK.pricing`. Asset detail: chop `price_usd` / market cap / VWAP / price-chart (KEEP holders/supply/trades). |

## CHOP — irrelevant to a lean test net (hide/gate network-aware)

| Surface | Reality (corrected) | Note |
| --- | --- | --- |
| **Verified-currency catalogue** (`/v1/assets/verified`) | Serves the **30-entry MAINNET catalogue** (XLM, USDC "Circle (centre.io)", PYUSD, EURC, AQUA, yXLM, + fiats) on BOTH nets — and **actively contaminates per-asset detail**: a test-net `USDC` is stamped `unverified_ticker_collision: true` with an `unverified_warning` pointing at *mainnet* Circle USDC (`StellarCollision()`, no network gate). **Confirmed — worse than v1 stated.** | **Network-gate the catalogue load** (serve empty on test nets) — this also kills the false collision-mislabeling. |
| **External assets/markets** (`/v1/external/assets`, CEX/FX) | Endpoint still returns the **populated mainnet global catalogue** (USD, USDT with `price_usd`/`market_cap_usd`) — NOT "not deployed." Only the **nav** is hidden. | Gate the endpoint network-side (or ensure no surface calls it on test nets). |
| **Aggregators** (`/v1/aggregators`) | Returns **mainnet defindex vault configs** (real mainnet `contract_id`s) leaking onto test nets — same contamination class. | Gate network-side; nav already hidden. |
| **`/convert/[from]/[to]`** | Entirely fiat/USD (`/v1/price/batch`, `/v1/chart?…fiat`); non-functional on a no-pricing net. **MISSED in v1** (route reachable, not nav-gated). | Gate the route + any link on `CURRENT_NETWORK.pricing`. |
| Pricing: `/v1/coins` / `/v1/currencies` (404) | No pricing/FX | `/v1/coins?q=` still powers the search modal — keep that path, hide the rest |
| Insights / Anomalies / Coverage / Divergence / MEV | empty (`firing_count 0`, `total_sources 0`) | Nav already removed; also reachable by direct URL — gate or empty-state |
| Oracles (reflector/redstone/band) | not deployed (`/v1/lending/pools` → `[]`) | Nav already removed |
| Protocols (bespoke Soroban DEX/AMM: soroswap/aquarius/phoenix/blend/comet) | not indexed (`/v1/protocols/sdex` metadata exists but `events_24h: 0`) | Nav already removed; **generic Contracts stays** (real Soroban contracts exist) |

Note on routes: `TESTNET_HIDDEN_HREFS` only hides the **nav-rail** items
(`protocols`, `oracles`, `insights`, `exchanges`, `external/assets`). Other
routes (`/markets`, `/dexes`, `/lending`, `/aggregators`, `/anomalies`,
`/divergences`, `/mev`, `/convert`) still resolve by direct URL / hub links and
render empty or mainnet-shaped content. `/markets` in particular is fed to the
testnet homepage via `HomeTopMarkets`, so the homepage shows a visibly empty
"Top Markets" widget until it's repointed at `/v1/pools`.

## Already done (this campaign)

- Nav filtered: Protocols, Oracles, Insights, External group hidden on test nets.
- Price **stream** (`/v1/price/tip/stream`) gated off (`CURRENT_NETWORK.pricing`)
  — but this is only the stream; the non-stream USD paths above are NOT yet gated.

## Implementation plan (post-adversarial)

**Explorer-side (network-aware, test-net-only, zero mainnet risk):**
1. Broaden the pricing gate to every USD fetch + column (`useNativeUsdPrice`,
   `/v1/price/batch` consumers, `HomeCurrencies`, `AssetConverter`, `/convert`,
   `volume_24h_usd` / `market_cap_usd` columns). One `CURRENT_NETWORK.pricing`
   guard per surface.
2. Repoint `/markets` page + `HomeTopMarkets` at `/v1/pools` (native) on
   non-pricing nets; hide the USD-volume tile / show a native measure.
3. Ensure `/liquidity-pools` is surfaced (KEEP) and NOT hidden.
4. Gate `/convert` + `/anomalies` / `/divergences` / `/mev` routes (direct-URL
   reachable) behind pricing/feature flags with a clean empty-state.

**API-side (network-aware — must NEVER change mainnet/r1 behaviour):**
5. Network-gate the verified-catalogue load, `/v1/external/assets`, and
   `/v1/aggregators` to serve empty on test nets (kills the collision
   mislabeling too). Guard on the indexer's network id.
6. Accounts directory: native-balance ranking on a no-pricing net.
7. Markets listing: source from `prices_1m`/`pools` (or fix the `prices_1d`
   CAGG lag) on lean nets — lower priority since the explorer can read
   `/v1/pools` directly.

All API-side changes must be gated on network so r1/mainnet is untouched; codify
in the archival-node role + verify against a mainnet probe before deploy.

## Browser walkthrough confirmations (2026-08-27)

Drove testnet + futurenet in a real browser, page by page, capturing console +
network. Findings beyond the API probes above:

- **Console errors are network-failure logs, not JS exceptions.** A JS-level
  console hook recorded **0** `console.error`/`window.error` across 12 routes;
  the "tons of console errors" are Chrome logging failed requests: the pricing
  **404s** (`/v1/price`, `/v1/price/batch`, `/v1/chart?…fiat`), a transient
  **503** flap on `/v1/healthz`/`/v1/readyz`/`/v1/account/me` during API load
  spikes, `/v1/account/me` **401** on every page for logged-out users (all
  nets — the app should skip that fetch with no session), and Next.js
  static-export **prefetch** noise (`__next._tree.txt` 404s + burst HEAD 503s
  from Cloudflare Pages). Gating the pricing fetches removes the bulk on test
  nets; the 401 needs a session-presence guard; the flaps are the latency
  root-cause (Alert 1).

- **False "Degraded performance" banner on BOTH test nets.** `/v1/status`
  returns `overall: degraded` because `indexer` + `aggregator` services read
  `unknown` (last_seen 00:00:00) — the aggregator is intentionally absent on the
  lean nets, and the indexer heartbeat isn't recorded there. Explorer fix:
  suppress `DegradedBanner` on `!CURRENT_NETWORK.pricing`. API fix (follow-up):
  the status `overall` computation must not count an intentionally-absent
  aggregator as degraded, and the test-net indexer heartbeat should be wired.

- **Futurenet is contracts-only** — homepage shows `0 markets, 0 assets`
  (genuinely empty, not a backfill gap). So on **futurenet specifically**, the
  Assets / SDEX / Markets / Liquidity-pool surfaces should be **hidden**
  (genuinely-empty), keeping Contracts / Ledgers / Transactions / Accounts /
  Network. This is a `CURRENT_NETWORK.id === 'futurenet'` distinction, NOT the
  `pricing` flag (testnet HAS assets/SDEX and keeps them). Add a per-network
  `hasSdexActivity`/`hasAssets` capability to the registry rather than
  overloading `pricing`.

- **Mainnet reference data leaks onto test nets (CHOP, API-side gate).**
  `/v1/sources` serves **29 mainnet source configs** (binance, coinbase, band,
  chainlink, blend, defindex, …) on both test nets — none of which run there;
  the homepage "SOURCES ONLINE" tile counts them. Same class as
  `/v1/assets/verified` (30 mainnet entries + false collision stamps),
  `/v1/external/assets`, `/v1/aggregators`. All are baked into the binary and
  need a **network gate at the API** (serve only sources/catalogue entries real
  for the running network). Careful money-adjacent change → branch + tests +
  test-net-VM deploy, mainnet untouched.

### API-side follow-up queue (network-gated; never change mainnet)
1. Gate the mainnet reference registries on test nets: `/v1/sources`,
   `/v1/assets/verified` (+ the per-asset `unverified_warning`/collision stamp),
   `/v1/external/assets`, `/v1/aggregators`.
2. Status `overall` computation: don't mark an intentionally-absent aggregator
   as degraded; wire the test-net indexer heartbeat.
3. Accounts directory native-balance ranking on no-pricing nets.
4. Markets listing source (`prices_1d` CAGG empty on lean nets → use
   `prices_1m`/`pools`) — lower priority, explorer reads `/v1/pools` directly.

### Presentation-only test-net items (explorer, low severity)
- **Protocol/SDEX page "certified-lake reader is currently unreachable".** The
  endpoint returns HTTP 200 with empty analytics (the certified-lake/verifier
  pipeline isn't run on the lean nets, by design — no backend error in the API
  log). The explorer renders this intentionally-absent analytic as an alarming
  "unreachable" message; it should be a clean "analytics not available on this
  network" empty-state on `!CURRENT_NETWORK.pricing` (or when the analytics
  block is absent). The contract-registry + protocol-identity sections below
  are unaffected.
- **Asset detail on a thin asset** (e.g. testnet BTCZ: 0 holders, no supply, 6
  trades/24h) legitimately renders sparse — that is correct, not a bug; a
  more-active testnet asset (e.g. tUSDC, ~268 holders) shows the full KEEP set.
  Only the "$0 volume (24h)" USD line needs the pricing gate.

### Status page (Goal-1 "look at the status page") — 2026-08-27
Drove the testnet `/status` page. Fixed explorer-side (deployed):
- **"Last seen 739854d ago"** on Indexer/Aggregator — an epoch-0 `last_seen`
  (never-reported service) rendered as an absurd ~2000-year age. Now shows
  "Not reporting" (general robustness fix; mainnet services have valid times).
- **Latency P50/P95/P99 = "0.0 ms" vs "target 0" with red breach bars + a
  "0-min window"** — the lean nets wire no latency metrics, so `/v1/status`
  returns all-zero over a 0s window; `?? null` didn't catch the literal 0. Now
  a zero/absent window renders "not measured" (no cells, no false-red bars).
- **Ingestion labelled "r1 · Hetzner · Frankfurt"** on the testnet page — the
  `REGIONS` name/label were hardcoded to r1. Now network-aware (Testnet/Futurenet
  · Hetzner Helsinki, matching the actual VM host `95.217.x`). Data was always
  correct (`apiBaseUrl` = the current net) — only the label was wrong.

Remaining (API-side, decision-ready):
- **"Degraded performance" verdict** on the test-net status page: `/v1/status`
  returns `overall: degraded` because the intentionally-absent aggregator + an
  unrecorded indexer heartbeat read "unknown". Same root as the (already-
  suppressed) global banner; needs the status `overall` computation to not count
  an intentionally-absent service as degraded, + wiring the test-net indexer
  heartbeat.
- **"PRODUCTION" deployment tag** on the test-net ingestion panel comes from the
  API's `region.deployment` field (the test-net inventory copied r1's
  `deployment = production`); fix in the testnet/futurenet inventory region
  config.
