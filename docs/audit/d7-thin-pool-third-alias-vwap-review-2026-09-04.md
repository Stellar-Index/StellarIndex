---
title: "D7 — thin-pool third-alias VWAP surface (C4-012/13): served-price walk audit"
status: complete
date: 2026-09-04
scope: "Every read path that walks canonical.AssetAliases and serves, or feeds, a price: which venues can win, what gates the served value, and whether a thin Soroban SAC/SAC pool can displace a deep classic book."
method: "Measured against the tree at origin/main ff2acde5a (file:line cited per row, each anchor checked against 9eeac705b with the fix applied); one finding fixed with a red→green test; the ordering the other paths rely on pinned by tests proven non-vacuous by mutation."
---

# D7 — thin-pool third-alias VWAP surface (2026-09-04)

Launch-plan row 1.9 / decision D7: "the C4-012/13 third-alias thin-pool VWAP
surface needs a deliberate review before public traffic, and note it interacts
with W2's priority ordering." This is that artefact. C4-012/13 has no finding
file of its own anywhere in the repository — the launch plan is the only place
that names it, and this document is its first written treatment.

## Verdict

**One served-price surface was exposed; it is fixed here. Every other served
walk is safe by construction and by gate, and that construction is now pinned.**

- `/v1/price/tip` (and `/v1/price/tip/stream`, which shares `computeTip`)
  **merges** raw trades across BOTH sides' alias families. Once the operator's
  `[supply].sac_wrappers` registry is installed — codified in the ansible
  template, 38 wrapper families
  (`configs/ansible/roles/archival-node/templates/stellarindex.toml.j2:415-459`;
  a template is not evidence of the box) — a classic-quoted tip for a wrapped
  classic pulled the Soroban SAC/SAC pool's prints into the window VWAP with
  no gate on the pool itself. In any 30s window in which the SDEX book was
  silent, **one trade on a few-hundred-dollar pool was the served tip,
  `stale:false`**, ahead of the deep book's closed bucket one tier down.
  Fixed: `tipMergePairs` merges only the established combinations; a SAC-form
  combination the caller did not name is read **last** — after the window at
  both bounds, the closed-bucket read and every other fallback have missed —
  so a pool print can neither displace nor blend into an answer the classic
  book can give, while a wrapped classic whose only market is its pool still
  serves from it. Red→green in `TestPriceTip_ThinPoolThirdAlias_*`.
- Every **first-hit** walk that serves an aggregated price crosses the base's
  alias family with the **literal quote** (`readPriceWithAliases`, the
  stablecoin proxy, the headline tiers, the catalogue SQL, the valuation
  resolver). A Soroban pool carries the SAC form on **both** legs, so from a
  classic-quoted request the SAC/SAC pool is not a candidate at all — the deep
  book's stale bucket is served, flagged stale, never the pool's fresh one.
  Nothing in the code spelled that property out; it fell out of loop shape. It
  is now pinned (`TestPrice_ThinPoolThirdAlias_*`,
  `TestComputeGlobalPrice_VWAPTierConfiguredWrapper*`), and each pin was shown
  to go red under the mutation it guards against.
