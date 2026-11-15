package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type fixedDecimals map[string]int

func (f fixedDecimals) Lookup(id string) (int, bool) { d, ok := f[id]; return d, ok }

// A venue's scale describes 7-decimal on-chain amounts. An 18-decimal token's leg is in its own units,
// so stating 7 makes a client report 10^11 times the real volume.
func TestLegScaleDecimals(t *testing.T) {
	token, err := canonical.NewSorobanAsset("CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA")
	if err != nil {
		t.Fatal(err)
	}
	lookup := fixedDecimals{token.String(): 18}
	xlm := canonical.NativeAsset()
	for _, tc := range []struct {
		name    string
		leg     canonical.Asset
		stated  int
		sources []string
		want    int
	}{
		{"standard leg keeps the venue scale", xlm, 7, []string{"soroswap"}, 7},
		{"standard leg keeps a CEX scale", xlm, 8, []string{"binance", "sdex"}, 8},
		{"nonstandard leg on chain states its own decimals", token, 7, []string{"soroswap", "sdex"}, 18},
		{"nonstandard leg with an off-chain venue has no scale", token, 8, []string{"soroswap", "binance"}, ohlcBarScaleUnknown},
		{"an unknown bar scale stays unknown", token, ohlcBarScaleUnknown, []string{"soroswap"}, ohlcBarScaleUnknown},
	} {
		if got := legScaleDecimals(lookup, tc.leg, tc.stated, tc.sources); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := legScaleDecimals(nil, token, 7, []string{"soroswap"}); got != 7 {
		t.Errorf("no lookup wired: got %d, want the venue scale 7", got)
	}

	bars := []OHLCSeriesBar{{Sources: []string{"soroswap"}}}
	pair, err := canonical.NewPair(xlm, token)
	if err != nil {
		t.Fatal(err)
	}
	annotateOHLCSeriesBarScale(bars, lookup, pair)
	if bars[0].VBaseDecimals == nil || *bars[0].VBaseDecimals != 7 || bars[0].VQuoteDecimals == nil || *bars[0].VQuoteDecimals != 18 {
		t.Errorf("series bar scales = %v / %v, want 7 / 18", bars[0].VBaseDecimals, bars[0].VQuoteDecimals)
	}
}
