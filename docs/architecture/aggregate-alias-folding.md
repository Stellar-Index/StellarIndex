---
title: Folding a market's two spellings into an aggregate
last_verified: 2026-09-05
status: partially implemented
---

# Folding a market's two spellings into an aggregate

**Last verified:** 2026-09-05
**Status:** The direction fold is implemented (launch-plan row 1.16,
aggregate half). The alias widening on the fiat quote leg is designed
here and deliberately **not** implemented — rows 1.14 and 1.15 stay
open, and §7 says exactly what blocks them.
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
the two must never be summed into one bar. That is not implemented, and
§7 gives the mechanical reason the series arm cannot take it yet.

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

They are not, and the reason is a one-line distinction:

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
sits beside book data in one population.

### 7.3 Row 1.14 — the fiat point path

The mechanism is known and it fits: on a point path there is one window
and one aggregate, so a set-level first-hit across all established
spellings of all families is complete. Row 1.15's requirement (b)
— whether suppressing a family's *later buckets* is acceptable — has no
analogue here, because there are no buckets to suppress.

It is **not implemented**, for one reason that has nothing to do with
the mechanism: `usdPeggedConstituents` feeds the point path and the
series path from one list, and `TestFiatVWAPPointMatchesSeries` /
`TestFiatSingleBarOHLCMatchesSeriesExtremes` pin point and series to the
same constituent population by comparing price, both volumes and both
extremes. Widening one side alone turns that pin red. The pin is the
C1-024 invariant, and it is right: two surfaces answering one question
from two populations is the defect this path was rebuilt to remove.

So 1.14 cannot land before 1.15. Closing it alone would trade a `404`
for a fresh divergence, which is a worse bargain than the one on the
table.

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

And one piece is **missing**, mechanically, and it is disqualifying.

`ohlcSeriesFiatCombined` combines CAGG **bars**, and a CAGG bar has no
`source` column. `NormalizeAmountScale` — the thing that stops a
finer-scaled venue out-weighting a coarser one by 10x per decimal —
resolves its factor from a trade's source, so it cannot run on this
path. That is already a recorded open defect on the series combine
("the same root cause via a different mechanism: cross-scale CAGG bars,
no per-trade source"). A SAC-quoted Soroban pool is a 7-decimal venue;
the CEX bars it would be combined with are 8-decimal. Widening the
series to reach the pool therefore introduces a new cross-scale mix into
the one path that has no way to correct it — which also means shape A's
measured `n=102 / v_base=6,000,020` is, if anything, understated.

The point path does not have this problem. `fiatCombinedTrades` merges
raw trades, each carrying its own `Source`, and already calls
`NormalizeAmountScale`.

**Conclusion.** Row 1.15 is not blocked on a missing idea. It is blocked
on a defect one layer beneath it: the series combine cannot weight
across scales. Fix that first — give the series path a per-source scale,
or a bar-level equivalent — and then (a)+(c) settle the rest and (b) can
be decided on its merits. Starting a sixth attempt on the current
footing would repeat the lineage's pattern exactly: each of the five
prior changes in this family fixed its predecessor's defect and
introduced its own.

Rows 1.14 and 1.15 stay open, in that order of dependency, and 1.14 is
gated on 1.15 rather than the other way round.

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
