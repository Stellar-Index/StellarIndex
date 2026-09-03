// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// xlmBaseTestQuote is a pure-Soroban SEP-41 token with no USD market —
// the quote leg shape behind every #372 violation.
const xlmBaseTestQuote = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"

// usdcAssetID is the operator's canonical USD peg on r1.
const usdcAssetID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// xlmBaseScan builds a scanned row in the shape the SELECT produces.
func xlmBaseScan(base, quote, baseAmt, quoteAmt string, stored *string) xlmBaseRestampScan {
	return xlmBaseRestampScan{
		Source:      "sdex",
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

// xlmAnchorAt wraps a stub resolver into the anchor closure the planner
// injects, so the unit tests drive the SAME production function the live
// insert path calls.
func xlmAnchorAt(r USDVolumeFXResolver) func(canonical.Trade) *string {
	return func(t canonical.Trade) *string {
		return tradeUSDVolumeViaXLMBaseAnchorFor(context.Background(), t, r)
	}
}

func strptr(s string) *string { return &s }

// TestXLMBaseRestampDecide_ProducesTheAnchorValueForTheIncidentRow is the
// #372 fixture, reproduced from the r1 measurement in the G9 triage:
//
//	ledger 62643474, 2026-05-19T19:12:03Z, sdex native/BUCK
//	base  49,999,996 stroops (4.9999996 XLM)
//	XLM/USD at that minute  0.14488
//	stored usd_volume       0.00372265   <- the quote-side thin-book value
//	XLM-leg value           0.72439994   <- what the trade was worth
//
// The assertion is the CORRECTED NUMBER, not "non-nil": 4.9999996 x
// 0.14488 rendered exactly as [tradeUSDVolume] renders it.
func TestXLMBaseRestampDecide_ProducesTheAnchorValueForTheIncidentRow(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.14488",
	}}
	row := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", strptr("0.00372265"))

	got, disp := xlmBaseRestampDecide(row, nil, false, nil, xlmAnchorAt(resolver))
	if disp != xlmBaseWrite {
		t.Fatalf("disposition = %v, want xlmBaseWrite", disp)
	}
	const want = "0.72439994"
	if got.Want != want {
		t.Errorf("Want = %q, want %q", got.Want, want)
	}
	if got.AbsDelta.FloatString(8) != "0.72067729" {
		t.Errorf("AbsDelta = %s, want 0.72067729", got.AbsDelta.FloatString(8))
	}
	if !got.RelOK {
		t.Fatal("RelOK = false, want a relative delta against a non-zero stored value")
	}
	// 0.72067729 / 0.00372265 = 193.59x — the 43x headline is the RATIO
	// of the two values (0.72439994/0.00372265 = 194.6); the relative
	// DELTA is one less than that.
	if got.RelDelta.Cmp(big.NewRat(100, 1)) < 0 {
		t.Errorf("RelDelta = %s, want >= 100 (a >100x move)", got.RelDelta.FloatString(4))
	}
	if got.NullFill {
		t.Error("NullFill = true for a row that already carried a value")
	}
}

