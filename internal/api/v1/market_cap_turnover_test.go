package v1

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// The absolute dust floor asks whether trading is small, not whether the claimed cap is large against it: two vanity mints
// cleared the floor on four-figure volume yet published huge caps. Figures here are the ones actually served.
const (
	// SLVR-GBZVELEQ… "Silver Gram", mintx.co. No SEP-1, no curated
	// directory entry. 826,462x its own daily volume — 2,264 years to
	// turn the float over once.
	slvrCapUSD    = "3133231779.97"
	slvrVolumeUSD = "3791.13915551"
	// GOLD-GBC5ZGK6… "Gold Gram", same domain. 928,117x — 2,542 years.
	goldCapUSD    = "2754485981.60"
	goldVolumeUSD = "2967.82084657"
	// TFT-GBOVQKJY... has the highest ratio among the other assets (2,856x, 7.8 years); the default ceiling must clear it
	// widely, since illiquid is not unvalued.
	tftCapUSD    = "6131615.11"
	tftVolumeUSD = "2147.10"
	// USDC, for scale: 5.2x. One working week turns the float over.
	usdcCapUSD    = "355084398.95"
	usdcVolumeUSD = "68388882.73730980"
)

// TestCapExceedsObservedTurnoverSeparatesTheMeasuredSet: at 50,000 the two mintx.co rows are refused and every other row
// keeps its cap; the TFT to SLVR gap is 289x, so the line is not finely balanced.
func TestCapExceedsObservedTurnoverSeparatesTheMeasuredSet(t *testing.T) {
	const defaultRatio = 50_000
	for _, tt := range []struct {
		name        string
		capUSD, vol string
		wantRefused bool
	}{
		{"SLVR at 826,462x", slvrCapUSD, slvrVolumeUSD, true},
		{"GOLD at 928,117x", goldCapUSD, goldVolumeUSD, true},
		{"TFT at 2,856x — illiquid, not unvalued", tftCapUSD, tftVolumeUSD, false},
		{"USDC at 5.2x", usdcCapUSD, usdcVolumeUSD, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.vol
			if got := capExceedsObservedTurnover(tt.capUSD, &v, defaultRatio); got != tt.wantRefused {
				t.Errorf("capExceedsObservedTurnover(%s, %s, %v) = %v, want %v",
					tt.capUSD, tt.vol, float64(defaultRatio), got, tt.wantRefused)
			}
		})
	}
}

// TestCapExceedsObservedTurnoverNeverActsOnAnUnmeasuredReading mirrors the floor's rule: a
// missing, unparseable or non-positive volume is the ABSENCE of a reading, not evidence of
// thin trading, so it must not withhold a cap for a data gap.
func TestCapExceedsObservedTurnoverNeverActsOnAnUnmeasuredReading(t *testing.T) {
	huge := "3133231779.97"
	for _, tt := range []struct {
		name string
		vol  *string
	}{
		{"no volume field at all", nil},
		{"empty string", strptr("")},
		{"not a number", strptr("n/a")},
		{"zero", strptr("0")},
		{"zero with decimals", strptr("0.00")},
		{"negative", strptr("-5")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if capExceedsObservedTurnover(huge, tt.vol, 50_000) {
				t.Error("suppressed a cap on an unmeasured volume; the absence of a " +
					"reading is not evidence about liquidity")
			}
		})
	}
}

// TestCapExceedsObservedTurnoverIsDisabledAtZero — the knob's documented
// off switch. An operator who sets it to 0 gets the pre-guard behaviour
// exactly, including for the row that motivated the guard.
func TestCapExceedsObservedTurnoverIsDisabledAtZero(t *testing.T) {
	v := slvrVolumeUSD
	for _, ratio := range []float64{0, -1} {
		if capExceedsObservedTurnover(slvrCapUSD, &v, ratio) {
			t.Errorf("ratio %v suppressed a cap; <= 0 disables the guard", ratio)
		}
	}
}

