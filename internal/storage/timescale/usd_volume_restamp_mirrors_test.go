// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── the two mirror tiers' decision rules ───────────────────────────────
//
// The scan is pinned by the integration tests (a real hypertable, a real
// compressed chunk). What is pinned HERE is what each tier DECIDES about
// a row it scanned: which rows it owns, what it computes for them, and —
// the part a money column is judged on — which rows it refuses to price
// at all.

// mirrorScan builds a scanned row in the shape the shared SELECT
// produces, for whichever source the tier under test scans.
func mirrorScan(source, base, quote, baseAmt, quoteAmt string, stored *string) restampScanRow {
	return restampScanRow{
		Source:      source,
		Ledger:      62643474,
		TxHash:      "b2c1e0f9a8d7c6b5a4930201f0e1d2c3b4a5968778695a4b3c2d1e0f9a8b7c6d",
		OpIndex:     1,
		TS:          time.Date(2026, 5, 19, 19, 12, 3, 0, time.UTC),
		BaseAsset:   base,
		QuoteAsset:  quote,
		BaseAmount:  baseAmt,
		QuoteAmount: quoteAmt,
		Stored:      stored,
	}
}

// bigIntFromString parses a decimal amount for a fixture trade.
func bigIntFromString(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("unparseable fixture amount %q", s)
	}
	return v
}

// xlmQuoteValuerAt is the xlm-quote tier's valuer over a stub resolver —
// the same closure [Store.PlanXLMQuoteUSDVolumeRestamp] injects.
func xlmQuoteValuerAt(r USDVolumeFXResolver) restampValuer {
	return func(t canonical.Trade) (*string, error) {
		return tradeUSDVolumeViaXLMQuoteAnchorFor(context.Background(), t, r), nil
	}
}

// cexFiatValuerAt is the cex-fx tier's valuer over a stub resolver, with
// the as-of gate left out (it is pinned separately, against a database).
func cexFiatValuerAt(r USDVolumeFXResolver) restampValuer {
	return func(t canonical.Trade) (*string, error) {
		return tradeUSDVolumeViaFiatQuoteFor(context.Background(), t, r), nil
	}
}

// TestXLMQuoteRestampDecide_ValuesTheXLMLegAndIsLockstepWithTheInsertPath
// is the mirror tier's central claim: for a DEX trade whose QUOTE leg is
// XLM, the re-derive writes `quote_amount/1e7 x XLM/USD` — and it writes
// exactly what the insert path's own function produces for the same row,
// because it IS that function, handed the mirrored trade.
func TestXLMQuoteRestampDecide_ValuesTheXLMLegAndIsLockstepWithTheInsertPath(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.14488",
	}}
	// 4.9999996 XLM in the QUOTE leg against a token with no market:
	// 4.9999996 x 0.14488 = 0.72439994, the same arithmetic the xlm-base
	// fixture pins from the other side.
	row := mirrorScan("sdex", xlmBaseTestQuote, "native", "7132667", "49999996", strptr("0.00372265"))

	got, disp, err := restampDecide(row, nil, xlmQuoteTierFor, xlmQuoteValuerAt(resolver), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if disp != xlmBaseWrite {
		t.Fatalf("disposition = %v, want the write disposition", disp)
	}
	const want = "0.72439994"
	if got.Want != want {
		t.Errorf("Want = %q, want %q", got.Want, want)
	}
	// Lockstep: the mirrored row through the BASE-side anchor is the same
	// number, so the two tiers cannot drift apart in the arithmetic.
	quote, err := canonical.NewSorobanAsset(xlmBaseTestQuote)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), quote)
	if err != nil {
		t.Fatal(err)
	}
	mirrored := canonical.Trade{
		Source:      "sdex",
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(bigIntFromString(t, "49999996")),
		QuoteAmount: canonical.NewAmount(bigIntFromString(t, "7132667")),
	}
	viaBase := tradeUSDVolumeViaXLMBaseAnchorFor(context.Background(), mirrored, resolver)
	if viaBase == nil || *viaBase != got.Want {
		t.Errorf("xlm-quote wrote %q, the base-side anchor computes %v for the same economic trade", got.Want, viaBase)
	}
}

