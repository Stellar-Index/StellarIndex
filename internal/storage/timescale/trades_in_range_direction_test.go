// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"math/big"
	"regexp"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// [Store.TradesInRange] is the read behind an AGGREGATE — /v1/vwap,
// /v1/twap, single-bar /v1/ohlc, /v1/price/tip and the aggregator
// orchestrator — which is why it was held back when the two
// mechanical raw-trade reads were folded. A mean and an extreme are
// not relabellings: fold a row into them the wrong way and the answer
// is a plausible number rather than an empty one.
//
// The rule turns out to be the SAME per-row leg swap, and these tests
// are the reason to believe it. Every aggregate downstream is defined
// on the two integer leg amounts and never on a stored price:
//
//   - aggregate.VWAP        Σ(QuoteAmount) / Σ(BaseAmount)
//   - aggregate.ComputeOHLC per row, price = QuoteAmount/BaseAmount as
//     an exact big.Rat; High = max, Low = min, Open/Close by slice order
//   - aggregate.TWAP        Σ(price·Δt) / Σ(Δt), Δt from Timestamp
//
// (restated here rather than imported: this package must not depend on
// internal/aggregate — see asset_price_snapshot_test.go for the same
// restatement of a constant.)
//
// So swapping a flipped row's two legs does two things at once. It
// inverts the price exactly, with no division. And it re-weights the
// mean, because the weight is the row's own base leg and a flipped
// row's base leg in the REQUESTED orientation is its stored QUOTE
// amount. There is no separate re-weighting step to get wrong — but
// there is a fold that skips it, and the fixture below is chosen so
// that fold reports a wrong high instead of an empty bar.

// The market: AQUA priced in USDC. Four prints, deliberately at four
// different prices, two recorded each way round.
//
// Every base leg is 100 AQUA and every quote leg differs, so the
// requested-orientation weights are uniform and the arithmetic below
// stays checkable by eye.
//
//	stored AQUA/USDC  100 AQUA / 20 USDC   → 0.20
//	stored AQUA/USDC  100 AQUA / 18 USDC   → 0.18
//	stored USDC/AQUA   25 USDC / 100 AQUA  → 0.25  ← the market's HIGH
//	stored USDC/AQUA   10 USDC / 100 AQUA  → 0.10  ← the market's LOW
//
// Read whole: Σbase = 400 AQUA, Σquote = 73 USDC, VWAP = 0.1825,
// high 0.25, low 0.10, n = 4.
const (
	aggAQUA100 = "1000000000" // 100 AQUA, 7dp
	aggUSDC20  = "200000000"  // 20 USDC
	aggUSDC18  = "180000000"  // 18 USDC
	aggUSDC25  = "250000000"  // 25 USDC
	aggUSDC10  = "100000000"  // 10 USDC
)

// aggWindow is the [from, to) the fixture rows fall inside.
var aggWindow = struct{ from, to time.Time }{
	from: time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC),
	to:   time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC),
}

// aggScriptedRows renders the four fixture prints in the order the
// two-armed query returns them: newest first, since the read orders
// DESC and reverses to ascending in Go.
func aggScriptedRows() [][]driver.Value {
	base := aggWindow.from
	return [][]driver.Value{
		// t+30m — stored the other way round, the market's LOW.
		storedTradeRow("sdex", 58_000_004, dirTxB, 0, base.Add(30*time.Minute),
			dirUSDC, dirAQUA, aggUSDC10, aggAQUA100),
		// t+20m — stored the other way round, the market's HIGH.
		storedTradeRow("sdex", 58_000_003, dirTxA, 0, base.Add(20*time.Minute),
			dirUSDC, dirAQUA, aggUSDC25, aggAQUA100),
		// t+10m — stored as asked.
		storedTradeRow("sdex", 58_000_002, dirTxB, 0, base.Add(10*time.Minute),
			dirAQUA, dirUSDC, aggAQUA100, aggUSDC18),
		// t+00m — stored as asked.
		storedTradeRow("sdex", 58_000_001, dirTxA, 0, base,
			dirAQUA, dirUSDC, aggAQUA100, aggUSDC20),
	}
}

// aggSumLegs returns (Σ QuoteAmount, Σ BaseAmount) over the slice —
// aggregate.VWAP's numerator and denominator, and TotalQuoteVolume /
// TotalBaseVolume.
func aggSumLegs(trades []canonical.Trade) (sumQuote, sumBase *big.Int) {
	sumQuote, sumBase = new(big.Int), new(big.Int)
	for i := range trades {
		sumQuote.Add(sumQuote, trades[i].QuoteAmount.BigInt())
		sumBase.Add(sumBase, trades[i].BaseAmount.BigInt())
	}
	return sumQuote, sumBase
}