// TestCapExceedsObservedTurnoverRefusesNothingOnAnAbsentCap: a cap never computed cannot be too large; the guard must
// not report "refused" for a row with nothing to refuse.
func TestCapExceedsObservedTurnoverRefusesNothingOnAnAbsentCap(t *testing.T) {
	v := "1"
	for _, c := range []string{"", "0", "0.00", "-1", "not a number"} {
		if capExceedsObservedTurnover(c, &v, 50_000) {
			t.Errorf("cap %q reported as exceeding turnover", c)
		}
	}
}

// TestCapExceedsObservedTurnoverIsExactAtTheBoundary: the comparison is strictly greater-than,
// so a cap of exactly N x volume is KEPT. The arithmetic is big.Float because a float64 would
// round across the line at this ratio times an eight-decimal dollar figure.
func TestCapExceedsObservedTurnoverIsExactAtTheBoundary(t *testing.T) {
	vol := "1000.00"
	for _, tt := range []struct {
		capUSD      string
		wantRefused bool
	}{
		{"49999999.99", false}, // just under 50,000 x
		{"50000000.00", false}, // exactly 50,000 x — kept
		{"50000000.01", true},  // one cent over
	} {
		v := vol
		if got := capExceedsObservedTurnover(tt.capUSD, &v, 50_000); got != tt.wantRefused {
			t.Errorf("cap %s against %s x 50,000: refused = %v, want %v",
				tt.capUSD, vol, got, tt.wantRefused)
		}
	}
}

// TestFillRowMarketCapRefusesACapItsOwnMarketNeverValued is the wiring test: it drives the production listing fill over
// SLVR's real figures, so a build that computed the verdict and discarded it fails.
func TestFillRowMarketCapRefusesACapItsOwnMarketNeverValued(t *testing.T) {
	s := &Server{Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000}}
	price := "1.5666161388"
	vol := slvrVolumeUSD
	row := AssetDetail{
		AssetID:      "SLVR-GBZVELEQD3WBN3R3VAG64HVBDOZ76ZL6QPLSFGKWPFED33Q3234NSLVR",
		Code:         "SLVR",
		Decimals:     7,
		PriceUSD:     &price,
		VolumeUSD24h: &vol,
	}
	// 1,999,999,682.35 tokens, the served figure.
	precise := observedSupply(map[string]string{row.AssetID: "19999996823547616"})

	s.fillRowMarketCap(context.Background(), &row, precise, nil, nil, map[string]int{row.AssetID: 5})

	if row.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q, want suppressed — $3.13B on $3,791 of daily "+
			"volume is 826,462x the float's own turnover, and the dust floor cannot "+
			"see it because the volume is real", *row.MarketCapUSD)
	}
	if !row.MarketCapLowLiquidity {
		t.Error("market_cap_low_liquidity is false on a suppressed cap; a null with no " +
			"flag is indistinguishable from an asset nobody has priced")
	}
	if row.CirculatingSupply == nil {
		t.Error("circulating_supply must still surface — the supply is a chain fact " +
			"and only the claim that a market valued it is withheld")
	}
}

// TestFillRowMarketCapKeepsTheLeastLiquidRecognisedAsset is the control: TFT is the thinnest
// asset that still publishes a cap, and if the guard took it the line would cut through the
// served set rather than around an outlier.
func TestFillRowMarketCapKeepsTheLeastLiquidRecognisedAsset(t *testing.T) {
	s := &Server{Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000}}
	price := "0.0062624500"
	vol := tftVolumeUSD
	row := AssetDetail{
		AssetID:      "TFT-GBOVQKJYSQJJIIZJJTJEXUZWGMLCAWQAZ2C4VHG3WPQBSNJMCUCLDGHY",
		Code:         "TFT",
		Decimals:     7,
		PriceUSD:     &price,
		VolumeUSD24h: &vol,
	}
	precise := observedSupply(map[string]string{row.AssetID: "9791078149866822"})

	s.fillRowMarketCap(context.Background(), &row, precise, nil, nil, map[string]int{row.AssetID: 5})

	if row.MarketCapUSD == nil {
		t.Fatal("suppressed the cap of the least liquid asset the surface still values; " +
			"being illiquid is not the same as being unvalued, and a ceiling that " +
			"catches 2,856x is drawn through the served set rather than around it")
	}
	if row.MarketCapLowLiquidity {
		t.Error("market_cap_low_liquidity set on a published cap")
	}
}

