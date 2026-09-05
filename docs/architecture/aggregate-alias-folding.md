---
title: Folding a market's two spellings into an aggregate
last_verified: 2026-09-05
status: implemented
---

# Folding a market's two spellings into an aggregate

**Last verified:** 2026-09-05
**Status:** Implemented. The direction fold shipped as the aggregate
half of launch-plan row 1.16; the cross-scale bar defect §7.4 named as
1.15's blocker is fixed; and the alias widening on the fiat quote leg
— rows 1.15 (series) and 1.14 (point), landed together — is §7.5. §7.4
is kept as written because it records a correction worth keeping: the
blocker it called disqualifying rested on a claim about the schema that
was false.
**Scope:** launch-plan rows 1.14, 1.15 and the aggregate half of 1.16.

---

## 0. The one-paragraph answer

Three open rows were circling one question, and they turn out to be
**two** questions, not one.

The first is *direction*: a market has no stored orientation of its own,
so `trades` holds it as both `(A,B)` and `(B,A)` rows. Reading one
spelling gives a partial market. The answer is a per-row leg swap, and
it needs no new rule for a mean or an extreme — every aggregate this
data feeds is defined on the two integer leg amounts rather than on a
price, so swapping the legs re-weights the mean at the same instant as
it inverts the price, exactly and without dividing. That is implemented.

The second is *alias*: a market's two legs each have several canonical
spellings, and a thin Soroban pool is a different market from the deep
SDEX book that shares an asset with it. Widening a read across spellings
therefore admits new liquidity, not new rows of the same liquidity — and
the two must never be summed into one bar. So the widening is gated, not
merged: a held-back spelling answers only a bucket no established one
holds. §7.5 has the rule, the r1 measurement behind it and what the
alternative would have cost.

Distinguishing the two is the whole content of this document. **Folding
a direction adds no market. Folding an alias adds one.**

---

## 1. What the aggregates actually compute