// TestXLMBaseRestampDecide_IsLockstepWithTheInsertPath is the
// anti-reimplementation pin: for the same row and the same resolver, the
// value the re-derive would WRITE must be byte-identical to the value
// [tradeUSDVolume] — the function InsertTrade calls — produces. If a
// future change makes the restamp compute its own number, this fails.
func TestXLMBaseRestampDecide_IsLockstepWithTheInsertPath(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.1449773",
	}}
	quote, err := canonical.NewSorobanAsset(xlmBaseTestQuote)
	if err != nil {
		t.Fatal(err)
	}
	for _, baseAmt := range []string{"1", "49999996", "2500000000", "123456789012345678901234567890"} {
		amt, _ := new(big.Int).SetString(baseAmt, 10)
		pair, perr := canonical.NewPair(canonical.NativeAsset(), quote)
		if perr != nil {
			t.Fatal(perr)
		}
		trade := canonical.Trade{
			Source:      "sdex",
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(amt),
			QuoteAmount: canonical.NewAmount(big.NewInt(7132667)),
		}
		viaInsert := tradeUSDVolume(context.Background(), trade, nil, resolver)
		if viaInsert == nil {
			t.Fatalf("base %s: tradeUSDVolume declined an XLM-base fixture", baseAmt)
		}
		// Stored "0" differs from every positive anchor value, so every
		// fixture reaches the write branch and its Want is comparable.
		row := xlmBaseScan("native", xlmBaseTestQuote, baseAmt, "7132667", strptr("0"))
		got, disp := xlmBaseRestampDecide(row, nil, false, nil, xlmAnchorAt(resolver))
		if disp != xlmBaseWrite {
			t.Fatalf("base %s: disposition = %v, want xlmBaseWrite", baseAmt, disp)
		}
		if got.Want != *viaInsert {
			t.Errorf("base %s: restamp would write %q, insert path writes %q — the two have drifted",
				baseAmt, got.Want, *viaInsert)
		}
	}
}

// TestXLMBaseRestampDecide_AnchorDeclinedIsNeverGuessedAt is rule 2+3 of
// the file header, and the requirement stated in #372's remediation
// brief: a row the anchor cannot price keeps whatever it holds. A stored
// NULL stays NULL — it is NOT filled with the quote-side estimate the
// live waterfall would fall through to — and a stored (wrong) value is
// not blanked either.
func TestXLMBaseRestampDecide_AnchorDeclinedIsNeverGuessedAt(t *testing.T) {
	t.Parallel()
	// No XLM/USD rate at all: the anchor declines. The token DOES have a
	// price, which is exactly the quote-side route the live path would
	// fall through to — and which must not be reached from here.
	resolver := stubFXResolver{prices: map[string]string{
		xlmBaseTestQuote: "0.0052167",
	}}
	cases := []struct {
		name   string
		stored *string
	}{
		{"stored NULL stays NULL", nil},
		{"stored value is left alone", strptr("0.00372265")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, fillNull := range []bool{false, true} {
				row := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", tc.stored)
				got, disp := xlmBaseRestampDecide(row, nil, fillNull, nil, xlmAnchorAt(resolver))
				if disp != xlmBaseAnchorDeclined {
					t.Fatalf("fill-null=%v: disposition = %v, want xlmBaseAnchorDeclined", fillNull, disp)
				}
				if got.Want != "" {
					t.Errorf("fill-null=%v: Want = %q, want empty — the restamp must not value a row the anchor declined", fillNull, got.Want)
				}
			}
		})
	}

	// And the plan files them into the two separate counters the report
	// prints, so "coverage we cannot recover" and "still wrong after this
	// run" are never merged into one number.
	plan := &XLMBaseRestampPlan{Stats: NewXLMBaseRestampStats()}
	plan.Record(XLMBaseRestampRow{}, xlmBaseAnchorDeclined)
	plan.Record(XLMBaseRestampRow{Stored: strptr("0.5")}, xlmBaseAnchorDeclined)
	if plan.Stats.AnchorDeclinedNull != 1 || plan.Stats.AnchorDeclinedStored != 1 {
		t.Errorf("declined split = %d null / %d stored, want 1/1",
			plan.Stats.AnchorDeclinedNull, plan.Stats.AnchorDeclinedStored)
	}
	if len(plan.Rows) != 0 {
		t.Errorf("declined rows entered the write set (%d rows)", len(plan.Rows))
	}
}

