package timescale

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The SQL rank must list every declared stablecoin SAC at rank 3, as
// canonical.quoteRank does, and render no empty IN list without one.
func TestQuoteRankSQL_DeclaredStablecoinSAC(t *testing.T) {
	const usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	if q := quoteRankSQL("base_asset", 1); strings.Contains(q, usdcSAC) || strings.Contains(q, "IN ()") {
		t.Fatalf("without a registry the rank must not list a SAC: %s", q)
	}
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase,
		map[string]string{usdcSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"})
	if err != nil {
		t.Fatal(err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	if q := quoteRankSQL("base_asset", 1); !strings.Contains(q, "WHEN base_asset IN ('"+usdcSAC+"') THEN 3") {
		t.Errorf("rank SQL lacks the declared USDC SAC at rank 3: %s", q)
	}
}

// The tie-break must compare bytes, like Go's string order, not the
// database's default collation.
func TestCanonOrientSQL_TieBreakIsByteOrder(t *testing.T) {
	_, _, flipped := canonOrientSQL(1)
	if !strings.Contains(flipped, `base_asset COLLATE "C" > quote_asset COLLATE "C"`) {
		t.Errorf("flipped tie-break is not byte-ordered: %s", flipped)
	}
}
