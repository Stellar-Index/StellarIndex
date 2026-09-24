package aggregate

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Exact-boundary pins for the served-price guards: each case sits ON a
// band edge, so an off-by-one comparison or a changed constant moves it.

func TestFilterOutliers_BandEdgesAreInclusiveAndRatioSymmetric(t *testing.T) {
	// Majority at 100 → MAD 0 → scale = 100/200 = 0.5 → threshold at
	// sigma 4 is 2, so the band is [100²/102, 102] exactly.
	mk := func(hash string, base, quote int64) canonical.Trade {
		tr := mkOrderedTrade("s", 1, hash, 0, time.Unix(0, 0), quote)
		tr.BaseAmount = canonical.NewAmount(big.NewInt(base))
		return tr
	}
	// The centre outweighs every probe, so a dropped probe never trips
	// the volume-majority withhold.
	centre := []canonical.Trade{mk("a", 1000, 100000), mk("b", 1000, 100000), mk("c", 1000, 100000), mk("d", 1000, 100000)}
	cases := []struct {
		name        string
		base, quote int64
		keep        bool
	}{
		{"upper edge 102", 1, 102, true},
		{"just above 102", 1000, 102001, false},
		{"lower edge 100²/102", 102, 10000, true},
		{"just below 100²/102", 103, 10000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append(append([]canonical.Trade{}, centre...), mk("x", tc.base, tc.quote))
			got := FilterOutliers(in, 4)
			kept := len(got) == 5
			if kept != tc.keep || len(got) < 4 {
				t.Fatalf("kept %d/5, want probe kept=%v", len(got), tc.keep)
			}
		})
	}
}

func TestRejectAggregatorOutliers_BandEdgeIsInclusive(t *testing.T) {
	// Majority at 4000 → MAD 0 → scale = 20 → half-width 5·20 = 100.
	for _, tc := range []struct {
		probe int64
		keep  bool
	}{{4100, true}, {4101, false}} {
		rows := []canonical.OracleUpdate{
			mkAggRow("a", 4000, 2), mkAggRow("b", 4000, 2), mkAggRow("c", tc.probe, 2),
		}
		got := rejectAggregatorOutliers(rows)
		if kept := len(got) == 3; kept != tc.keep {
			t.Fatalf("probe %d: kept %d/3, want probe kept=%v", tc.probe, len(got), tc.keep)
		}
	}
}

func TestFilterFreshAggregatorRows_ZeroMaxAgeKeepsEverything(t *testing.T) {
	old := mkAggRow("a", 1, 2)
	old.Timestamp = time.Unix(0, 0)
	if got := filterFreshAggregatorRows([]canonical.OracleUpdate{old}, 0); len(got) != 1 {
		t.Fatalf("maxAge 0 must not filter; kept %d/1", len(got))
	}
	if got := filterFreshAggregatorRows([]canonical.OracleUpdate{old}, time.Hour); len(got) != 0 {
		t.Fatalf("a 1970 row must be stale at maxAge 1h; kept %d/1", len(got))
	}
}

func TestComputeGlobalPrice_VWAPTierAcceptsExactlyMinTradeCount(t *testing.T) {
	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	r := &stubGlobalReader{}
	r.vwap.price, r.vwap.ok, r.vwap.tradeCount = "1.0", true, opts.VWAPMinTradeCount
	got, err := ComputeGlobalPrice(context.Background(), base, quote, r, opts)
	if err != nil || got.Authority != AuthorityVWAPNative {
		t.Fatalf("tradeCount == floor: (%+v, %v), want the VWAP tier", got, err)
	}
}