// TestXLMBaseRestampDecide_NullFillIsOptIn: the NULL population (~31% of
// the measured days) is a COVERAGE change, so it is only written under
// -fill-null — but it is COUNTED either way, so a dry run always shows
// the operator what the flag would buy.
func TestXLMBaseRestampDecide_NullFillIsOptIn(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.14488",
	}}
	row := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", nil)

	got, disp := xlmBaseRestampDecide(row, nil, false, nil, xlmAnchorAt(resolver))
	if disp != xlmBaseSkipNull {
		t.Fatalf("without -fill-null: disposition = %v, want xlmBaseSkipNull", disp)
	}
	if got.Want != "0.72439994" || !got.NullFill {
		t.Errorf("the skipped row must still carry the value it would gain: Want=%q NullFill=%v", got.Want, got.NullFill)
	}

	got, disp = xlmBaseRestampDecide(row, nil, true, nil, xlmAnchorAt(resolver))
	if disp != xlmBaseWrite || !got.NullFill || got.Want != "0.72439994" {
		t.Fatalf("with -fill-null: disposition=%v NullFill=%v Want=%q", disp, got.NullFill, got.Want)
	}

	plan := &XLMBaseRestampPlan{Stats: NewXLMBaseRestampStats()}
	plan.Record(got, xlmBaseSkipNull)
	if plan.Stats.NullCandidates != 1 || plan.Stats.Changed != 0 {
		t.Errorf("skipped NULL: candidates=%d changed=%d, want 1/0", plan.Stats.NullCandidates, plan.Stats.Changed)
	}
}

// TestXLMBaseRestampDecide_ExcludesTheExactTiers: a USD-pegged QUOTE leg
// is tier 1/2 and belongs to `-tier exact`. The two tiers must not both
// claim a row, or a run of one would undo the other.
func TestXLMBaseRestampDecide_ExcludesTheExactTiers(t *testing.T) {
	t.Parallel()
	spec, err := NewUSDVolumeQuoteSpec([]string{usdcAssetID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.14488",
	}}
	row := xlmBaseScan("native", usdcAssetID, "49999996", "7132667", strptr("0.71326670"))
	_, disp := xlmBaseRestampDecide(row, spec, true, nil, xlmAnchorAt(resolver))
	if disp != xlmBaseQuotePegged {
		t.Fatalf("native/USDC disposition = %v, want xlmBaseQuotePegged (the exact tier owns it)", disp)
	}
}

// TestXLMBaseRestampDecide_RefusesANonXLMBase: the 1e7 divisor in the
// anchor belongs to the ANCHOR asset. A widened scan that let a non-XLM
// base through would silently mis-scale it, so the Go gate re-asserts
// the tier definition and fails closed.
func TestXLMBaseRestampDecide_RefusesANonXLMBase(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String():                                "0.14488",
		"AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA": "0.003",
	}}
	row := xlmBaseScan("AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
		xlmBaseTestQuote, "49999996", "7132667", strptr("0.00372265"))
	if _, disp := xlmBaseRestampDecide(row, nil, true, nil, xlmAnchorAt(resolver)); disp != xlmBaseNotDEX {
		t.Fatalf("non-XLM base disposition = %v, want xlmBaseNotDEX (refused)", disp)
	}

	// A CEX row is likewise not in this tier — the anchor is DEX-only.
	cex := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", nil)
	cex.Source = "binance"
	if _, disp := xlmBaseRestampDecide(cex, nil, true, nil, xlmAnchorAt(resolver)); disp != xlmBaseNotDEX {
		t.Fatalf("binance disposition = %v, want xlmBaseNotDEX", disp)
	}
}

// TestXLMBaseRestampDecide_IdempotentAndNumericComparison: a row already
// holding the anchor's value is untouched even when Postgres rendered it
// at a different NUMERIC scale — otherwise a re-run would rewrite the
// whole span and bump every derive_generation for nothing.
func TestXLMBaseRestampDecide_IdempotentAndNumericComparison(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.14488",
	}}
	for _, stored := range []string{"0.72439994", "0.7243999400000", "0.724399940"} {
		row := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", strptr(stored))
		if _, disp := xlmBaseRestampDecide(row, nil, true, nil, xlmAnchorAt(resolver)); disp != xlmBaseUnchanged {
			t.Errorf("stored %q: disposition = %v, want xlmBaseUnchanged", stored, disp)
		}
	}
}