// aggExtremes returns (high, low) over the slice, each row's price
// taken as QuoteAmount/BaseAmount exactly — aggregate.ComputeOHLC's
// own per-row price and its max/min.
func aggExtremes(t *testing.T, trades []canonical.Trade) (high, low *big.Rat) {
	t.Helper()
	if len(trades) == 0 {
		t.Fatal("no trades to take extremes over")
	}
	for i := range trades {
		p := new(big.Rat).SetFrac(trades[i].QuoteAmount.BigInt(), trades[i].BaseAmount.BigInt())
		if high == nil || p.Cmp(high) > 0 {
			high = p
		}
		if low == nil || p.Cmp(low) < 0 {
			low = p
		}
	}
	return high, low
}

func aggRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("cannot parse %q as a rational", s)
	}
	return r
}

// TestTradesInRange_AggregateOverBothStoredDirections is the row-1.16
// aggregate half, stated as the three aggregate shapes it has to be
// right for at once.
//
// Unfolded, the read returns two of the four prints and the window
// answers 0.19 / high 0.20 / low 0.18 on 200 AQUA of volume — a bar
// that looks perfectly well-formed and is missing half its market.
// Folded but merely RELABELLED — flipped rows appended with their
// stored legs left alone — the same window answers a high of 10, the
// stored AQUA-per-USDC price read as if it were USDC-per-AQUA. Both
// failures are silent; the second is 40x wrong.
func TestTradesInRange_AggregateOverBothStoredDirections(t *testing.T) {
	pair := dirAQUAUSDCPair(t)

	store, _ := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: aggScriptedRows(),
	})

	got, err := store.TradesInRange(context.Background(), pair, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("returned %d trades, want 4 — the window holds two prints in "+
			"each stored direction and an aggregate is computed over all of them", len(got))
	}

	// (1) Every row arrives in the requested orientation. A relabelled
	// row would fail here before any aggregate is taken, which is the
	// point: the aggregates below are only meaningful once this holds.
	for i := range got {
		if got[i].Pair.Base.String() != dirAQUA || got[i].Pair.Quote.String() != dirUSDC {
			t.Errorf("row %d returned %s/%s, want the REQUESTED orientation %s/%s",
				i, got[i].Pair.Base, got[i].Pair.Quote, dirAQUA, dirUSDC)
		}
		if got[i].BaseAmount.String() != aggAQUA100 {
			t.Errorf("row %d base_amount = %s, want %s — every fixture print is "+
				"100 AQUA in the requested orientation, so a flipped row whose "+
				"legs were not swapped shows up here",
				i, got[i].BaseAmount, aggAQUA100)
		}
	}

	// (2) The volume-weighted mean. Σquote/Σbase over the folded set is
	// 73/400 = 0.1825 exactly. The weight is what a fold gets wrong: a
	// flipped row's weight in the requested base is its STORED QUOTE
	// leg, so the base volume is 400 AQUA and not the 235 that summing
	// the stored base legs would give.
	sumQuote, sumBase := aggSumLegs(got)
	if want := aggRat(t, "4000000000"); new(big.Rat).SetInt(sumBase).Cmp(want) != 0 {
		t.Errorf("Σbase = %s, want %s (4 x 100 AQUA).\n"+
			"The weight in a volume-weighted mean is the row's base leg IN THE "+
			"REQUESTED ORIENTATION. A flipped row contributes its stored QUOTE "+
			"amount here; summing stored base legs instead gives 2350000000 and "+
			"weights the two flipped prints by their USDC size.",
			sumBase, want.RatString())
	}
	vwap := new(big.Rat).SetFrac(sumQuote, sumBase)
	if want := aggRat(t, "0.1825"); vwap.Cmp(want) != 0 {
		t.Errorf("VWAP = %s, want exactly %s.\n"+
			"0.19 is the answer over the two same-direction prints alone; "+
			"anything near 1 is the two flipped prints summed with their legs "+
			"where the database left them.",
			vwap.FloatString(10), want.FloatString(10))
	}

	// (3) The extremes. Both the high and the low of this window are set
	// by a print stored the other way round — the shape that makes a
	// naive fold worse than no fold, because max() and min() over
	// un-re-expressed rows return a number rather than nothing.
	high, low := aggExtremes(t, got)
	if want := aggRat(t, "0.25"); high.Cmp(want) != 0 {
		t.Errorf("high = %s, want exactly %s.\n"+
			"0.2 is the high of the same-direction prints alone; 10 is the "+
			"flipped print's STORED price (AQUA per USDC) compared as though it "+
			"were USDC per AQUA. An inverted price turns a maximum into a "+
			"minimum, so the re-expression has to happen BEFORE the comparison, "+
			"never after.",
			high.FloatString(10), want.FloatString(10))
	}
	if want := aggRat(t, "0.10"); low.Cmp(want) != 0 {
		t.Errorf("low = %s, want exactly %s (0.18 is the same-direction low)",
			low.FloatString(10), want.FloatString(10))
	}

	// (4) The extremes bracket the mean, and the mean is strictly
	// inside them. An orientation-mixed population routinely violates
	// this, which is the cheapest possible smoke test for the class.
	if !(low.Cmp(vwap) < 0 && vwap.Cmp(high) < 0) {
		t.Errorf("low %s / VWAP %s / high %s are not ordered — a population "+
			"holding two orientations at once cannot satisfy this",
			low.FloatString(10), vwap.FloatString(10), high.FloatString(10))
	}

	// (5) Chronological order survives the union. aggregate.TWAP and
	// aggregate.ComputeOHLC take their time weights and their
	// open/close from slice position and deliberately do not sort, so
	// an unordered merge silently changes both.
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp.Before(got[i-1].Timestamp) {
			t.Fatalf("row %d (%s) precedes row %d (%s) — TradesInRange promises "+
				"ascending close time, and TWAP's Δt weights come straight from it",
				i, got[i].Timestamp, i-1, got[i-1].Timestamp)
		}
	}
}

