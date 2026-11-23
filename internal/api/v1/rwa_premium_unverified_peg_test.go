package v1_test

import (
	"slices"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRWAPremium_AnUnverifiedPegIsNeverPooledAsADollar: a declared USD peg the verified catalogue cannot bind has no market
// that could show it off its peg, so its trades never enter the dollar VWAP; the verified peg's still do.
func TestRWAPremium_AnUnverifiedPegIsNeverPooledAsADollar(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	usdc := installPegAliasRegistry(t)
	usdx := mustClassicAsset(t, "USDX", pegAliasAquaIssuer)
	id := premAssetID("USTRY", rwaGoodIssuer)
	market := &stubMarketHistory{rows: []timescale.MarketDay{premMarket(id, 1, "1.08"), pegDay(1, "1.0")}}
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
		OracleHistory:      &stubOracleHistory{rows: []timescale.OracleDayPoint{histOracle("redstone", "USTRY", 1, 107000000)}},
		MarketHistory:      market,
		USDPeggedClassics:  []canonical.Asset{usdc, usdx},
		VerifiedCurrencies: newTestCatalogue(t),
	})
	getRWAPremiumEnvelope(t, srv)

	if !slices.Contains(market.askedQuotes, usdc.String()) {
		t.Errorf("asked quotes = %v, want the verified peg %s pooled", market.askedQuotes, usdc)
	}
	for _, q := range market.askedQuotes {
		if strings.HasPrefix(q, "USDX") {
			t.Errorf("asked quotes = %v: the unverified peg %s was pooled as a dollar", market.askedQuotes, usdx)
		}
	}
}