// TestXLMBaseRestampDecide_MinRelDeltaSuppressesSmallMovesOnly: the write
// filter narrows a run to the large moves without ever suppressing a NULL
// fill, which has no relative move to measure.
func TestXLMBaseRestampDecide_MinRelDeltaSuppressesSmallMovesOnly(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.14488",
	}}
	tenPercent := big.NewRat(1, 10)

	// 0.72000000 -> 0.72439994 is +0.61%: below a 10% floor.
	small := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", strptr("0.72000000"))
	if _, disp := xlmBaseRestampDecide(small, nil, true, tenPercent, xlmAnchorAt(resolver)); disp != xlmBaseBelowMinRelDelta {
		t.Errorf("0.61%% move: disposition = %v, want xlmBaseBelowMinRelDelta", disp)
	}
	// The same row with no floor is written.
	if _, disp := xlmBaseRestampDecide(small, nil, true, nil, xlmAnchorAt(resolver)); disp != xlmBaseWrite {
		t.Errorf("0.61%% move with no floor: disposition = %v, want xlmBaseWrite", disp)
	}
	// A NULL fill is never suppressed.
	nullRow := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", nil)
	if _, disp := xlmBaseRestampDecide(nullRow, nil, true, tenPercent, xlmAnchorAt(resolver)); disp != xlmBaseWrite {
		t.Errorf("NULL fill under a 10%% floor: disposition = %v, want xlmBaseWrite", disp)
	}
}

// TestXLMBaseRestampDecide_RefusesUnparseableAmounts: a non-positive or
// non-integer amount is reportable, never rewritten — the same posture
// the exact-tier tool takes, and the same bail-out [tradeUSDVolume] makes.
func TestXLMBaseRestampDecide_RefusesUnparseableAmounts(t *testing.T) {
	t.Parallel()
	resolver := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.14488",
	}}
	cases := []struct{ base, quote string }{
		{"0", "7132667"},
		{"-5", "7132667"},
		{"49999996", "0"},
		{"1.5", "7132667"},
		{"", "7132667"},
	}
	for _, tc := range cases {
		row := xlmBaseScan("native", xlmBaseTestQuote, tc.base, tc.quote, nil)
		if _, disp := xlmBaseRestampDecide(row, nil, true, nil, xlmAnchorAt(resolver)); disp != xlmBaseUnparseable {
			t.Errorf("base=%q quote=%q: disposition = %v, want xlmBaseUnparseable", tc.base, tc.quote, disp)
		}
	}
	// A corrupt stored NUMERIC is likewise reportable, not rewritable.
	corrupt := xlmBaseScan("native", xlmBaseTestQuote, "49999996", "7132667", strptr("NaN"))
	if _, disp := xlmBaseRestampDecide(corrupt, nil, true, nil, xlmAnchorAt(resolver)); disp != xlmBaseUnparseable {
		t.Errorf("stored NaN: disposition = %v, want xlmBaseUnparseable", disp)
	}
}