- The walks that cross both sides (`/v1/price/at`, the raw series and point
  surfaces, and now the tip's last tier) reach a SAC/SAC pool only when every
  established combination has **nothing** in the caller's window or lookback —
  they fill a hole, they do not out-rank a usable answer.
- Four served-surface residuals remain (R1–R4), all bounded and recorded in
  §7; R1 (the alias-union substance verdict on SAC-keyed reads) is a
  served-surface policy choice for the maintainer. Two follow-ups are recorded
  there as well, outside the residual count: R5 (coverage) and R6 (a
  decoder-level pin).

## 1. The shape

Every classic asset with a configured SAC wrapper has two markets that are the
same asset in two spellings:

| market | stored pair | typical depth |
|---|---|---|
| SDEX book | `AQUA-G…/USDC-G…` (classic/classic) | deep |
| Soroban pools (Soroswap, Aquarius, Phoenix) | `CAUIK…/CCW67…` (SAC/SAC) | often a few hundred dollars |

XLM has a third spelling on top (`crypto:XLM`, the CEX feeds), and its Soroban
book is deep (291,883 SAC/SAC buckets measured on r1 2026-09-03), so for XLM the
concern is ordering, not depth. For every other wrapped classic the concern is
exactly the launch plan's: a pool that can be seeded and moved with one trade
must not become the served price of an asset whose depth is on SDEX. The
earlier valuation incident (2026-08-04) was attacker-authored pricing on a
thin venue; the substance gate (`internal/pricingguard/substance.go`) and the
resolver's dust floor exist because of it. The exclusion the tip now applies
is by **form**, not by depth, for the same reason: depth on a pool is what an
attacker can manufacture, and it holds for every wrapped classic, XLM included.

The attack shape used throughout: a pool `X-SAC/USDC-SAC` with ~$300 of depth;
the attacker prints one trade at 5× the SDEX price; the SDEX book has been quiet
for longer than the surface's freshness rule.

## 2. What the alias walk is (facts from the tree)

- `internal/canonical/alias.go:34-38` — XLM's family in priority order:
  `native`, `crypto:XLM`, the SAC. `:155-161` — every configured
  classic↔SAC pair becomes a two-member family `[classic, sac]`.
- `alias.go:169-187` — `Aliases(asset)` returns **the literal input first**, then
  the rest of the family in family order. For a two-member family the family
  order therefore never changes a walk; it only sets the fold key
  (`Canonical`, `:202-211`). For XLM's three-member family it does: a `native`
  read tries `crypto:XLM` before the SAC.
- `alias.go:275-295` — the documented invariant: the SAC form is reached "ONLY
  when both established forms miss — i.e. when the alternative is no price at
  all", and every read path "take[s] the FIRST form that produces a usable
  answer". A merge has no "first" — that is the tip finding, and giving the
  merge a "last" is the fix.
- Soroban decoders stamp **contract ids on both legs**:
  `internal/sources/soroswap/decode.go:408-412`,
  `internal/sources/aquarius/decode.go:364`,
  `internal/sources/phoenix/decode.go:417-421`. SDEX stamps classic on both
  legs. No writer folds a SAC leg onto its classic at insert time
  (`usd_volume_quote_spec.go` resolves SAC→classic for **valuation** only). The
  resolver's own measurement on r1, 2026-09-03
  (`internal/storage/timescale/usd_fx_resolver.go:824-826`): `native×USDC-G`
  215,790 buckets, `SAC×SAC` 291,883, **every mixed-form combination 0**. That
  measurement is cited from the tree, not re-measured here.
- `prices_1m` groups by the literal `(bucket, base_asset, quote_asset)`
  (`migrations/0002_create_price_aggregates.up.sql:35-53`), so a SAC/SAC pool
  and its classic sibling are distinct rows and never blend in the CAGG.

## 3. Served price claims — walk by walk

"Reachable" = can a SAC/SAC pool be a candidate for a **classic-quoted**
request. "Displace" = can it win over a deep answer that is itself usable on
that surface.

| # | Surface | Walk (file:line) | Candidates and pick rule | Gates on the served value | Pool reachable from a classic-quoted read? | Displaces a usable deep answer? |
|---|---|---|---|---|---|---|
| A1 | `/v1/price`, `/v1/price/batch`, `/v1/oracle/lastprice`, `/v1/oracle/x_last_price`, `/v1/assets/{id}` `price_usd`/`market_cap_usd` (via `readPriceWithAliases`, `internal/api/v1/assets_f2.go:483`), ADR-0051 USD leg (`internal/api/v1/price.go:1117`) | `readPriceWithAliases` `internal/api/v1/price.go:853`; loop `:860` | base aliases × **literal quote**; first **non-stale** alias wins (`:871-874`), else the first stale one (`:875-887`) | in the reader, `cmd/stellarindex-api/main.go:3747`: substance (alias union) + scam `:3784`; trailing-baseline guard `:3787` (3× band on ≥5 buckets, 10× on 1-4, `internal/aggregate/served_guard.go:68,78`); freshness 15 min (`defaultVWAPFreshness` `cmd/stellarindex-api/main.go:3565`, applied `:3798`); a pair's first bucket is served `stale` (`lowConfidence`) | **No** — the SAC base × classic quote pair has no venue | **No.** The only mechanism that could prefer a thin alias — fresh beats stale — never sees the pool. Pinned: `TestPrice_ThinPoolThirdAlias_ClassicQuoteServesTheQuietBook`, `…NativeQuoteWalkStaysOnTheLiteralQuote`; mutation (walk the quote aliases) → `price 0.0050`, `stale:false`, `sources ["soroswap"]` |
| A2 | `?quote=fiat:USD` proxy on all of A1 | `tryStablecoinFiatProxy` `internal/api/v1/price.go:1232`; walk `:1287` over `s.usdPeggedClassics` | **classic pegs only** — `parseUSDPeggedClassics` `cmd/stellarindex-api/main.go:4355-4375` drops any non-classic entry; the asset is the literal request (no alias loop); a peg is skipped without a closed bucket in 14 d (`RecentClosedVWAP1mExists` `internal/storage/timescale/aggregates.go:1456`, window `latestVWAPGateWindow` `:1388`) | same reader gates as A1 | **No** — no SAC peg form is ever bound | **No.** Pinned: `TestPrice_ThinPoolThirdAlias_FiatProxyWalksClassicPegsOnly` |
| A3a | Redis VWAP fallback on `/v1/price` and the tip (`tryRedisVWAPFallback` `internal/api/v1/price.go:694-732`) | **no walk** — the literal `(asset, quote)` only: `:702` → `redisTriangulatedLooker.LookupTriangulatedVWAP` `cmd/stellarindex-api/main.go:3263`, which reads the literal `cachekeys.VWAP(base, quote, window)` key (`:3269`) | a key exists only for an orchestrator-configured pair and only after `min_usd_volume` (`internal/config/config.go:1174`, default **$10,000**; an unvaluable quote fails **closed**, `dropForMinUSDVolume` `internal/aggregate/orchestrator/orchestrator.go:1903`, the unvaluable arm `:1910-1913`, the floor `:1922-1925`), the σ-outlier filter and freeze; routes are weakest-link-confidence gated (`internal/aggregate/router.go:41-51`, `rerouteMinConfidence 0.5` `internal/aggregate/orchestrator/triangulate.go:241`) | **No** — a classic-quoted key is the classic pair's own published VWAP | **No** |
| A3b | `?window=300/3600/86400` (`handlePriceWindowed` `internal/api/v1/price.go:2401`, loop `:2417-2418` both sides) | first cached key over base × quote aliases, literal-first, SAC last | as A3a — a pool key exists only if the operator configures the SAC/SAC pair **and** it clears $10k per window, at which point it is not thin | only under that configuration | **No** — a published pool key is reached only where no earlier combination has a key |
| A4 | **`/v1/price/tip`, `/v1/price/tip/stream`** | `computeTip` `internal/api/v1/price_tip.go:169` → `tipMergePairs` `:486` → `tipWindowEscalating` `:348` / `tipWindowVWAP` `:373` | **was** a MERGE of raw trades across base × quote aliases, VWAP over the union; 5 s then 30 s escalation; then A1. **Now**: the established combinations merge (`merge`); a SAC-form combination the caller did not name is held in `last` and read only after the window at both bounds, A1, A3a and the proxies have all missed (`:264-274`) | substance (alias union) + scam **on the requested pair, before any read** (`:179-188`); no trailing guard; window VWAP straight from raw trades | **Was YES** — every SAC combination was merged. **Now** only where every established read has missed — for a wrapped classic with no classic venue (R4) | **Was YES**: with the SDEX book silent in the window, the pool's single print was the served tip (`0.0050000000`, `["soroswap"]`, `stale:false`); for XLM the SAC pool blended into a `native`-keyed tip (`0.71` from `0.315` SDEX, `["sdex","soroswap"]`). **Now no**: the established forms still merge (XLM's reason to merge); with them silent the tip falls to A1; the pool is read last, where the alternative is 404. Red→green: `TestPriceTip_ThinPoolThirdAlias_ClassicQuoteFallsToTheClosedBook`, `…XLMMergesEstablishedFormsOnly`, `…DeeperPoolNeverBlendsIntoTheClassicBook`, `…SorobanOnlyWrappedClassicServesThePoolLast`; `…SACKeyedRequestMergesTheNamedPool` proves the pool stays reachable when named |
| A5 | `/v1/price/at` | `lookupPriceAt` `price_at.go:114`, loop `:115-116` both sides; fallback `:166` over classic pegs `:172` | first combination with a closed bucket within the 24 h lookback (`:52`) | substance (alias union) + scam in `PriceAt` `cmd/stellarindex-api/main.go:5494` (gate `:5497`); `observed_at` is the bucket's close | **Yes, but only** after every established combination has no bucket in the 24 h before `ts` | **No** — the deep book's in-lookback bucket is tried first and wins; the pool fills a hole where the alternative is 404 (residual R2 for the gate that applies once it is reached) |
| A6 | `/v1/assets/{slug}` headline (global tiers) | `tryVWAPTier` `internal/aggregate/global.go:177`, loop `:191`, floor `:199` (≥5 trades, `:116-122`); tiers 2/3 `:245`, `:292`; quote is `fiat:USD` (`internal/api/v1/assets_global.go:151,166`) | base aliases × literal quote; first alias clearing the trade-count floor; **no freshness rule** | reader `globalPriceReader.LatestVWAP` `cmd/stellarindex-api/main.go:3333`: substance (alias union) + scam `:3354`, trailing guard; withheld → "no data" → next tier | **No** — SAC base × `fiat:USD` has no venue | **No**; a quiet-but-deep classic bucket beats a fresh pool by order alone. Pinned: `TestComputeGlobalPrice_VWAPTierConfiguredWrapperSACLast`, `…BelowFloorSACDoesNotRescue` (+ existing `…ReachesSACOnlyLast` for XLM); mutation (reverse the walk) → `trade_count 9999` from the SAC form |
| A7 | `/v1/assets` listing `price_usd`, detail catalogue overlay, sparkline | `assetPriceCTEs` `internal/storage/timescale/asset_price_snapshot.go:129`, query `:149-159` (`DISTINCT ON (base_asset) … ORDER BY bucket DESC` over quote forms {USDC-G, USDC-SAC, `fiat:USD`}); sparkline `internal/storage/timescale/asset_catalogue.go:1030,1167` (`ORDER BY array_position(aliases), bucket DESC`); `foldAliasTwins` `internal/api/v1/assets.go:3604-3624` | per **literal** base row; the SAC twin is its own row and is folded away with only volume merged (`mergeAliasVolume` `:3641`) | `listingPriceAllowed` `internal/api/v1/asset_catalogue_extension.go:157` (substance) on the detail overlay; dust-liquidity guard on caps | **No** — a classic base row never pairs with a SAC quote; the SAC row's price is dropped by the fold, not adopted | **No** |
| A8 | `/v1/changes/{type}/{id}` | `changes.go:140-165` | rollup rows, established forms first, SAC last, first hit; a percentage, not a price | n/a | last only | **No** |