Every consumer of `Store.TradesInRange` reduces a `[]canonical.Trade` to
one number or one bar. There are three shapes, and none of them reads a
stored price, because `canonical.Trade` has no price field — the doc
comment says so outright ("Price is NOT stored; it is derived from
QuoteAmount / BaseAmount at query time").

| Shape | Definition | Fields read |
|---|---|---|
| `aggregate.VWAP` | `Σ(QuoteAmount) / Σ(BaseAmount)` | the two amounts |
| `aggregate.ComputeOHLC` | per row `price = QuoteAmount/BaseAmount` as an exact `big.Rat`; `High = max`, `Low = min`, `Open`/`Close` by slice order | the two amounts, plus order |
| `aggregate.TWAP` | `Σ(price·Δt) / Σ(Δt)`, `Δt` from consecutive timestamps | the two amounts, plus `Timestamp` |

`aggregate.TotalBaseVolume` / `TotalQuoteVolume` are plain sums of the
same two columns.

This is the load-bearing observation. A fold that gets the two integer
legs right gets all five numbers right, and there is nothing else to get
right.

---

## 2. The fold, and why it needs no re-weighting step

`orientTradeTo` swaps the pair and the two smallest-unit amounts:

```go
t.Pair = want
t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
```

For a row stored `(A,B)` with `a` units of A and `q` units of B, the
same trade expressed as `(B,A)` has `q` units of base and `a` units of
quote. That is definitional, not an approximation.

### The volume-weighted mean

The question the launch plan poses is whether the fold has to re-weight,
"and if so how, exactly, without dividing where an exact reciprocal is
available".

It does have to re-weight, and the swap **is** the re-weighting. The
weight in `Σq/Σb` is the row's base leg, and a flipped row's base leg in
the requested orientation is its stored *quote* amount. So the weight
changes meaning and changes value in the same operation that inverts the
price. There is no second step, and nothing divides: the only division
in the whole path is the final `SetFrac(sumQuote, sumBase)`.

The fold that skips it is the one to watch for. Inverting each flipped
row's price while leaving its weight where the database put it computes

```
Σ(b²/q) / Σ(b)
```

— a volume-weighted mean of reciprocals, which is not the reciprocal of
a volume-weighted mean and is not a price of anything. On the fixture in
`TestTradesInRange_AggregateOverBothStoredDirections` it answers
**1.0127659574** where the market's VWAP is **0.1825**.

There is a property that separates the two without reference to any
fixture, and it is pinned as a test. Because `Σq/Σb` and `Σb/Σq` are
ratios of the *same two totals*, the folded VWAP of a market and the
folded VWAP of its flip are exact reciprocals:

```
VWAP(A/B) x VWAP(B/A) == 1     exactly, for any window
```

A fold that re-priced without re-weighting cannot satisfy this for any
window holding more than one price. Unfolded, the same assertion reports
`56644/55225`.

### The high and the low

An inverted price turns a maximum into a minimum, so ordering and
inversion do not commute. The rule is that **re-expression happens
before the comparison, never after**.

At row level this is free, because a row carries one price rather than
an interval: fold every row first, then `max` and `min` over the folded
prices. Nothing has to know that an extreme swapped roles.

At *bucket* level it is not free, and `Store.OHLCSeries`'s `norm` CTE
has to write it out longhand — a flipped bar's requested-orientation
high comes from its stored **low**:

```sql
CASE WHEN base_asset = $1 THEN high_price ELSE 1.0 / NULLIF(low_price, 0) END AS hi,
CASE WHEN base_asset = $1 THEN low_price  ELSE 1.0 / NULLIF(high_price,0) END AS lo,
```

That asymmetry is the reason the raw-row fold is the safer of the two
and the reason it is where this change lives.

The failure shape here is the worst one available, and it is why this
read was held back rather than folded with the mechanical ones. An
unfolded read returns *less* data — the bar is thin but internally
consistent. A read that fetches both directions and merely relabels them
returns a bar that is fully populated and wrong: on the test fixture it
reports a **high of 10.0000000000** where the market's high is
**0.25**, because a stored AQUA-per-USDC price of 10 was compared as
though it were USDC-per-AQUA. Nothing about the response looks
malformed.

A second, cheap invariant is pinned alongside it: `low < VWAP < high`.
A population holding two orientations at once routinely violates that,
so it is the least expensive smoke test for the whole class.

### The time-weighted mean

`Δt` comes from `Timestamp`, which a swap does not touch, and the
per-trade price is the exact ratio of the two folded legs. So TWAP over
the folded set is the correct time-weighted mean of the requested
orientation's prices.

It is worth stating what is *not* true: the folded TWAP is **not** the
reciprocal of the flipped TWAP, because a time-weighted mean of
reciprocals is not the reciprocal of a time-weighted mean. That is
correct behaviour, not a defect — TWAP answers "what price was current,
for how long", and that question has a different answer in each
orientation. VWAP's reciprocal identity is a property of ratios of
totals and does not generalise.

TWAP has one further requirement the fold must not break: it takes its
weights from **slice order** and deliberately does not sort. The union
therefore keeps the read's existing total order and the ascending
contract, pinned as its own assertion.

### Scale

`aggregate.NormalizeAmountScale` lifts each trade to a common
smallest-unit scale by multiplying **both** legs by the same
per-source factor. Multiplying both legs by one factor commutes with
swapping them, so the fold and the normalisation are order-independent
and neither has to know about the other.

`aggregate.AdjustPrice` corrects a per-pair base-vs-quote decimals
mismatch as a post-hoc scalar. It is already called with the
orientation the *caller* asked for, which is the orientation the folded
rows are now in, so it needs no change either.

---

## 3. Which layer the fold lives in

**The store.** `Store.TradesInRange` reads both stored directions as two
limited arms and re-expresses per row.

The last two fixes split on this question and split differently, so the
reasoning matters:

- `/v1/history` folds in its **caller**, because its read carries a
  keyset cursor. A cursor names a position in one ordering; two
  directions have to be merged and resumed together or a page cuts
  through a tie group. That is caller knowledge, and
  `Store.TradesInRangeAfter` stays honest about serving one arm of it.
- The two latest-trade reads fold in the **store**, because they have no
  cursor and every caller wants the same thing.

`TradesInRange` is in the second category. It has no cursor. Its five
callers — `/v1/vwap`, `/v1/twap`, single-bar `/v1/ohlc`, `/v1/price/tip`
and the aggregator orchestrator — all want "the market's trades in this
window, expressed the way I asked". None of them wants one spelling, and
a caller-side fold would be the same code five times, across two
disjoint interfaces (`v1.HistoryReader` and `orchestrator.Store`) that
share no seam.

**What makes it provable.** The package has a scripted `database/sql`
driver that replays canned rows regardless of the SQL, so it splits the
proof in two halves, the way the shipped precedent does:

- *behaviour* — script a row in each stored direction and assert what
  comes back, and what a mean and an extreme over it come to. No
  database.
- *query shape* — read the statement the store actually issued and
  require both arms, both window bounds and all three limits. The
  scripted driver replays whatever the SQL says, so this is the only
  thing that can prove the flipped row was **fetched**.

On top of both, the class guard (§6) now parses every SQL literal in the
package, so the next reader of this table is caught whether or not
anyone writes it a test.

---

## 4. The truncation argument

`TradesInRange` orders descending and cuts at `limit` so truncation
keeps the **newest** rows. Two arms each cut at `limit`, unioned,
re-sorted and cut again is still exactly the market's newest `limit`:

> a row in the market's newest `limit` has at most `limit-1` rows above
> it in the whole market, hence at most `limit-1` above it within its
> own direction, so it survives its own arm's cut.

So the arms may be limited individually, `len(rows) == limit` still
means the window overflowed, and the orchestrator's truncation detector
keeps working. It will fire **more often**, correctly: the true
population is now roughly twice what the read used to see.

---

## 5. What this changes on live data

Measured on r1, 2026-09-05, bounded reads only.

One hour of `native / USDC-GA5Z…`, the flagship market:

| | served today | folded |
|---|---|---|
| rows in the window | 2957 | 5751 |
| VWAP | 0.1801337497 | 0.1801307954 |
| high | 0.1806435916 | **0.1818181818** |
| low | 0.1796178598 | **0.1794054551** |

The served window held **51.4%** of the market's prints — and a biased
51.4%, because the SDEX decoder sets `base = soldAsset`, so the visible
half is the sell side. The mean barely moves on a tight market (−0.0016%),
which is exactly why this survived: a VWAP over half a two-sided book
still looks plausible. The **extremes do not** survive. The hour's true
high was recorded the other way round, so the served high understated it
by **0.65%**, and the served low was 0.12% too high. Both on a bar that
never looked empty.

That also closes a live violation of C1-024 that the parity tests cannot
see: the fiat point path reads raw trades (one direction, until now)
while the series reads `Store.OHLCSeries` (both directions, via its
`norm` CTE). Point and series were computing over different populations
again. Their pinned parity test passes because its reader stub is
pair-keyed and sits above the store.

### Cost, from plans rather than estimates

`EXPLAIN` on r1, plan only — no `ANALYZE`, no writes.

| read | today | folded | ratio |
|---|---|---|---|
| 1 h window, populated pair | `Limit (cost=25.70..25.75)` | `Limit (cost=43.35..44.14)` | 1.72x |
| 24 h window, populated pair | `Limit (cost=1.72..1169.12)` | `Limit (cost=3.44..1191.84)` | **1.02x** |
| empty pair, 2021→now | `Limit (cost=0.56..147.06)` | `Limit (cost=1.14..184.61)` | 1.26x |
| FX snap, one row | `Limit (cost=0.56..4.46)` | `Limit (cost=1.14..5.05)` | 1.13x |

The shape that matters is what sits under the `Limit`: a **`Merge
Append`** over two independently limited arms, each keeping its own
`Index Scan using …_trades_pair_ts_idx` under the `ChunkAppend`, and
each keeping its `ColumnarScan` + compressed index on every compressed
chunk in the wide case. Nothing bitmaps the two directions together and
nothing sorts them as one set.

That is why the cost is not 2x. Startup cost doubles (both arms must
open), but total cost barely moves on the reads that were expensive,
because `Merge Append` pulls from the arms incrementally and each arm
stops early at its own limit. The expensive case gets cheaper in
relative terms, not dearer. All four stay far inside the unchanged 8s
handler ceilings.

---

## 6. The class guard

`TestCAGGPairReadsFoldBothDirections` parsed every SQL literal in the
package and failed any pair-bound **CAGG** read that filtered one
orientation. Its scope now includes the `trades` hypertable, which is
where all of those buckets come from.

It could not widen before this change: it would have gone red on
`TradesInRange` (correctly) and on `TradesInRangeAfter` (wrongly), and
the only way to green the second was an entry meaning "known blind",
which is what the exempt list exists to refuse.

There is now exactly **one** exemption, and it does not say a read is
blind. It says the read is folded by its caller under a keyset cursor,
and names the caller.

The widened scan turned up a third pair-bound read of `trades`:
`Store.FXQuoteAtOrBefore`'s legacy fallback arm, on the triangulation
money path. It serves a price, so by the guard's own rule it is not
exemptible — and an FX quote recorded `USD/EUR` answers a `EUR/USD`
question by the same leg swap, at full precision. It is folded rather
than excused. The arm holds no rows on r1 today (checked 2026-09-05,
both FX sources, 400 days, zero rows — the path moved to `fx_quotes`),
so this widens a dormant read, not a hot one.

---

## 7. What this deliberately does not cover

### 7.1 A pair and its flip in one merge set

Folding direction into the store changed what a *list* of pairs means to
a caller that merges one. Before, `A/B` and `B/A` were two reads
returning disjoint rows; now they are one read returning the same rows,
differing only in the orientation the answer arrives in.

This is reachable, not theoretical. Both legs alias, so
`/v1/price/tip?asset=native&quote=crypto:XLM` makes `tipMergePairs` emit
`native/crypto:XLM` **and** `crypto:XLM/native` in the same merged set,
plus both SAC combinations in the held-back set — and `tipWindowVWAP`
concatenates the set into one `Σquote/Σbase`. Every trade would be
counted twice, in two orientations.

`distinctMarkets` drops any pair whose flip already appeared, keeping
the first spelling and its position, so the SAC-last priority every walk
depends on still decides which orientation answers. It is applied at the
two places that merge: `tipMergePairs` and `usdPeggedConstituents`. The
second holds today by accident rather than construction — the peg
expansion emits classic quote spellings only — which is precisely the
accident §7.3 would remove.

### 7.2 The alias question is not the direction question

Row 1.14 asks for the fiat point path to be alias-complete on its quote
leg, and row 1.15 for the fiat series to reach a SAC-quoted Soroban
pool. Both were expected to be answered by "whatever rule the aggregate
fold settles on".

They are not, and the reason is a one-line distinction (§7.5 built them
on the rule this section names, not on the fold):

> **Folding a direction adds no market. Folding an alias adds one.**

`(A,B)` and `(B,A)` are two spellings of the *same rows*, so merging
them is complete rather than dangerous, and the only way to get it wrong
is arithmetic. `<AQUA SAC>/<USDC SAC>` and `AQUA/USDC-GA5Z…` are two
*different* markets that happen to share an asset family — one a
few-hundred-dollar Soroban pool, the other an SDEX book. Merging those
is not a completeness fix, it is a liquidity decision, and the measured
result of getting it wrong is on the record: a 100-print book over
6,000,000 base units beside a two-print pool serves `n=102`,
`v_base=6,000,020`, **high 0.50, low 0.01** — two prints setting a bar's
extremes against six million units of book volume.

So the mechanism the alias rows need is not the fold. It is the
**merge/last split** already shipped for `/v1/price/tip`
(`tipMergePairs`): the established spellings merge, the SAC forms go in
a set that is read only when every established read has missed, so a
thin pool answers where the alternative is no answer at all and never
sits beside book data in one population. That is what §7.5 built, with
"has missed" resolved per BUCKET on the series and per WINDOW on the
point path — one rule at each path's own grain.

### 7.3 Row 1.14 — the fiat point path

The mechanism is known and it fits: on a point path there is one window
and one aggregate, so a set-level first-hit across all established
spellings of all families is complete. Row 1.15's requirement (b)
— whether suppressing a family's *later buckets* is acceptable — has no
analogue here, because there are no buckets to suppress.

It was **not implemented** for one reason that had nothing to do with
the mechanism: `usdPeggedConstituents` feeds the point path and the
series path from one list, and `TestFiatVWAPPointMatchesSeries` /
`TestFiatSingleBarOHLCMatchesSeriesExtremes` pin point and series to the
same constituent population by comparing price, both volumes and both
extremes. Widening one side alone turns that pin red. The pin is the
C1-024 invariant, and it is right: two surfaces answering one question
from two populations is the defect this path was rebuilt to remove.

So 1.14 cannot land before 1.15. Closing it alone would trade a `404`
for a fresh divergence, which is a worse bargain than the one on the
table. **Both landed together in §7.5**, from one split of the
constituent list, which is what keeps the two surfaces on one
population.

### 7.4 Row 1.15 — the fiat series, and why not a sixth attempt

The launch plan records two candidate shapes measured and rejected, and
three things a close needs together: (a) a first-hit rule across all
established spellings of all families, (b) a decision on suppressing a
family's later buckets, (c) a guarantee that a thin pool never sets a
bar's high, low, count or volume beside book data.

Three of the four pieces are now in hand that were not before:

1. **(a) and (c) have shipped elsewhere.** `usdPegProxyQuotes`'s
   classic-pass-then-SAC-pass is (a) at set level; `tipMergePairs`'s
   merge/last split is (c). Both landed after the two shapes were
   measured.
2. **The sub-cent dust floor is materialised.** Migration 0115's
   `usd_volume >= 0.01` filter on the CAGG extremes was recreated by
   0147 and is live on r1's schema. Requirement (c) is now only about a
   *legitimate-notional* thin pool, a much smaller problem than the one
   the 2026-06/07 lineage was fighting.
3. **The direction fold gives a costed precedent** for widening a
   pair-bound read without a plan regression (§5).

The fourth piece was recorded here as **missing and disqualifying**, and
that was wrong on the facts. It is now fixed, so this section records
both the correction and what it leaves.

The claim was: `ohlcSeriesFiatCombined` combines CAGG **bars**, and a
CAGG bar has no `source` column, so `NormalizeAmountScale` — the thing
that stops a finer-scaled venue out-weighting a coarser one by 10x per
decimal — cannot run on this path.

**A CAGG bar has carried its sources since migration 0002.** Every
`prices_*` view selects `array_agg(DISTINCT source) AS sources`, and
0147 recreated all seven with it. What had no source was the Go struct:
`Store.OHLCSeries` never put the column in its SELECT list, so
`timescale.OHLCBar` and `v1.OHLCSeriesBar` arrived unattributed. The
blocker was a missing projection, not a missing column, and closing it
needed no migration and no re-materialisation.

**The defect was also live, not latent.** It was recorded on the
assumption that the shipped constituent set was single-scale so nothing
was visibly wrong until a SAC-quoted pool was admitted. Measured on r1
2026-09-05, the `native/fiat:USD` combine already merges three
constituents in the same buckets — `native/<USDC classic>` (sdex, 7dp),
`crypto:XLM/crypto:USDT` (binance, 8dp) and `crypto:XLM/fiat:USD`
(bitstamp/coinbase/kraken, 8dp). On the 02:00Z 1h bar the sdex leg's
135,713.79 XLM entered the combine as 13,571.38, the served `v_base`
understated the market by **3.41%**, and that leg carried 0.39% of the
open/close weight against a real 3.79%. `fiatCombinedTrades` has
normalised the same constituent set since CS-040, so point and series
were weighting one population two ways — a live C1-024 violation, and
one the pinned parity tests could not see because every trade in their
fixture is stamped `sdex`.

**What shipped.** Both bar readers carry `sources`, folded across BOTH
stored directions. In `OHLCSeries` the fold is a concatenation of the
two directions' arrays taken with `max(…) FILTER`, which is exact rather
than clever: the CAGG groups on `(bucket, base_asset, quote_asset)` and
the read admits exactly two such values, so a bucket holds at most one
row per direction. `OHLCSeriesReBucketed` needs that fold twice — once
within a source bucket, then a real N-way union across the buckets an
out_bucket spans — so only the second gets its own grouping and join.
The combine then lifts every bar to the maximum scale in the response,
an exact integer multiply by `10^(max−scale) ≥ 1`, no division
(ADR-0003). A response whose bars share one scale is byte-identical to
before, which is every non-fiat quote and every single-venue-class
window; on live data both readers return every numeric column unchanged.
A bar that names no venue is left where it is rather than assigned the
registry fallback of 8, because a plausible default here is exactly the
mis-weight being removed.

Cost, from `EXPLAIN` on r1 (plan only, 30 days, flagship pair):
`OHLCSeries` 29911 → 30076 (**1.005x**), `OHLCSeriesReBucketed` 1h → 4h
31003 → 38892 (1.25x). The shapes that were rejected are recorded with
their numbers: a second CTE joined back on bucket costs 1.34x on
`OHLCSeries`, and unioning at the pre-fold row grain — which forces the
row-level CTE to materialise — costs 2.08x on the re-bucketed read. A
single-pass shape using `WITH ORDINALITY` plus a `FILTER` on every sum
measured 1.09x and was refused: its failure mode is silently wrong sums
on a money path, and it cannot be proved without a database.

**Conclusion.** The defect one layer beneath rows 1.14 and 1.15 is gone,
and a widened constituent set can no longer mix scales silently. What
those rows still needed — (a), (b), (c) — is §7.5, which built them.

---

## 7.5 The fiat quote leg, widened — rows 1.15 and 1.14

### The measurement first

Two candidate shapes had been measured and rejected on fixtures. This
one was measured on r1 before it was designed, read-only and bounded,
over `prices_1d` (the rung the series and the floor probe both read).

**Does the shape occur?** Over 365 days to 2026-09-05, **132 markets**
carry the declared peg's SAC wrapper
(`CCW67TSZ…`, the SAC of `USDC-GA5Z…`) on one leg, holding **1,916,996
prints** across **67 distinct counterparties**. Splitting those
counterparties by whether ANY spelling the fiat combine reads — the
direct fiat pair, the five abstract stablecoin backers, the declared
classic peg, each under every base alias, in both stored directions —
holds a single bucket for them:

| | assets | prints | USD volume |
|---|---:|---:|---:|
| reachable today | 24 | 1,656,163 | $419,184,137.56 |
| **SAC-only USD depth** | **43** | **260,833** | **$14,630,761.46** |

The 43 are the row's own case, and they are not dust: the largest
carries **$6,375,518.23** over 129,925 prints across a full unbroken
year, and five carry six figures or more. Every one of them served
`intervals: []` on `/v1/ohlc` and `404` on `/v1/vwap`, while `/v1/chart`
— whose proxy walk already runs a classic pass then a SAC pass — charted
them. So the shape occurs, at scale, and the three surfaces disagreed
about whether these assets have a dollar price at all.

**What a widened set does to a real bar.** Taking the 24 reachable
assets — the ones with both a book and a pool — and asking, per daily
bucket, what an unconditional merge would do to the book's extremes:
the worst cases are not close. On **2026-06-02**, `GQX-GD7TC72O…`'s
book carried **660 prints and $140** with a high of
**9.5396055089328007**; the pool carried **one print worth $0.60** —
0.43% of the day's dollar volume — at **13.0995677490335234**. Merged,
that single print sets the bar's high: **+37.32%**. Three days earlier
the same pair moves **+37.52%** on three prints worth $1.03. This is
the shape the launch plan recorded from a fixture (`n=102`, high 0.50,
low 0.01 against six million units of book volume), found in production
data.

Note what it is *not*: $0.60 clears migration 0115's `usd_volume >=
0.01` floor on the CAGG extremes by sixty times. Requirement (c) really
had narrowed to a legitimate-notional thin pool, exactly as §7.4 said,
and the dust floor does not touch it.

### (b) The decision: per bucket, never per response

> **A held-back spelling is suppressed for a bucket an established
> spelling answered, and for no other bucket.**

The alternative on the table was a first-hit evaluated once per
response: if any established spelling answers anywhere in the window,
the held-back set is never read. It is simpler, it is what the point
path's own shape suggests, and it is wrong for one reason that is a
property rather than a preference:

**A first-hit chosen once per response makes the constituent set a
function of the WINDOW.** The same day then renders one way inside a
window the book also covers and another way inside a window it does
not — one unchanged database, two answers, and no way for a caller to
tell which they hold. Resolving per bucket depends only on the bucket,
so a bar is identical in every window containing it. That is pinned as
a test rather than asserted.

And the cost of the alternative is not hypothetical. Across those same
24 assets, **3,356 daily buckets are pool-only** against 3,032 shared —
the pool trades on more days than the book does — carrying **671,712
prints and $175,962,608.19**. A per-response first-hit reports every one
of them as quiet. That is the second fault the launch plan predicted for
candidate shape B ("one bar is served and 29 days of real prints are
suppressed"), and it is larger in production than the gap being closed.

The rule keeps the property the row asked for: the series stays
consistent with the live aggregator's own source set, because every
bucket that set can answer is answered by it alone, byte for byte. The
held-back spellings only reach buckets where the alternative is nothing.

**The flagship pair moves, and that is worth stating plainly.** In
`prices_1d` on r1, `native/USDC-GA5Z…` holds 176 daily buckets from
2026-03-12, while `<XLM SAC>/<USDC SAC>` holds **874 from 2024-03-12**.
So `native/fiat:USD`'s daily series gains the buckets the classic book's
aggregate does not hold. That pool is not thin — $371M over 365 days —
so this is depth rather than noise, and `/v1/chart` has served the same
market for months. But the bars are Soroban-AMM-sourced and the wire
does not say so: `OHLCSeriesBar.Sources` is `json:"-"`. That opacity is
pre-existing — the combine has always merged SDEX, four CEXs and the FX
pollers into one unattributed bar — and this widens it rather than
introducing it. Putting `sources` on the wire is a spec change and is
not made here.

### (a) and (c) are one change

They ship together because widening reach without the bucket gate is
precisely how a bar acquires a wrong high — the +37.32% above is that
mistake, measured.

`Server.usdPeggedConstituentSets` splits the constituents in two.
`established` is the peg expansion as it always was, each quote in the
priority-first spelling. `heldBack` is the remaining canonical form of
each of those quotes — a declared peg's SAC wrapper. Two passes, not one
pass per family: this is `usdPegProxyQuotes`'s classic-pass-then-SAC-pass
lifted to the constituent grain, so no family's pool is consulted before
another family's book. The two sets are deduplicated by MARKET against
each other, so a pair reachable under both (base and quote in one alias
family) stays with the set read first.

`ohlcSeriesFiatCombined` then reads `established`, snapshots which
buckets it answered, and reads `heldBack` admitting only bars whose
bucket is not in that snapshot. The snapshot is taken before the second
pass so two held-back constituents sharing an unanswered bucket still
merge with each other.

(c) falls out of that as an absolute rather than an arithmetic
guarantee: a held-back bar in an answered bucket is not down-weighted,
not banded, not filtered — it is not in the bucket. The per-bucket max,
min, count and sums the launch plan named as the fault cannot see it,
because there is nothing for them to see. A rejected bar also
contributes no scale, so it cannot move the units the served bars are
rendered in.

### The lift target is per BUCKET, not per response

The scale fix that unblocked this row lifted every bar to the maximum
scale **in the response**. That cannot survive a window-dependent
constituent set: whether a held-back bar is admitted depends on the
window, an admitted bar contributes a scale, and so the presence of a
pool-only day changed the lift applied to the BOOK's bar on a different
day. Measured on the first build of this row: a 7dp book day served
`v_base` 1000000 asked for alone and **10000000** asked for beside an
8dp pool day — the book's own bucket, ten times over, from one unchanged
database. Which is precisely the property §7.5 had just argued was the
reason to resolve admission per bucket.

**It was not caused by the widening.** The same shape reproduces with no
held-back set in play at all: two ESTABLISHED constituents at two venue
scales on two days — `native/<USDC classic>` (sdex, 7dp) and
`crypto:XLM/crypto:USDT` (binance, 8dp), which is r1's actual
constituent set — give the identical 1000000 → 10000000 split on the
pre-widening tree. A response-wide maximum was always a way for a window
to change a served volume; the widening only added a second route to it.

So the lift target is the maximum scale **within a bucket**. Every lift
stays an exact integer multiply by `10^(common−scale) ≥ 1` (ADR-0003),
and a bar now depends on nothing outside its own bucket.

What that costs is that two buckets of one response can be summed at
different scales, where the response-wide maximum made them uniform.
Three reasons that is the right trade:

1. The uniformity held only WITHIN one response. A caller stitching two
   windows — a paged chart, an incremental refresh — already received
   the same bucket in two different units, with nothing on the wire to
   say so. Per-bucket is stable across every request, which is the form
   a caller can rely on.
2. It is what the wire already promises. `v_base` is documented as
   "Σ base_amount over the bucket" — a per-bucket sum. The response-wide
   maximum was the thing exceeding the contract.
3. Prices are untouched either way. A lift multiplies both legs of a
   bar, so `v_quote/v_base`, the open, close, high and low are
   invariant; only the absolute magnitude of the two volumes moves.

**Disclosed rather than buried:** on r1 this changes served volumes for
sdex-only buckets inside a response that also contains CEX buckets —
`native/fiat:USD` before 2026-05-05 has on-chain-only days beside later
three-venue days — which previously rendered lifted to 8dp and now
render at their own 7dp sum. Prices, extremes and counts are unchanged.

### The point path (row 1.14) closes in the same shape — at a stated grain

The first build of this row gated the point path on the whole window:
read the held-back set only when the established set returned nothing,
on the reasoning that a point window is its own bucket. **That is true
only when the window IS one bucket**, and every parity fixture in the
tree was exactly one bucket, so nothing saw it fail. Over a two-hour
window with the book trading in the first hour and the pool in the
second, the series served two bars carrying both while `/v1/vwap` served
the book alone — `n=1 v_base=1000` against `n=2 v_base=1500`. Two served
money surfaces answering one window from two populations: the C1-024
defect this path exists to remove, reintroduced on the surface being
widened, and `/v1/ohlc` single-bar reported a high of 0.20 where the
same window's series reported 0.80.

The gate is therefore per bucket here too, at `fiatPointGateInterval` —
`1m`, the finest interval the series accepts and the finest rung the
deployment materialises. The point path reads raw trades, so it buckets
them itself at no extra read cost; the `answered` set is snapshotted
from the established rows before any held-back row joins them, so two
held-back constituents sharing an empty bucket still merge with each
other, exactly as they do on the series side.

**The claim this supports, stated precisely.** Point equals the series
**exactly at `interval=1m`**. It does not equal the series at every
interval, and no rule could: per-bucket admission is a function of what
a bucket IS, so a coarser bucket is likelier to hold an established
print and therefore suppresses more. A 1h series and a 1d series over
one window disagree with each other for the same reason. That is the
caller's question changing, not the population splitting — both surfaces
run one rule over one constituent split. Pinning the finest grain is
what keeps the statement checkable rather than aspirational, and
`TestFiatPointEqualsTheFinestSeriesExactly` pins it.

Both surfaces now answer the 43 assets, and `/v1/chart` already did, so
all three read one population.

### Cost

The query shape is unchanged — `Store.OHLCSeries` and
`Store.TradesInRange` are untouched, so there is no plan to re-cost. The
added cost is extra reads of the same shape against different pairs, and
it is bounded by (base alias count) × (declared classic pegs that have a
SAC wrapper): on r1 that is 3 × 1. Measured on the fixtures that mirror
r1's registry, `native/fiat:USD` goes from **21 to 24** constituent
reads (1.14x) and a single-wrapper `AQUA/fiat:USD` from **7 to 8**. Each
goes through the cached `HistoryReader`, so a repeat request pays for
none of them. An operator who declares no SAC wrapper has an empty
held-back set and a byte-identical response.

### What is pinned

| Claim | Test |
|---|---|
| A contract-quoted pool as the only venue is served (series) | `TestFiatSeries_SACQuotedPoolIsTheOnlyVenueIsServed` |
| …and on the point path | `TestFiatPoint_SACQuotedPoolIsTheOnlyVenueIsServed` |
| A thin pool sets no high, low, count or volume beside a book | `TestFiatSeries_ThinSACPoolNeverSetsABarBesideBookData` |
| The pool fills only the buckets the book cannot answer | `TestFiatSeries_PoolFillsOnlyTheBucketsTheBookCannotAnswer` |
| A bucket renders identically in every window containing it | `TestFiatSeries_ABucketRendersTheSameInEveryWindow` |
| Point == series over constituents at two venue scales | `TestFiatPointMatchesSeries_AcrossVenueScales` |
| Point == series on the held-back arm | `TestFiatPointMatchesSeries_OnAPoolOnlyVenue` |
| Point == series over a window of MORE than one bucket | `TestFiatPointMatchesSeries_OverAMultiBucketWindow` |
| Point == the finest series exactly | `TestFiatPointEqualsTheFinestSeriesExactly` |
| A pool print in a bucket the book answered is dropped on the point path too | `TestFiatPoint_PoolInAnAnsweredBucketIsSuppressed` |
| A bucket of purely established, mixed-scale constituents is window-invariant | `TestFiatSeries_EstablishedMixedScaleBucketIsWindowInvariant` |
| Every established spelling is read before any held-back one | `assertSACQuotedSeriesReadLast` |
| The floor spans exactly what the combine reads | `TestOHLCSeries_FiatProbeSpansWhatTheCombineReads` |

The last two rows of the previous behaviour were pinned too, and moved
deliberately: `TestOHLCSeries_FiatQuoteBookOutranksSACQuotedPool` now
expects the book's day-1 bar **and** the pool's day-2 bar, and
`TestOHLCSeries_SACQuotedOnlyDepthIsEmptyAndUnclaimed` became
`TestOHLCSeries_SACQuotedOnlyDepthIsServed`.

**Two fixtures here were vacuous, and both are fixed.** The parity
fixture that was already in the tree stamps every trade `sdex`, so it
holds the cross-surface invariant with one scale and every lift factor
at 10^0. `TestFiatPointMatchesSeries_AcrossVenueScales` carries three
constituents across two scales; with the series-side lift removed it
reports `base_volume point 100000 != series 37000`, while the
single-source fixture beside it stays green.

The window-invariance pin had the same defect from the other direction:
`mkSeriesBar` leaves the CAGG's `sources` column empty, so every fixture
bar was unknown-scale and the pin passed with the scale machinery
deleted outright — asserting precisely the property that was broken.
Its bars now carry real venue names, and it checks EVERY bucket of the
response against itself fetched alone, in both directions (coarse book
with a fine pool, and the mirror).

---

## 8. Where this lives

| Concern | Location |
|---|---|
| The fold | `internal/storage/timescale/trades.go` — `Store.TradesInRange`, `Store.FXQuoteAtOrBefore` |
| The per-row rule | `internal/storage/timescale/trades.go` — `orientTradeTo`, `scanTradeOriented` |
| Behaviour + aggregate shapes | `internal/storage/timescale/trades_in_range_direction_test.go` |
| Query shape | `internal/storage/timescale/trades_direction_test.go` — `TestRawTradeReadsSpanBothStoredDirections` |
| Class guard | `internal/storage/timescale/pair_direction_guard_test.go` |
| Merge-set dedupe | `internal/api/v1/price.go` — `distinctMarkets`; applied in `price_tip.go` and `ohlc_fiat_combine.go` |
| Dedupe tests | `internal/api/v1/market_dedupe_internal_test.go` |
| The bucket-level twin | `internal/storage/timescale/aggregates.go` — `Store.OHLCSeries`'s `norm` CTE |
| The quote-leg widening (§7.5) | `internal/api/v1/ohlc_fiat_combine.go` — `Server.usdPeggedConstituentSets`, `Server.ohlcSeriesFiatCombined`, `Server.fiatCombinedTrades` |
| Its behaviour | `internal/api/v1/fiat_series_sac_reach_test.go` |
| Its floor | `internal/api/v1/coverage_floor.go` — `Server.ohlcCoverageSet`; `internal/api/v1/coverage_floor_test.go` |
