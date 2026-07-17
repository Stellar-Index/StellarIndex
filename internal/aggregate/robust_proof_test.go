package aggregate

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// mkAggRow builds an aggregator-class OracleUpdate whose raw integer
// price is `priceScaled` at `decimals` fixed-point.
func mkAggRow(source string, priceScaled int64, decimals uint8) canonical.OracleUpdate {
	return canonical.OracleUpdate{
		Source:    source,
		Timestamp: time.Now().UTC(),
		Price:     canonical.NewAmount(big.NewInt(priceScaled)),
		Decimals:  decimals,
	}
}

// TestComputeGlobalPrice_AggregatorRejectsDivergentSource is the
// finding-M8 proof: the global Tier-2 headline averaged the aggregator
// sources with a PLAIN MEAN, so a single 2x-off print dragged the
// served price ~33%. Post-fix a median+MAD filter drops the divergent
// print and serves the consensus of the two agreeing sources.
func TestComputeGlobalPrice_AggregatorRejectsDivergentSource(t *testing.T) {
	reader := &stubGlobalReader{}
	reader.vwap.ok = false // force the aggregator tier
	reader.agg.rows = []canonical.OracleUpdate{
		mkAggRow("coingecko", 10000, 2),     // 100.00
		mkAggRow("coinmarketcap", 10020, 2), // 100.20 (agrees)
		mkAggRow("cryptocompare", 20000, 2), // 200.00 (2x-off outlier)
	}
	base, quote := usdcUSDPair(t)
	opts := DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko", "coinmarketcap", "cryptocompare"}

	res, err := ComputeGlobalPrice(context.Background(), base, quote, reader, opts)
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	// Plain mean (pre-fix) = (100.00 + 100.20 + 200.00)/3 = 133.40 — one
	// bad print moving the headline ~33%. Robust (post-fix) =
	// (100.00 + 100.20)/2 = 100.10.
	if res.Price != "100.10000000000000" {
		t.Fatalf("served price = %q, want 100.10000000000000 (consensus of the two agreeing sources, not the 133.40 inflated mean)", res.Price)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("contributing sources = %v, want 2 (the 200.00 outlier dropped)", res.Sources)
	}
}

// TestGuardServedVWAP_Catches5xManipulation is the finding-M11(a)
// proof: the serve-time guard only rejected >=10x deviations, so a 5x
// pump was served. Post-fix (ratio bound tightened 10x -> 3x) it is
// held, while a realistic large-but-real move inside the bound is
// still served.
func TestGuardServedVWAP_Catches5xManipulation(t *testing.T) {
	// A realistic liquid history: ~1% bucket-to-bucket noise around 100
	// (MAD small, so the ratio band governs).
	trailing := rats(t, "100.5", "99.7", "100.2", "99.9", "100.3",
		"99.8", "100.1", "100.0", "99.6", "100.4")
	// 5x pump.
	if accept, lkg := GuardServedVWAP(rat(t, "500.0"), trailing); accept || lkg < 0 {
		t.Fatalf("5x pump must be HELD post-fix (accept=%v lkg=%d)", accept, lkg)
	}
	// A genuine ~1.8x move — large but inside a 3x sanity bound — must
	// still be served (don't falsely reject real volatility).
	if accept, _ := GuardServedVWAP(rat(t, "180.0"), trailing); !accept {
		t.Fatal("realistic 1.8x move wrongly rejected (guard over-tightened)")
	}
}

// TestGuardServedVWAP_ThinHistoryNotFullyOpen is the finding-M11(b)
// proof: with fewer than guardMinSamples trailing buckets the guard
// failed FULLY OPEN — any manipulation passed. Post-fix a thin (but
// non-empty) baseline falls back to a wider-but-FINITE 10x band, so
// gross manipulation is still caught; only a truly empty baseline
// fails open.
func TestGuardServedVWAP_ThinHistoryNotFullyOpen(t *testing.T) {
	thin := repeatRat(t, "1.0", guardMinSamples-1) // 4 samples
	if accept, lkg := GuardServedVWAP(rat(t, "1000.0"), thin); accept || lkg < 0 {
		t.Fatalf("thin-history 1000x must be HELD (accept=%v lkg=%d), not fully-open", accept, lkg)
	}
	// A within-order-of-magnitude move on thin history is still served —
	// a scarce baseline is widened, never tightened.
	if accept, _ := GuardServedVWAP(rat(t, "5.0"), thin); !accept {
		t.Fatal("5x move on thin history should pass the wider thin-history band")
	}
	// No usable baseline at all → nothing to judge against → fail open.
	if accept, lkg := GuardServedVWAP(rat(t, "1000.0"), nil); !accept || lkg != -1 {
		t.Fatalf("empty baseline must fail open (accept=%v lkg=%d)", accept, lkg)
	}
}