// TestXLMQuoteRestampDecide_DeclinedRowsAreReportedNotGuessed: a row the
// anchor cannot price keeps exactly what it holds. A stored NULL stays
// NULL (coverage the tier cannot recover, counted); a stored value is
// never blanked and never replaced by a quote-side estimate.
func TestXLMQuoteRestampDecide_DeclinedRowsAreReportedNotGuessed(t *testing.T) {
	t.Parallel()
	// A resolver with no XLM/USD rate: the anchor declines every row.
	resolver := stubFXResolver{prices: map[string]string{}}
	plan := &RestampPlan{Stats: NewXLMBaseRestampStats()}
	for _, stored := range []*string{nil, strptr("41.00000000")} {
		row := mirrorScan("sdex", xlmBaseTestQuote, "native", "7132667", "49999996", stored)
		got, disp, err := restampDecide(row, nil, xlmQuoteTierFor, xlmQuoteValuerAt(resolver), true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if disp != xlmBaseAnchorDeclined {
			t.Fatalf("stored=%v: disposition = %v, want the declined disposition", stored, disp)
		}
		if got.Want != "" {
			t.Errorf("stored=%v: the tier proposed %q for a row the anchor declined", stored, got.Want)
		}
		plan.Record(got, disp)
	}
	if len(plan.Rows) != 0 {
		t.Errorf("declined rows reached the write set: %d row(s)", len(plan.Rows))
	}
	if plan.Stats.AnchorDeclinedNull != 1 || plan.Stats.AnchorDeclinedStored != 1 {
		t.Errorf("declined rows were not COUNTED: null=%d stored=%d",
			plan.Stats.AnchorDeclinedNull, plan.Stats.AnchorDeclinedStored)
	}
	if plan.Stats.Residual() != 0 {
		t.Errorf("residual = %d: a scanned row was filed nowhere", plan.Stats.Residual())
	}
}

// TestCEXFiatRestampDecide_ValuesTheFiatQuoteAtTheSourceScale: a binance
// BTC/EUR trade is worth quote_amount/1e8 x EUR/USD — the CEX amount
// scale (CS-040), not the on-chain 1e7 — and the number is the insert
// path's own.
func TestCEXFiatRestampDecide_ValuesTheFiatQuoteAtTheSourceScale(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{"fiat:EUR": "1.08"}}
	// 250.00000000 EUR at $1.08 = $270.
	row := mirrorScan("binance", "crypto:BTC", "fiat:EUR", "100000", "25000000000", nil)

	got, disp, err := restampDecide(row, nil, cexFiatTierFor, cexFiatValuerAt(resolver), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if disp != xlmBaseWrite {
		t.Fatalf("disposition = %v, want the write disposition", disp)
	}
	if want := "270.00000000"; got.Want != want {
		t.Errorf("Want = %q, want %q", got.Want, want)
	}
	if !got.NullFill {
		t.Error("NullFill = false for a row that was stored NULL")
	}
}

// TestCEXFiatRestampDecide_DeclinedRowsAreReportedNotGuessed: no rate for
// the currency means the row is reported, not valued through some other
// route. The stored value survives; the stored NULL stays NULL.
func TestCEXFiatRestampDecide_DeclinedRowsAreReportedNotGuessed(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{}}
	for _, stored := range []*string{nil, strptr("270.00000000")} {
		row := mirrorScan("binance", "crypto:BTC", "fiat:EUR", "100000", "25000000000", stored)
		got, disp, err := restampDecide(row, nil, cexFiatTierFor, cexFiatValuerAt(resolver), true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if disp != xlmBaseAnchorDeclined {
			t.Fatalf("stored=%v: disposition = %v, want the declined disposition", stored, disp)
		}
		if got.Want != "" {
			t.Errorf("stored=%v: the tier proposed %q for a row the FX feed declined", stored, got.Want)
		}
	}
}

// TestRestampTierGates_TheSubstanceGate is the tier-3b valuation
// incident's rule, stated as a test: NO tier here prices a pair whose
// legs are all counterparty-authored. A token/token row — the ~54M-row
// population on r1 — is out of scope for every gate, so it can never
// enter a write set however the scan is widened.
func TestRestampTierGates_TheSubstanceGate(t *testing.T) {
	t.Parallel()
	spec, err := NewUSDVolumeQuoteSpec([]string{usdcAssetID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const tokenA = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	const tokenB = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"

	gates := map[string]restampGate{
		"xlm-base":  xlmBaseTierFor,
		"xlm-quote": xlmQuoteTierFor,
		"cex-fx":    cexFiatTierFor,
	}
	spam := mirrorScan("sdex", tokenA, tokenB, "10000000", "50000000000000", nil)
	for name, gate := range gates {
		// A resolver that WOULD price both legs, so the only thing keeping
		// the row unpriced is the gate itself.
		resolver := stubFXResolver{prices: map[string]string{
			tokenA: "0.17118456", tokenB: "0.17118456",
			canonical.NativeAsset().String(): "0.14488",
		}}
		value := restampValuer(func(t canonical.Trade) (*string, error) { return nil, nil })
		switch name {
		case "xlm-base":
			value = func(tr canonical.Trade) (*string, error) {
				return tradeUSDVolumeViaXLMBaseAnchorFor(context.Background(), tr, resolver), nil
			}
		case "xlm-quote":
			value = xlmQuoteValuerAt(resolver)
		case "cex-fx":
			value = cexFiatValuerAt(resolver)
		}
		got, disp, derr := restampDecide(spam, spec, gate, value, true, nil)
		if derr != nil {
			t.Fatal(derr)
		}
		if disp != xlmBaseNotDEX {
			t.Errorf("%s: token/token row got disposition %v, want out-of-scope", name, disp)
		}
		if got.Want != "" {
			t.Errorf("%s: token/token row was valued at %q — the substance gate is open", name, got.Want)
		}
	}
}

// TestRestampTierGates_KeepTheTiersDisjoint: every row belongs to exactly
// ONE tier. Two tiers claiming a row would each stamp it at their own
// generation, and the INV-3 guard would make the run ORDER decide the
// value.
func TestRestampTierGates_KeepTheTiersDisjoint(t *testing.T) {
	t.Parallel()
	spec, err := NewUSDVolumeQuoteSpec([]string{usdcAssetID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name                       string
		row                        restampScanRow
		xlmBase, xlmQuote, cexFiat restampTierVerdict
	}{
		{
			name:    "XLM base, token quote — xlm-base owns it",
			row:     mirrorScan("sdex", "native", xlmBaseTestQuote, "49999996", "7132667", nil),
			xlmBase: restampTierOwns, xlmQuote: restampTierOutOfScope, cexFiat: restampTierOutOfScope,
		},
		{
			name:    "token base, XLM quote — xlm-quote owns it",
			row:     mirrorScan("sdex", xlmBaseTestQuote, "native", "7132667", "49999996", nil),
			xlmBase: restampTierOutOfScope, xlmQuote: restampTierOwns, cexFiat: restampTierOutOfScope,
		},
		{
			name:    "XLM on BOTH legs — the base tier alone, never both",
			row:     mirrorScan("sdex", "native", nativeXLMSAC, "49999996", "49999996", nil),
			xlmBase: restampTierOwns, xlmQuote: restampTierOutOfScope, cexFiat: restampTierOutOfScope,
		},
		{
			name:    "USDC base, XLM quote — tier 2b, EXACT",
			row:     mirrorScan("sdex", usdcAssetID, "native", "1000000", "49999996", nil),
			xlmBase: restampTierOutOfScope, xlmQuote: restampTierPegged, cexFiat: restampTierOutOfScope,
		},
		{
			name:    "XLM base, USDC quote — tier 2, EXACT",
			row:     mirrorScan("sdex", "native", usdcAssetID, "49999996", "1000000", nil),
			xlmBase: restampTierPegged, xlmQuote: restampTierOutOfScope, cexFiat: restampTierOutOfScope,
		},
		{
			name:    "binance BTC/EUR — cex-fx owns it",
			row:     mirrorScan("binance", "crypto:BTC", "fiat:EUR", "100000", "25000000000", nil),
			xlmBase: restampTierOutOfScope, xlmQuote: restampTierOutOfScope, cexFiat: restampTierOwns,
		},
		{
			name:    "binance BTC/USD — tier 1, EXACT",
			row:     mirrorScan("binance", "crypto:BTC", "fiat:USD", "100000", "27000000000", nil),
			xlmBase: restampTierOutOfScope, xlmQuote: restampTierOutOfScope, cexFiat: restampTierPegged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trade, _, ok := restampScope(tc.row, spec, func(canonical.Trade, *USDVolumeQuoteSpec) restampTierVerdict {
				return restampTierOwns
			})
			if !ok {
				t.Fatalf("fixture row does not parse: %+v", tc.row)
			}
			if got := xlmBaseTierFor(trade, spec); got != tc.xlmBase {
				t.Errorf("xlmBaseTierFor = %v, want %v", got, tc.xlmBase)
			}
			if got := xlmQuoteTierFor(trade, spec); got != tc.xlmQuote {
				t.Errorf("xlmQuoteTierFor = %v, want %v", got, tc.xlmQuote)
			}
			if got := cexFiatTierFor(trade, spec); got != tc.cexFiat {
				t.Errorf("cexFiatTierFor = %v, want %v", got, tc.cexFiat)
			}
			owners := 0
			for _, v := range []restampTierVerdict{tc.xlmBase, tc.xlmQuote, tc.cexFiat} {
				if v == restampTierOwns {
					owners++
				}
			}
			if owners > 1 {
				t.Errorf("%d tiers claim this row; a row belongs to exactly one", owners)
			}
		})
	}
}

// TestRestampScanSelect_BoundsTheTiersOwnLeg: the only thing that varies
// between the three scans is the column, and every value is a
// placeholder. A statement that lost the generation guard or the window
// would widen a money-column write set silently.
func TestRestampScanSelect_BoundsTheTiersOwnLeg(t *testing.T) {
	t.Parallel()
	base, quote := restampScanSelect(restampLegBase), restampScanSelect(restampLegQuote)
	if !strings.Contains(base, "AND base_asset = ANY($4)") {
		t.Errorf("base-leg scan does not filter base_asset:\n%s", base)
	}
	if !strings.Contains(quote, "AND quote_asset = ANY($4)") {
		t.Errorf("quote-leg scan does not filter quote_asset:\n%s", quote)
	}
	for _, q := range []string{base, quote} {
		for _, want := range []string{
			"ts >= $1 AND ts < $2",
			"source = ANY($3)",
			"derive_generation <= $5",
			"ORDER BY ts, source, ledger, tx_hash, op_index",
		} {
			if !strings.Contains(q, want) {
				t.Errorf("scan lacks %q:\n%s", want, q)
			}
		}
	}
}

// TestCEXFiatQuoteAssets_IsTheNonUSDFiatAllowList: the cex-fx scan is
// bounded by a closed allow-list of fiat asset ids. USD is NOT on it (a
// USD quote is tier 1, exact), and the list is derived from ADR-0010
// rather than from a literal that would go stale.
func TestCEXFiatQuoteAssets_IsTheNonUSDFiatAllowList(t *testing.T) {
	t.Parallel()
	assets := cexFiatQuoteAssets()
	seen := map[string]bool{}
	for _, a := range assets {
		if !strings.HasPrefix(a, "fiat:") {
			t.Errorf("non-fiat asset id %q in the cex-fx scan list", a)
		}
		seen[a] = true
	}
	for _, want := range []string{"fiat:EUR", "fiat:GBP"} {
		if !seen[want] {
			t.Errorf("%s missing from the cex-fx scan list (the population on r1 today)", want)
		}
	}
	if seen["fiat:USD"] {
		t.Error("fiat:USD is on the cex-fx scan list; a USD quote is tier 1 and exact")
	}
	if len(assets) != len(canonical.KnownFiatCodes())-1 {
		t.Errorf("scan list has %d entries, want every ADR-0010 code but USD (%d)",
			len(assets), len(canonical.KnownFiatCodes())-1)
	}
}

// TestPlanCEXFiatUSDVolumeRestamp_RefusesAWiderToleranceThanTheLivePath:
// the as-of tolerance may be narrowed, never widened. A quote the live
// insert path would decline as stale must never be the source of a
// backfilled value — that is the property the whole re-derive rests on.
func TestPlanCEXFiatUSDVolumeRestamp_RefusesAWiderToleranceThanTheLivePath(t *testing.T) {
	t.Parallel()
	s := &Store{}
	_, err := s.PlanCEXFiatUSDVolumeRestamp(context.Background(), RestampScanParams{
		From: time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
	}, CEXFiatMaxQuoteStaleness+time.Hour)
	if err == nil || !strings.Contains(err.Error(), "exceeds the live insert path's own fx_quotes lookback") {
		t.Fatalf("err = %v, want the widened-tolerance refusal", err)
	}
}