W2 interaction, answered: every first-hit walk above rests on W2's SAC-last
family order **and** on `Aliases()` leading with the literal input. The merge
walk (A4) was the one consumer of the family with no "first" and therefore no
protection from the ordering — it needed a "last", and it now has one: the
established combinations merge, and an unnamed SAC-form combination is read
only after every other read has missed.

## 4. Raw and point surfaces (ADR-0018 raw tier)

These are deliberately **not** substance-gated ("the substance gate would newly
404 every THIN pair here", `internal/api/v1/vwap.go:93-100`); they carry the
scam gate only (`internal/api/v1/vwap.go:108`). A thin pool that answers here answers because
nothing else traded in the caller's window — which is what a raw surface is
for.

| # | Surface | Walk | Pick rule | Pool reachable from a classic-quoted read? |
|---|---|---|---|---|
| B1 | `/v1/vwap`, `/v1/twap`, single-bar `/v1/ohlc`, non-fiat quote | `tradesInRangeWithStablecoinFallback` `internal/api/v1/vwap.go:281`, loop `:296-297` both sides | first combination with **any** trade in the window | only when every established combination is empty in the window |
| B2 | the same, fiat quote; `/v1/ohlc` series fiat | `usdPeggedConstituents` `ohlc_fiat_combine.go:91` (loop `:94`): base aliases × `ExpandTargetPairWithClassicPegs` (`internal/aggregate/stablecoin.go:197-241`: direct + crypto backers + **classic** pegs) | combine all constituents | **No** — the peg side never takes a SAC form |
| B3 | `/v1/ohlc` series, `/v1/history`, `/v1/chart` | `ohlc_series.go:360-361`, `history.go:767,796`, `chart.go:769-770` — both sides | first non-empty series | only when every established combination is empty over the window |
| B4 | `/v1/chart` and since-inception fiat proxies | `chartFiatProxyPairs` `chart.go:702` (base-major `:721`), `history.go:838` (`:849`); `usdPegProxyQuotes` `internal/api/v1/price.go:790-813` | every classic peg before any SAC peg, base literal before base SAC | only after every classic-base × classic-peg series is empty; pinned by `TestChart_StablecoinFallback_EveryClassicPegBeforeAnySAC` |

## 5. Valuation and rollup readers (not served; they feed `usd_volume`, which feeds the substance floor)

| # | Reader | Walk | Pick rule | Gate | Note |
|---|---|---|---|---|---|
| C1 | `VWAPUSDFXResolver.queryDB` `usd_fx_resolver.go:857`, loop `:858`; `queryDirectLeg` `:902` | base aliases first-hit × peg forms as a **set** (classic + SAC, `quote_asset = ANY`) | first base form with a bucket inside the 1 h freshness (`:203-204`) | dust floor: quote notional ≥ $0.01 (`:910`, `directLegMinQuoteVolume` `:915`) | the pool prices `<asset>`-quoted trades only after the classic form has had no bucket for 1 h; pinned by `TestVWAPUSDFXResolver_AliasLoopStopsAtTheEstablishedForm` — residual R3 |
| C2 | `LatestAssetStats` `internal/storage/timescale/asset_catalogue.go:2080`, `AssetAliasStrings` `internal/storage/timescale/markets.go:775`, `asset_volume_character_rollup.go:135` | `= ANY(aliases)` / `AllAliasForms` fold | additive sums and filters | — | volume, never a price pick |
| C3 | `SubstanceGate.measure` `substance.go:282`, union `:285-286` | sums every base × quote alias combination | — | — | a thin SAC/SAC pair with a deep classic sibling is **allowed on the sibling's strength** — residual R1 |

## 6. The attack, traced

$300 pool `X-SAC/USDC-SAC`, one print at 5× the SDEX price, SDEX quiet.
`Y` below is a wrapped classic with **no** classic venue at all — its only
market is its pool.

| Request | Before this change | After |
|---|---|---|
| `/v1/price?asset=X-G&quote=USDC-G` | SDEX bucket (`stale:true` once quiet > 15 min) | same |
| `/v1/price?asset=X-G` (`fiat:USD`) | SDEX bucket via the classic-peg proxy | same |
| `/v1/assets/X-G` `price_usd`, listing, sparkline | SDEX | same |
| `/v1/price/tip?asset=X-G&quote=USDC-G`, SDEX silent 30 s, closed bucket exists | **the pool's print**, `stale:false`, `["soroswap"]` | SDEX closed bucket via A1, `["sdex"]`; the pool is never read |
| `/v1/price/tip?asset=X-G&quote=USDC-G`, SDEX **dormant** — silent for hours or days, its latest closed bucket still inside the reader's 14-day gate window (`LatestClosedVWAP1mForPair` `internal/storage/timescale/aggregates.go:1293`, gate `:1356-1363`, `latestVWAPGateWindow` `:1388`) — and the SAC pool printing in the window | the pool's window VWAP, `stale:false`, `["soroswap"]` — fresh, thin, attacker-movable | the dormant close via A1, `["sdex"]`: `price_type` `vwap`, **`stale:false`** by the tip's envelope contract (ADR-0018, `internal/api/v1/price_tip.go:146-148`; `tipFlags` `:152-156` sets `single_source` and the divergence flags only; the reader's stale bit is discarded at `:205`), with `observed_at` the bucket's close (`VWAP1mToSnapshot` `internal/api/v1/price.go:2319`, `:2325`) carrying the age. It is the value `/v1/price` gives for the pair, flagged `stale:true` there once the close is older than 15 min. With no closed bucket inside the gate window the same read falls to the pair's last trade (`cmd/stellarindex-api/main.go:3822`; `price_type` `last_trade`, `observed_at` the trade's time, `stale:false` on the tip) whenever the substance union — which the live pool can carry — passes. This is the one classic-keyed shape whose served value the change moves: off a fresh thin print, onto the deep book's old one |
| `/v1/price/tip?asset=native&quote=USDC-G`, SAC pool printed in window | blended (`0.71` against `0.315` SDEX) | SDEX-only window VWAP |
| `/v1/price/tip?asset=X-G&quote=USDC-G`, the pool is the **deeper** venue in the window (ten prints against one SDEX trade) | blended (`0.0046` against `0.0010` SDEX), `["sdex","soroswap"]` | SDEX-only window VWAP, `["sdex"]` — the exclusion is by form, not depth; `/stream` consumers on such a pair see `sources` shrink to the classic venues. Accepted: it is the `/v1/price` literal-quote posture |
| `/v1/price/tip?asset=Y-G&quote=USDC-G`, pool printed in window | the pool's window VWAP, 200, `["soroswap"]` | same value, 200, `["soroswap"]` — but reached **last**: after the classic window at 5 s and 30 s, the closed-bucket read, the Redis literal key and the proxies have all missed. Accepted (R4): the alternative is 404 for an asset whose only market is the pool |
| `/v1/price/tip?asset=Y-G&quote=USDC-G`, pool silent > 30 s | 404 (A1 has no classic bucket; A3a reads the literal classic key; no proxy for a non-fiat quote) | same — unchanged coverage gap, R5 |
| `/v1/price/at?asset=X-G&quote=USDC-G&ts=T`, SDEX had a bucket in the 24 h before T | SDEX | same |
| …SDEX had **no** bucket in that 24 h | the pool's bucket (else 404), `observed_at` = its close | same (R2) |
| `/v1/price?asset=X-SAC&quote=USDC-SAC` | the pool's own VWAP (the caller named it) | same (R1) |
| `usd_volume` of trades quoted in X, SDEX quiet > 1 h | valued at the pool's rate for ≤ 1 h | same (R3) |

Whether the served price moves for a classic-quoted request, in one line:
**only the tip did; it now moves only where no established read can answer at
all, and there it serves the same pool value it always did.**

## 7. Residuals — accepted with reason (R1–R4), and follow-ups (R5–R6)

- **R1 — alias-union substance verdict on SAC-keyed literal reads.**
  `/v1/price?asset=X-SAC&quote=USDC-SAC` serves the pool's own bucket while the
  gate (`substance.go:285-286`) passes on the classic sibling's volume. What
  still bounds it: the trailing-baseline guard on the pool's **own** history
  (≤ 3× per minute against its median with ≥ 5 buckets, 10× below that,
  `served_guard.go:68,78`), the first-ever bucket served `stale`, 15-min
  freshness. The two fixes both change served policy: measuring the served
  pair alone withholds every Soroban-keyed read whose pool is under the
  substance floor — ≥ $1,000 volume **and** ≥ 20 distinct buckets **and**
  ≥ 6 h span, over the trailing 24 h — (a blackout of Soroban wallets'
  natural spelling), and a
  canonical-first quote walk breaks the literal-first contract
  (`alias.go:291-295`). **Maintainer decision.** If taken, the least-invasive
  shape is one additional method on `SubstanceGate` that measures the literal
  pair, called from `storePriceReader.LatestPrice` only (the pre-read callers —
  tip, listing, TVL — must keep the union, or `native/fiat:USD` withholds XLM).
- **R2 — `/v1/price/at` after 24 h of classic silence** reaches the pool under
  the same union verdict. `observed_at` exposes the bucket's age; the
  alternative is 404. Same decision as R1.
- **R3 — valuation tier 3 through the pool after 1 h of classic silence**
  (`usd_fx_resolver.go:857-938`) is bounded by the $0.01 quote-notional floor
  and the 1 h freshness, and affects only the `usd_volume` of trades quoted in
  that asset. It is the route by which an attacker-authored rate can inflate a
  counter-pair's USD volume toward the substance floor; the 2026-08-04
  remediation chose the floor + freshness over a substance check on this
  hot-path resolver. Not changed here.
- **R4 — the tip's last tier for a wrapped classic with no classic venue.**
  When every established read has missed, `computeTip` reads the SAC-form
  combinations (`price_tip.go:264-274`) and serves the pool's raw window VWAP
  with no trailing-baseline guard — the tip has none on any tier. What bounds
  it: the substance gate runs first and, for an asset whose only market is the
  pool, the alias union **is** the pool, so the full floor applies to the
  pool itself — three-part, every part required: ≥ $1,000 volume **and**
  ≥ 20 distinct closed 1-minute buckets **and** ≥ 6 h span between the
  oldest and newest active bucket, all over the trailing 24 h (`SubstanceOK`
  `internal/pricingguard/substance.go:195-209`, defaults `:84-87`; a
  one-burst pool clears the volume part and fails the other two); the scam
  gate; and the tier is unreachable for any asset
  with a closed classic bucket, a published classic key or a classic peg
  route. Accepted: it is the served behaviour that asset already had, the
  alternative is 404 for an asset whose only market is the pool, and the
  change here adds the order without removing the answer. Pinned by
  `TestPriceTip_ThinPoolThirdAlias_SorobanOnlyWrappedClassicServesThePoolLast`
  (200 from `soroswap`, read after the classic window and the closed bucket;
  404 with the SAC set dropped).
- **R5 — coverage, not safety.** `/v1/price?quote=fiat:USD` 404s for a
  Soroban-only wrapped classic (the proxy walks classic pegs only) while
  `/v1/chart` serves it; a SAC-keyed `/v1/price?quote=fiat:USD` 404s too; and
  the classic-keyed tip for such an asset 404s once its pool has been silent
  for 30 s (A1 has no classic bucket, A3a reads the literal key). Closing any
  of these must keep `usdPegProxyQuotes`'s every-classic-before-any-SAC order
  and route through the reader's gates.
- **R6 — the structural invariant is a decoder property, not a rule.** The
  safety of A1/A2/A6/A7 rests on Soroban rows carrying the SAC form on both
  legs. A future decoder or backfill that folds a SAC leg onto its classic at
  insert time would create mixed-form pairs, and `readPriceWithAliases`'s
  fresh-beats-stale rule would then reach the pool from a classic-quoted read.
  The pins added here hold the **quote** literal and would not catch that; a
  decoder-level pin (every Soroban-sourced trade row has both legs
  `AssetSoroban`) is the follow-up.
- **Follow-up, pre-existing, not changed here — the tip's staleness bit on
  a dormant book.** `stale` is always `false` on the tip (ADR-0018;
  `tipFlags` `internal/api/v1/price_tip.go:152-156` never sets it, and the
  closed-bucket tier discards the reader's bit at `:205`). The closed-bucket
  read behind that tier is bounded by the 14-day gate
  (`LatestClosedVWAP1mForPair` `internal/storage/timescale/aggregates.go:1293`,
  gate `:1356-1363`, `latestVWAPGateWindow` `:1388`), but the last-trade arm
  it falls to is not (`LatestTradesForPair`
  `internal/storage/timescale/trades.go:1384`, `ORDER BY ts DESC LIMIT`;
  returned as stale by the reader, `cmd/stellarindex-api/main.go:3849`). So
  on a freshness surface a closed bucket up to 14 days old, or a last trade
  of any age, is served `stale:false` whenever the substance union passes,
  with only `observed_at` carrying the age. The tip's closed-bucket tier has
  always read this way; the change here moves one more shape onto it (the
  dormant-book row in §6) and does not alter the envelope contract. The
  remedy, if wanted, is on the tip's envelope, not on the walk.

Related, not closed here: the G9 money-data triage note flags the XLM/USD
valuation anchor (`usd_fx_resolver` `directLegMinQuoteVolume = 0.01`, `ORDER BY
bucket DESC LIMIT 1`) as a possible duplicate of D7. R3 covers the pool route
only; the one-cent SDEX print route is a separate question and this document
does not close it.

## 8. Evidence — what was run

All from the worktree at `origin/main` ff2acde5a; the `path:line` anchors in
this document were then checked against `origin/main` 9eeac705b with the fix
applied (last row).

| Step | Command | Result |
|---|---|---|
| build at HEAD | `go build ./...` | exit 0 |
| new API pins at HEAD | `go test ./internal/api/v1/ -run ThinPoolThirdAlias -count=1` | PASS, exit 0 |
| new aggregate pins at HEAD | `go test ./internal/aggregate/ -run 'ConfiguredWrapperSACLast\|BelowFloorSACDoesNotRescue' -count=1` | 2 PASS, exit 0 |
| mutation A (walk the quote aliases in `readPriceWithAliases`) | same API run | FAIL, exit 1 — `price = "0.0050", want … "0.0010"`, `stale = false, want true`, `sources = "soroswap", want sdex`; reverted, `git diff` empty |
| mutation B (family order `[sac, classic]` in `NewAliasRegistry`) | same aggregate run | PASS — **ineffective by construction** (`Aliases()` leads with the literal input); recorded so nobody re-tries it as a proof |
| mutation B2 (reverse the alias walk in `tryVWAPTier`) | same aggregate run | FAIL, exit 1 — `trade_count = "vwap_native"/9999, want vwap_native/50`; `vwap query[0] = "CAUIK…"`; reverted, `git diff` empty |
| mutation C (drop the `last` set instead of reading it) | `go test ./internal/api/v1/ -run PriceTip_ThinPoolThirdAlias -count=1` | FAIL, exit 1 — `SorobanOnlyWrappedClassicServesThePoolLast`: `status = 404, want 200`; reverted, file byte-identical |
| mutation D (merge the full cross — the pre-fix walk) | same run | FAIL, exit 1 — `ClassicQuoteFallsToTheClosedBook`: `"price":"0.0050000000"`, `"sources":["soroswap"]`; `XLMMergesEstablishedFormsOnly`: `"price":"0.7100000000"`, `["sdex","soroswap"]`; `DeeperPoolNeverBlendsIntoTheClassicBook`: `"price":"0.0046363636"`, `CAUIK…/CCW67…` consulted; `SorobanOnlyWrappedClassicServesThePoolLast`: served without the closed-bucket read ever running; reverted, file byte-identical |
| tip tests after the fix, plus the whole tip family | `go test ./internal/api/v1/ -run 'PriceTip\|ThinPool\|Tip' -count=1` | ok, exit 0 |
| the gates | `go build ./...`; `go test ./internal/api/... ./internal/aggregate/... -count=1`; `golangci-lint run --allow-parallel-runners ./internal/api/... ./internal/aggregate/...`; `lint-docs.sh`; `lint-doc-links.sh`; `lint-lexicon.sh` | all exit 0 (lint: `0 issues`; doc links: 596 files) |
| anchors | every `` `path:line` `` anchor in this document extracted; each path resolved to exactly one tracked file (basenames that exist in more than one package carry their directory), each cited line present, and where a symbol is named beside the anchor, the symbol on that line | 123 anchors, every one resolved and present, 0 misses |

## 9. Files

- `internal/api/v1/price_tip.go` — `tipMergePairs` (the fix: `merge` and `last`), `tipWindowEscalating`, the last tier in `computeTip`; `tipWindowVWAP` takes the pair set it is reading.
- `internal/api/v1/price_tip_thin_pool_test.go` — red→green for the tip: the closed-book fall-through, XLM's established-only merge, the deeper pool, the Soroban-only wrapped classic (with the call order), and the named-SAC cross.
- `internal/api/v1/thin_pool_third_alias_test.go` — the `/v1/price` walk pins.
- `internal/aggregate/global_thin_pool_test.go` — the headline-tier pins for configured wrappers.
- `docs/operations/v1-launch-plan.md` — row 1.9 and D7 marked done, pointing here.
- `CHANGELOG.md` — the served-behaviour change on the tip.