// TestTradesInRange_FoldedMeanIsReciprocalConsistent is the property
// that distinguishes a re-weighted fold from a re-priced one, without
// reference to any fixture number.
//
// Σq/Σb in one orientation and Σb/Σq in the other are exact
// reciprocals BY CONSTRUCTION once the legs are swapped, because both
// are ratios of the same two totals. A fold that inverted each row's
// price while leaving its weight alone cannot satisfy this for any
// window holding more than one price: it computes Σ(b²/q)/Σb, which is
// a weighted mean of reciprocals, not the reciprocal of a weighted
// mean.
func TestTradesInRange_FoldedMeanIsReciprocalConsistent(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	flipped := pair.Flip()

	forward, _ := newScriptedStore(t, scriptedResult{cols: latestTradeCols, rows: aggScriptedRows()})
	reverse, _ := newScriptedStore(t, scriptedResult{cols: latestTradeCols, rows: aggScriptedRows()})

	fwd, err := forward.TradesInRange(context.Background(), pair, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange(AQUA/USDC): %v", err)
	}
	rev, err := reverse.TradesInRange(context.Background(), flipped, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange(USDC/AQUA): %v", err)
	}
	if len(fwd) != len(rev) {
		t.Fatalf("the same market returned %d rows one way round and %d the other",
			len(fwd), len(rev))
	}

	fq, fb := aggSumLegs(fwd)
	rq, rb := aggSumLegs(rev)
	fwdVWAP := new(big.Rat).SetFrac(fq, fb)
	revVWAP := new(big.Rat).SetFrac(rq, rb)

	product := new(big.Rat).Mul(fwdVWAP, revVWAP)
	if product.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("VWAP(AQUA/USDC) x VWAP(USDC/AQUA) = %s, want exactly 1.\n"+
			"%s and %s are the two answers. They are reciprocals only if the "+
			"fold swapped each flipped row's WEIGHT along with its price; "+
			"inverting the price alone gives a weighted mean of reciprocals.",
			product.RatString(), fwdVWAP.FloatString(10), revVWAP.FloatString(10))
	}

	// The extremes swap roles under the same inversion: the high one way
	// round is the reciprocal of the low the other. This is what
	// Store.OHLCSeries' `norm` CTE has to write out longhand on the
	// bucket side (hi = 1/low_price), and what a per-ROW fold gets for
	// free because a row carries one price rather than an interval.
	fwdHigh, fwdLow := aggExtremes(t, fwd)
	revHigh, revLow := aggExtremes(t, rev)
	if got := new(big.Rat).Mul(fwdHigh, revLow); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("high(AQUA/USDC) x low(USDC/AQUA) = %s, want exactly 1 — "+
			"an inverted price turns a maximum into a minimum", got.RatString())
	}
	if got := new(big.Rat).Mul(fwdLow, revHigh); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("low(AQUA/USDC) x high(USDC/AQUA) = %s, want exactly 1", got.RatString())
	}
}

// TestTradesInRange_LeavesTheRequestedOrientationUntouched is the other
// half of a destructive transform: a window in which nothing is flipped
// must come back byte-for-byte as it always did, or the fold is a
// regression on every market that only ever traded one way.
func TestTradesInRange_LeavesTheRequestedOrientationUntouched(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	ts := aggWindow.from

	store, _ := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{requestedStoredRow("sdex", 58_000_001, dirTxA, ts)},
	})

	got, err := store.TradesInRange(context.Background(), pair, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trades, want 1", len(got))
	}
	wantAQUAUSDCTrade(t, got[0], "TradesInRange")
	if got[0].TxHash != dirTxA || got[0].Ledger != 58_000_001 || got[0].Source != "sdex" {
		t.Errorf("identity changed: source %s ledger %d tx_hash %s",
			got[0].Source, got[0].Ledger, got[0].TxHash)
	}
}

