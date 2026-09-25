package timescale

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The monthly fold keys every spelling of an asset onto ONE canonical
// id: all three XLM forms land under 'native' (the row an XLM-quoted
// asset will be priced through), a classic id is its own key, and a
// spelling that is not an asset id at all folds with nothing.
func TestMonthlyUSDVWAPAsset_FoldsEverySpellingOntoTheCanonicalForm(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	for raw, want := range map[string]string{
		"native":                   "native",
		"crypto:XLM":               "native",
		canonical.XLMSacContractID: "native",
		usdc:                       usdc,
		"not an asset id":          "not an asset id",
	} {
		if got := monthlyUSDVWAPAsset(raw); got != want {
			t.Errorf("monthlyUSDVWAPAsset(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The cohort price join matches a month price to the flows on the asset
// spelling alone, and the two sides spell a SAC-wrapped classic asset
// independently: the movements archive writes a verified SAC transfer
// under its classic CODE-ISSUER (chops.cap67AssetName, pinned by
// TestCap67AssetName with this same USDC SAC), and this fold keys the SAC
// spelling through the installed registry. Pin that both land on the
// same id, so a registered SAC's month is never priced under a C… id no
// flow row carries.
func TestMonthlyUSDVWAPAsset_RegisteredSACFoldsOntoTheMovementsSpelling(t *testing.T) {
	const (
		usdcSAC      = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		usdcMovement = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	reg, err := canonical.NewAliasRegistry(map[string]string{usdcSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"})
	if err != nil {
		t.Fatal(err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
	for _, raw := range []string{usdcSAC, usdcMovement} {
		if got := monthlyUSDVWAPAsset(raw); got != usdcMovement {
			t.Errorf("monthlyUSDVWAPAsset(%q) = %q, want the movements archive's %q", raw, got, usdcMovement)
		}
	}
}

// A canonical asset's spellings are stored at different scales: on-chain
// 'native' in stroops (10^7), the CEX feeds' crypto:XLM at 10^8. The fold
// weights each row by its WHOLE-unit volume, so the same 1B XLM traded on
// each side weighs the same. Weighting by raw stored volume would give
// the CEX month ten times the weight: (0.3×1e16 + 0.31×1e17) / 1.1e17 =
// 0.30909…, where the union market's price is 0.305.
func TestFoldMonthlyUSDVWAPs_WeightsSpellingsByWholeUnits(t *testing.T) {
	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rat := func(s string) *big.Rat {
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("bad rat %q", s)
		}
		return r
	}
	out, err := foldMonthlyUSDVWAPs([]monthlyUSDVWAPRow{
		{month: may, base: "native", vwap: rat("0.3"), vol: rat("10000000000000000"), usdVol: rat("0"), sources: []string{"sdex"}},
		{month: may, base: "crypto:XLM", vwap: rat("0.31"), vol: rat("100000000000000000"), usdVol: rat("0"), sources: []string{"coinbase", "kraken"}},
	}, testSourceDecimals)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Asset != "native" || out[0].VWAPUSD != "0.305" {
		t.Fatalf("fold = %+v, want one native row at 0.305", out)
	}
}

// testSourceDecimals mirrors the source registry's scales: CEX 10^8, FX
// 10^6, on-chain DEX 10^7.
func testSourceDecimals(source string) int {
	switch source {
	case "massive":
		return 6
	case "sdex":
		return 7
	}
	return 8
}

// A row whose sources stamp amounts at different scales is not one
// market's volume, and a row with no source (or no lookup) has no scale
// to read: each fails the fold rather than weigh it by a guessed unit.
func TestFoldMonthlyUSDVWAPs_RefusesAnUnknowableScale(t *testing.T) {
	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		sources []string
		lookup  SourceAmountDecimals
	}{
		"mixed CEX and FX scales": {[]string{"coinbase", "massive"}, testSourceDecimals},
		"no source":               {nil, testSourceDecimals},
		"no lookup":               {[]string{"coinbase"}, nil},
	} {
		_, err := foldMonthlyUSDVWAPs([]monthlyUSDVWAPRow{
			{month: may, base: "crypto:XLM", vwap: big.NewRat(31, 100), vol: big.NewRat(1e8, 1), usdVol: new(big.Rat), sources: tc.sources},
		}, tc.lookup)
		if err == nil {
			t.Errorf("%s: fold succeeded, want an error", name)
		}
	}
}
