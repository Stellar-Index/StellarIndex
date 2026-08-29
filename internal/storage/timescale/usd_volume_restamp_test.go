package timescale

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── W5.3: the exact-tier restamp formula ─────────────────────────

// TestExactTierUSDVolume_TracksTheWaterfall pins the restamp's formula to
// the INSERT path byte-for-byte: for every exact-tier shape, the value
// [ExactTierUSDVolume] would write equals what [tradeUSDVolume] writes for
// the same row — same leg, same scale, same FloatString(8) render. A drift
// here means the restamp "repairs" rows to a value the writer would never
// have produced, and verify-usd-volume would then flag the repaired rows.
func TestExactTierUSDVolume_TracksTheWaterfall(t *testing.T) {
	spec := testQuoteSpec(t)
	usdcAsset, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usdx, err := canonical.NewClassicAsset("USDX", "GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7DS56Z4OKOFU")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		source      string
		base, quote canonical.Asset
		baseAmt     int64
		quoteAmt    int64
		want        string
	}{
		// CEX stamps 1e8: 1,250,000,000 → $12.5.
		{"cex USD quote (tier 1/2, 8 decimals)", "binance", xlm, usd, 10_000_000_000, 1_250_000_000, "12.50000000"},
		// SDEX classic 1e7: 1,250,000,000 stroops → $125.
		{"dex pegged quote (tier 2, 7 decimals)", "sdex", canonical.NativeAsset(), usdcAsset, 10_000_000_000, 1_250_000_000, "125.00000000"},
		// Tier 2b reads the BASE leg — the W5.3 class.
		{"dex pegged base (tier 2b)", "sdex", usdcAsset, canonical.NativeAsset(), 1_250_000_000, 10_000_000_000, "125.00000000"},
		// Both legs pegged: the quote wins, and the amounts differ so
		// reading the wrong leg yields a different number.
		{"dex both legs pegged (quote wins)", "sdex", usdcAsset, usdx, 1_250_000_000, 9_990_000_000, "999.00000000"},
		// Sub-unit dust: the render must keep all 8 places.
		{"dex dust", "sdex", canonical.NativeAsset(), usdcAsset, 1, 3, "0.00000030"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair, perr := canonical.NewPair(tc.base, tc.quote)
			if perr != nil {
				t.Fatal(perr)
			}
			tr := canonical.Trade{
				Source:      tc.source,
				Pair:        pair,
				BaseAmount:  canonical.NewAmount(big.NewInt(tc.baseAmt)),
				QuoteAmount: canonical.NewAmount(big.NewInt(tc.quoteAmt)),
			}
			written := tradeUSDVolume(context.Background(), tr, spec, nil)
			if written == nil {
				t.Fatalf("tradeUSDVolume declined an exact-tier fixture")
			}
			tier, decimals, cerr := ClassifyUSDVolumeTier(tc.source, tc.base.String(), tc.quote.String(), spec)
			if cerr != nil {
				t.Fatalf("ClassifyUSDVolumeTier: %v", cerr)
			}
			got, ok := ExactTierUSDVolume(tier, decimals, tr.BaseAmount.String(), tr.QuoteAmount.String())
			if !ok {
				t.Fatalf("ExactTierUSDVolume declined tier %q/%d", tier, decimals)
			}
			if got != *written {
				t.Errorf("restamp formula = %q but the insert path wrote %q — the two have drifted", got, *written)
			}
			if got != tc.want {
				t.Errorf("restamp formula = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExactTierUSDVolume_DeclinesWhatTheWaterfallDeclines: the waterfall
// returns nil (usd_volume NULL) for a non-positive quote before probing any
// tier, and tier 2b declines a non-positive base. The restamp must decline
// the same rows — "repairing" them to a number would be a new defect.
func TestExactTierUSDVolume_DeclinesWhatTheWaterfallDeclines(t *testing.T) {
	cases := []struct {
		name        string
		tier        USDVolumeTier
		base, quote string
	}{
		{"zero quote on quote-pegged", TierQuotePegged, "100", "0"},
		{"zero quote on base-pegged (waterfall bails first)", TierBasePegged, "100", "0"},
		{"zero base on base-pegged", TierBasePegged, "0", "100"},
		{"negative base", TierBasePegged, "-5", "100"},
		{"garbage amount", TierQuotePegged, "100", "1e5"},
		{"estimated tier is not restampable", TierEstimated, "100", "100"},
		{"unvaluable tier is not restampable", TierUnvaluable, "100", "100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := ExactTierUSDVolume(tc.tier, 7, tc.base, tc.quote); ok {
				t.Errorf("ExactTierUSDVolume = %q, ok=true; want declined", got)
			}
		})
	}
}

// TestUSDVolumeRestampDecision is the DIFFERENTIAL: a correctly-stamped row
// is reported UNCHANGED (whatever NUMERIC render Postgres chose), a wrong
// row is reported changed with the identity as the target, and a NULL row
// changes only when the operator opted into filling coverage.
func TestUSDVolumeRestampDecision(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name        string
		stored      *string
		tier        USDVolumeTier
		base, quote string
		fillNull    bool
		wantValue   string
		wantChanged bool
		wantOK      bool
	}{
		{"correct row, canonical render", str("125.00000000"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, true},
		{"correct row, short NUMERIC render", str("125"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, true},
		{"correct row, long NUMERIC render", str("125.0000000000000000"), TierQuotePegged, "10000000000", "1250000000", false, "125.00000000", false, true},
		// The W5.3 class: a USDC-base row valued by the resolver's VWAP
		// (+0.7%) instead of the $1 peg identity.
		{"resolver-priced base_pegged row", str("125.87500000"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", true, true},
		// One unit at the render scale — the smallest possible defect.
		{"off by 1e-8", str("125.00000001"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", true, true},
		{"NULL row, coverage fill off", nil, TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, true},
		{"NULL row, coverage fill on", nil, TierBasePegged, "1250000000", "10000000000", true, "125.00000000", true, true},
		{"corrupt stored render is not rewritten", str("12x"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, false},
		{"non-restampable row", str("125"), TierBasePegged, "0", "10000000000", false, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, ok := USDVolumeRestampDecision(tc.stored, tc.tier, 7, tc.base, tc.quote, tc.fillNull)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if got != tc.wantValue {
				t.Errorf("want value = %q, want %q", got, tc.wantValue)
			}
		})
	}
}

// TestExactTierRestampScope_MirrorsTheDecision pins the SQL scope to the
// Go decision: the predicate must (1) carry the INV-3 generation guard,
// (2) skip rows already satisfying the identity via IS DISTINCT FROM, (3)
// exclude NULLs unless FillNull, (4) bind the Go-decided leg + 10^decimals
// per group rather than deciding either in SQL, and (5) refuse non-exact
// groups outright.
func TestExactTierRestampScope_MirrorsTheDecision(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	groups := []USDVolumeRestampGroup{
		{Source: "sdex", BaseAsset: "USDC-GA5Z", QuoteAsset: "native", Tier: TierBasePegged, Decimals: 7},
		{Source: "kraken", BaseAsset: "crypto:XLM", QuoteAsset: "fiat:USD", Tier: TierQuotePegged, Decimals: 8},
	}
	p := USDVolumeRestampParams{Groups: groups, From: from, To: from.Add(time.Hour), Generation: 1_755_000_000}

	groupRel, where, identity, args, err := exactTierRestampScope(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(groupRel, "(VALUES ") || !strings.HasSuffix(groupRel, ") AS g(source, base_asset, quote_asset, leg, denom)") {
		t.Errorf("group relation = %q", groupRel)
	}
	fromWhere := where
	for _, want := range []string{
		"t.derive_generation <= $3",
		"t.usd_volume IS DISTINCT FROM round(",
		"t.usd_volume IS NOT NULL",
		"t.quote_amount > 0",
	} {
		if !strings.Contains(fromWhere, want) {
			t.Errorf("scope lacks %q:\n%s", want, fromWhere)
		}
	}
	if identity != "round((CASE WHEN g.leg = 'base' THEN t.base_amount ELSE t.quote_amount END) / g.denom, 8)" {
		t.Errorf("identity expression = %q", identity)
	}
	// $1..$3 then 5 per group, leg + denominator decided in Go.
	if len(args) != 3+5*len(groups) {
		t.Fatalf("args = %d, want %d", len(args), 3+5*len(groups))
	}
	if args[2] != int64(1_755_000_000) {
		t.Errorf("generation arg = %v", args[2])
	}
	if args[6] != "base" || args[7] != "10000000" {
		t.Errorf("sdex group bound (leg, denom) = (%v, %v), want (base, 10000000)", args[6], args[7])
	}
	if args[11] != "quote" || args[12] != "100000000" {
		t.Errorf("kraken group bound (leg, denom) = (%v, %v), want (quote, 100000000)", args[11], args[12])
	}

	p.FillNull = true
	_, fromWhere, _, _, err = exactTierRestampScope(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fromWhere, "IS NOT NULL") {
		t.Errorf("FillNull scope still excludes NULL rows:\n%s", fromWhere)
	}

	p.Groups = append(p.Groups, USDVolumeRestampGroup{Source: "sdex", BaseAsset: "native", QuoteAsset: "AQUA-GB", Tier: TierEstimated})
	if _, _, _, _, err := exactTierRestampScope(p); err == nil {
		t.Error("an estimated-tier group was accepted — the restamp would overwrite resolver-priced rows with quote/10^0")
	}
	if _, _, _, _, err := exactTierRestampScope(USDVolumeRestampParams{Groups: groups, From: from, To: from}); err == nil {
		t.Error("an empty window was accepted")
	}
}