// TestFillRowMarketCapNativeIsNeverTurnoverSuppressed: XLM is definitionally liquid and the
// listing SQL gives it no source count, so like the dust floor the ceiling must exempt it. A
// low-volume day against the whole XLM float would otherwise trip the ratio.
func TestFillRowMarketCapNativeIsNeverTurnoverSuppressed(t *testing.T) {
	s := &Server{Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000}}
	price := "0.16"
	vol := "10"
	row := AssetDetail{
		AssetID: "native", Code: "XLM", Decimals: 7,
		PriceUSD: &price, VolumeUSD24h: &vol,
	}
	precise := observedSupply(map[string]string{"native": "100000000000000000"})

	s.fillRowMarketCap(context.Background(), &row, precise, nil, nil, map[string]int{})

	if row.MarketCapUSD == nil {
		t.Fatal("native market cap must never be turnover-suppressed")
	}
	if row.MarketCapLowLiquidity {
		t.Error("native must not carry market_cap_low_liquidity")
	}
}

// TestPopulateMarketCapAgreesWithTheListingOnTheSameAsset: listing and /v1/assets/{id} must agree on whether a cap is
// published. FDV is asserted too, since publishing a bigger fdv_usd while withholding market_cap_usd defeats the guard.
func TestPopulateMarketCapAgreesWithTheListingOnTheSameAsset(t *testing.T) {
	s := &Server{
		Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000},
		logger:  slogDiscard(),
	}
	price := "1.5666161388"
	vol := slvrVolumeUSD
	detail := AssetDetail{Decimals: 7, PriceUSD: &price, VolumeUSD24h: &vol}
	circ, _ := new(big.Int).SetString("19999996823547616", 10)
	maxSup, _ := new(big.Int).SetString("20000000000000000", 10)
	snap := supply.Supply{CirculatingSupply: circ, MaxSupply: maxSup}
	asset, err := canonical.ParseAsset("SLVR-GBZVELEQD3WBN3R3VAG64HVBDOZ76ZL6QPLSFGKWPFED33Q3234NSLVR")
	if err != nil {
		t.Fatalf("parse asset: %v", err)
	}

	s.populateMarketCap(context.Background(), &detail, asset, snap, asset.String(), 5)

	if detail.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q on the detail page for an asset the listing "+
			"refuses; the two surfaces must reach the same verdict", *detail.MarketCapUSD)
	}
	if !detail.MarketCapLowLiquidity {
		t.Error("market_cap_low_liquidity is false on a suppressed detail-page cap")
	}
	if detail.FDVUSD != nil {
		t.Errorf("fdv_usd = %q beside a withheld market cap; FDV is the LARGER figure, "+
			"so publishing it defeats the guard on the same row", *detail.FDVUSD)
	}
}

