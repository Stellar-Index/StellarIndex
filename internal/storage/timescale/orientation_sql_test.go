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

// TestNetworkStatsQueryFoldsOrientation pins that markets_count_24h counts DISTINCT canonical (base,
// quote) pairs, else a market stored in both orientations (XLM/USDC and USDC/XLM) is counted twice.
func TestNetworkStatsQueryFoldsOrientation(t *testing.T) {
	q := networkStatsQuery()
	if strings.Contains(q, "SELECT DISTINCT base_asset, quote_asset") {
		t.Errorf("markets_count_24h must not DISTINCT on raw base_asset/quote_asset — it double-counts a flipped-orientation market:\n%s", q)
	}
	canonBase, canonQuote, _ := canonOrientSQL(2)
	if !strings.Contains(q, "SELECT DISTINCT "+canonBase+" AS base_asset, "+canonQuote+" AS quote_asset") {
		t.Errorf("markets_count_24h must DISTINCT on canonOrientSQL's folded (base, quote):\n%s", q)
	}
}

// TestSourceStatsQueryFoldsOrientation pins the same fold in GetSourceStats: the groups per_pair emits must be
// the canonical (base, quote), or markets_24h is inflated by each flipped-orientation pair. A raw-column
// pre-aggregate beneath that level is fine.
func TestSourceStatsQueryFoldsOrientation(t *testing.T) {
	q := sourceStatsQuery()
	canonBase, canonQuote, _ := canonOrientSQL(1)
	canonGroup := "GROUP BY source, " + canonBase + ", " + canonQuote
	i := strings.Index(q, canonGroup)
	if i < 0 {
		t.Fatalf("per_pair must GROUP BY canonOrientSQL's folded (base, quote):\n%s", q)
	}
	if rest := strings.TrimSpace(q[i+len(canonGroup):]); !strings.HasPrefix(rest, ") per_pair") {
		t.Errorf("the folded GROUP BY must be the level that closes per_pair — anything after it can double-count a flipped-orientation market:\n%s", q)
	}
}