// TestXLMBaseRestampStats_ResidualIsZeroWhenEveryRowIsFiled: the report
// prints a residual so a row scanned and filed NOWHERE is visible rather
// than quietly shrinking a population. Drive every disposition through
// Record and require the residual to close.
func TestXLMBaseRestampStats_ResidualIsZeroWhenEveryRowIsFiled(t *testing.T) {
	t.Parallel()
	plan := &XLMBaseRestampPlan{Stats: NewXLMBaseRestampStats()}
	changed := XLMBaseRestampRow{
		Stored: strptr("1.00000000"), Want: "2.00000000",
		AbsDelta: big.NewRat(1, 1), RelDelta: big.NewRat(1, 1), RelOK: true,
	}
	nullFill := XLMBaseRestampRow{Want: "2.00000000", AbsDelta: big.NewRat(2, 1), NullFill: true}
	for _, tc := range []struct {
		row  XLMBaseRestampRow
		disp xlmBaseDisposition
	}{
		{changed, xlmBaseWrite},
		{nullFill, xlmBaseWrite},
		{XLMBaseRestampRow{}, xlmBaseUnchanged},
		{XLMBaseRestampRow{}, xlmBaseQuotePegged},
		{XLMBaseRestampRow{}, xlmBaseNotDEX},
		{XLMBaseRestampRow{}, xlmBaseUnparseable},
		{XLMBaseRestampRow{}, xlmBaseAnchorDeclined},
		{XLMBaseRestampRow{Stored: strptr("1")}, xlmBaseAnchorDeclined},
		{XLMBaseRestampRow{}, xlmBaseSkipNull},
		{XLMBaseRestampRow{}, xlmBaseBelowMinRelDelta},
	} {
		plan.Record(tc.row, tc.disp)
	}
	if got := plan.Stats.Residual(); got != 0 {
		t.Errorf("Residual() = %d, want 0 — a disposition is not accounted for", got)
	}
	if plan.Stats.Changed != 2 || plan.Stats.NullFilled != 1 {
		t.Errorf("changed=%d nullFilled=%d, want 2/1", plan.Stats.Changed, plan.Stats.NullFilled)
	}
	if plan.Stats.RelBucket[">=100%"] != 1 || plan.Stats.RelBucket[">=10x"] != 0 {
		t.Errorf("rel buckets = %v, want one row at >=100%% and none at >=10x", plan.Stats.RelBucket)
	}
	if plan.Stats.SumStored.FloatString(8) != "1.00000000" || plan.Stats.SumWant.FloatString(8) != "4.00000000" {
		t.Errorf("sums = stored %s / want %s", plan.Stats.SumStored.FloatString(8), plan.Stats.SumWant.FloatString(8))
	}
}

// TestDEXSourceNames_MatchesTheRegistry pins the scan's source list to
// external.Registry rather than a hard-coded list, so a newly-registered
// on-chain venue is covered the day it lands.
func TestDEXSourceNames_MatchesTheRegistry(t *testing.T) {
	t.Parallel()
	got := DEXSourceNames()
	if len(got) == 0 {
		t.Fatal("DEXSourceNames() is empty")
	}
	seen := map[string]bool{}
	for _, s := range got {
		if external.Lookup(s).Subclass != external.SubclassDEX {
			t.Errorf("%q is in the DEX scan list but its registry subclass is %q", s, external.Lookup(s).Subclass)
		}
		seen[s] = true
	}
	for name, md := range external.Registry {
		if md.Subclass == external.SubclassDEX && !seen[name] {
			t.Errorf("registered DEX source %q is missing from DEXSourceNames()", name)
		}
	}
	if !seen["sdex"] {
		t.Error("sdex missing — it is the source carrying the #372 population")
	}
}

// TestXLMAssetForms_MirrorsIsXLMAsset keeps the SQL scan's base_asset
// filter and the Go gate on the same two wire forms.
func TestXLMAssetForms_MirrorsIsXLMAsset(t *testing.T) {
	t.Parallel()
	forms := xlmAssetForms()
	if len(forms) != 2 {
		t.Fatalf("xlmAssetForms() = %v, want the two XLM wire forms", forms)
	}
	for _, f := range forms {
		a, err := canonical.ParseAsset(f)
		if err != nil {
			t.Fatalf("ParseAsset(%q): %v", f, err)
		}
		if !isXLMAsset(a) {
			t.Errorf("%q is in the scan filter but isXLMAsset says otherwise", f)
		}
	}
}

// TestXLMBaseRestampSources_FiltersToTheAllowList.
func TestXLMBaseRestampSources_FiltersToTheAllowList(t *testing.T) {
	t.Parallel()
	if got := xlmBaseRestampSources(nil); len(got) != len(DEXSourceNames()) {
		t.Errorf("nil allow-list = %v, want every DEX source", got)
	}
	got := xlmBaseRestampSources(map[string]bool{"sdex": true, "binance": true})
	if len(got) != 1 || got[0] != "sdex" {
		t.Errorf("allow-list {sdex, binance} = %v, want [sdex] — binance is not a DEX source", got)
	}
}