// TestTradesInRange_BindsTheWindowAndLimitOnEveryArm: the two arms are
// only equivalent to the read they replace if BOTH carry the whole
// predicate. An arm missing the window bound would aggregate a
// flipped row from outside [from, to); an arm missing the limit would
// uncap half the scan the ceiling exists to bound.
func TestTradesInRange_BindsTheWindowAndLimitOnEveryArm(t *testing.T) {
	pair := dirAQUAUSDCPair(t)

	store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
	if _, err := store.TradesInRange(context.Background(), pair,
		aggWindow.from, aggWindow.to, 250); err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	stmt := conn.only(t)

	if got := stmt.arg(t, 1); got != dirAQUA {
		t.Errorf("bound $1 = %v, want %s", got, dirAQUA)
	}
	if got := stmt.arg(t, 2); got != dirUSDC {
		t.Errorf("bound $2 = %v, want %s", got, dirUSDC)
	}
	if got := stmt.arg(t, 5); got != 250 {
		t.Errorf("bound $5 = %v, want the caller's limit 250", got)
	}

	for _, want := range []struct {
		what string
		pat  string
		n    int
	}{
		{"the window lower bound `ts >= $3`", `ts\s*>=\s*\$3`, 2},
		{"the window upper bound `ts < $4`", `ts\s*<\s*\$4`, 2},
		// One cut per arm plus the outer cut: without the outer one a
		// populated pair returns up to 2x the limit the ceiling promises.
		{"`LIMIT $5`", `LIMIT\s+\$5`, 3},
	} {
		if n := countSQLMatches(stmt.sql, want.pat); n != want.n {
			t.Errorf("%s appears %d time(s), want %d — every arm carries the "+
				"whole predicate, or the two are not the read they replace:\n%s",
				want.what, n, want.n, stmt.sql)
		}
	}
}

// countSQLMatches counts non-overlapping matches of `pat` in a
// statement. Query-shape assertions in this package are about how MANY
// arms carry a predicate, not merely whether one does.
func countSQLMatches(sql, pat string) int {
	return len(regexp.MustCompile(pat).FindAllString(sql, -1))
}

// TestFXQuoteAtOrBefore_ServesAQuoteRecordedTheOtherWayRound: the
// legacy `trades` arm of the FX snap is on the triangulation money
// path, so it cannot be exempted from the class guard — it serves a
// price. A EUR/USD rate recorded USD/EUR is the exact reciprocal once
// the two legs are swapped.
func TestFXQuoteAtOrBefore_ServesAQuoteRecordedTheOtherWayRound(t *testing.T) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatalf("NewFiatAsset EUR: %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset USD: %v", err)
	}
	pair, err := canonical.NewPair(eur, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	ts := aggWindow.from

	// Stored USD/EUR: 5 USD bought 4 EUR, i.e. 0.8 EUR per USD. The
	// EUR/USD rate the caller asked for is 5/4 = 1.25 exactly.
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"source", "ts", "base_asset", "base_amount", "quote_amount"},
		rows: [][]driver.Value{{"exchangeratesapi", ts, "fiat:USD", "5000000", "4000000"}},
	})

	price, observedAt, source, err := store.FXQuoteAtOrBefore(
		context.Background(), pair, ts.Add(time.Hour), []string{"exchangeratesapi"})
	if err != nil {
		t.Fatalf("FXQuoteAtOrBefore: %v", err)
	}
	if want := big.NewRat(5, 4); price.Cmp(want) != 0 {
		t.Errorf("rate = %s, want exactly %s — a quote recorded USD/EUR answers "+
			"EUR/USD by swapping its legs, which is the reciprocal at full "+
			"precision. %s is the stored rate served without re-expression.",
			price.RatString(), want.RatString(), big.NewRat(4, 5).RatString())
	}
	if !observedAt.Equal(ts) || source != "exchangeratesapi" {
		t.Errorf("observed_at %s / source %q, want %s / exchangeratesapi",
			observedAt, source, ts)
	}
	if n := countSQLMatches(conn.only(t).sql, `source\s*=\s*ANY\(\$4\)`); n != 2 {
		t.Errorf("the FX source filter appears %d time(s), want 2 — once per "+
			"stored direction, or one arm serves any source at all:\n%s",
			n, conn.only(t).sql)
	}
}