// TestPopulateMarketCapStillPublishesALiquidCap is the control for the detail
// path — the ceiling must be invisible to an asset with real turnover, or it
// is not a guard, it is an outage.
func TestPopulateMarketCapStillPublishesALiquidCap(t *testing.T) {
	s := &Server{
		Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000},
		logger:  slogDiscard(),
	}
	price := "1.0006355636"
	vol := usdcVolumeUSD
	detail := AssetDetail{Decimals: 7, PriceUSD: &price, VolumeUSD24h: &vol}
	circ, _ := new(big.Int).SetString("3548588635712599", 10)
	snap := supply.Supply{CirculatingSupply: circ}
	asset, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse asset: %v", err)
	}

	s.populateMarketCap(context.Background(), &detail, asset, snap, asset.String(), 5)

	if detail.MarketCapUSD == nil {
		t.Fatal("suppressed the cap of the largest asset on the index")
	}
	if detail.MarketCapLowLiquidity {
		t.Error("market_cap_low_liquidity set on a published cap")
	}
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPopulateMarketCapFlagsFDVOnlyCeilingBreach: market_cap_usd clears the turnover ceiling and publishes while
// fdv_usd alone breaches it, the common shape when declared max supply exceeds what has vested.
func TestPopulateMarketCapFlagsFDVOnlyCeilingBreach(t *testing.T) {
	s := &Server{
		Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000},
		logger:  slogDiscard(),
	}
	price := "1.5666161388"
	vol := slvrVolumeUSD // $3,791.13915551 — ceiling at 50,000x is ~$189.56M
	detail := AssetDetail{Decimals: 7, PriceUSD: &price, VolumeUSD24h: &vol}
	// 50,000,000 tokens circulating ($78.3M cap) — under the ~$189.56M ceiling.
	circ, _ := new(big.Int).SetString("500000000000000", 10)
	// 200,000,000 tokens max supply ($313.3M FDV) — over the ceiling.
	maxSup, _ := new(big.Int).SetString("2000000000000000", 10)
	snap := supply.Supply{CirculatingSupply: circ, MaxSupply: maxSup}
	asset, err := canonical.ParseAsset("SLVR-GBZVELEQD3WBN3R3VAG64HVBDOZ76ZL6QPLSFGKWPFED33Q3234NSLVR")
	if err != nil {
		t.Fatalf("parse asset: %v", err)
	}

	s.populateMarketCap(context.Background(), &detail, asset, snap, asset.String(), 5)

	if detail.MarketCapUSD == nil {
		t.Fatal("market_cap_usd suppressed even though it clears the ceiling on its own")
	}
	if detail.FDVUSD != nil {
		t.Errorf("fdv_usd = %q, want withheld — it breaches the turnover ceiling on its own",
			*detail.FDVUSD)
	}
	if !detail.MarketCapLowLiquidity {
		t.Error("market_cap_low_liquidity is false on an FDV-only ceiling breach; " +
			"a withheld fdv_usd with no flag is indistinguishable from an asset with " +
			"no max_supply on record")
	}
}

// TestFillContractMarketCapsAppliesTheSameCeiling covers the third call site: the RWA contract arm computes its own
// cap, and an unwired call site would go unnoticed.
func TestFillContractMarketCapsAppliesTheSameCeiling(t *testing.T) {
	const contractID = "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA"
	// 2,000,000,000 tokens at $1.5666 = $3.13B, against $3,791 of volume.
	total, _ := new(big.Int).SetString("20000000000000000", 10)
	price := "1.5666161388"
	vol := slvrVolumeUSD
	s := &Server{
		Options: Options{MinMarketCapVolumeUSD: 1000, MaxMarketCapVolumeRatio: 50_000, TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: total, Mint: total, Burn: big.NewInt(0), Clawback: big.NewInt(0), FlowCount: 7},
		}}},
		logger: slogDiscard(),
	}
	rows := []AssetDetail{{
		AssetID: contractID, Kind: "soroban_token", Decimals: 7,
		PriceUSD: &price, VolumeUSD24h: &vol,
	}}
	src := map[string]timescale.AssetRow{contractID: {SourceCount: intPtr(5)}}

	s.fillContractMarketCaps(context.Background(), rows, src, map[string]struct{}{contractID: {}})

	if rows[0].MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q on the contract arm; the ceiling is not wired here",
			*rows[0].MarketCapUSD)
	}
	if !rows[0].MarketCapLowLiquidity {
		t.Error("market_cap_low_liquidity is false on a suppressed contract-arm cap")
	}
	if rows[0].CirculatingSupply == nil {
		t.Error("circulating_supply must still surface on the contract arm too")
	}
}

func intPtr(i int) *int { return &i }
