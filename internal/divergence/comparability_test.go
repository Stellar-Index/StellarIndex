package divergence_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// A reference live by its own 26h budget but observed 20h ago must not
// vote beside fresh quotes against a minutes-wide VWAP: before, the two
// stale prints at 1.00 dragged the median of a 0.91 market to 0.955.
func TestCompare_StaleReferenceCannotVote(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stale := at.Add(-20 * time.Hour)
	refs := []divergence.Reference{
		&stubReference{name: "reflector-cex", price: 0.91},
		&stubReference{name: "coingecko", price: 0.91},
		&stubReference{name: "redstone", price: 1.00, asOf: stale},
		&stubReference{name: "band", price: 1.00, asOf: stale},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.91, at, divergence.CompareOptions{})

	if res.SuccessCount != 2 || res.Median != 0.91 || res.DivergencePct != 0 {
		t.Errorf("SuccessCount=%d Median=%v DivergencePct=%v, want 2 / 0.91 / 0", res.SuccessCount, res.Median, res.DivergencePct)
	}
	for _, name := range []string{"redstone", "band"} {
		if _, ok := res.Sources[name]; ok {
			t.Errorf("%s (observed 20h before the comparison) is in Sources", name)
		}
		if got := res.Failures[name]; got != "too_stale_to_compare" {
			t.Errorf("Failures[%s] = %q, want too_stale_to_compare", name, got)
		}
	}
}

// A quote with no observation time cannot be aged, so it cannot vote.
func TestCompare_QuoteWithoutAsOfCannotVote(t *testing.T) {
	ref := &undatedReference{}
	res := divergence.Compare(context.Background(), []divergence.Reference{ref}, xlmUSD(t), 1, time.Now(), divergence.CompareOptions{})
	if res.SuccessCount != 0 || res.Failures["undated"] != "too_stale_to_compare" {
		t.Errorf("undated quote: SuccessCount=%d Failures=%v, want 0 and too_stale_to_compare", res.SuccessCount, res.Failures)
	}
}

type undatedReference struct{}

func (*undatedReference) Name() string { return "undated" }

func (*undatedReference) LookupQuote(context.Context, canonical.Pair, time.Time) (divergence.Quote, error) {
	return divergence.Quote{Price: 1}, nil
}

// FX quotes pause over market closes; a fiat/fiat pair is held to the FX
// liveness budget, not the 1h crypto ceiling.
func TestMaxComparableAge_FXPairUsesFXBudget(t *testing.T) {
	eur, err := canonical.ParseAsset("fiat:EUR")
	if err != nil {
		t.Fatal(err)
	}
	fx := canonical.Pair{Base: eur, Quote: xlmUSD(t).Quote}
	if got := divergence.MaxComparableAge(fx); got != divergence.DefaultMaxComparableAgeFX {
		t.Errorf("MaxComparableAge(%s) = %v, want %v", fx, got, divergence.DefaultMaxComparableAgeFX)
	}
	if got := divergence.MaxComparableAge(xlmUSD(t)); got != divergence.DefaultMaxComparableAge {
		t.Errorf("MaxComparableAge(%s) = %v, want %v", xlmUSD(t), got, divergence.DefaultMaxComparableAge)
	}
}

// reflector-dex prices the SDEX book our VWAP is built from, so on an
// SDEX walk it "agrees" with us and disarmed the nobody-agrees leg: two
// independent references at -6% and +6% put the median on our price and
// only reflector-dex corroborated it.
func TestRefreshPair_ReflectorDEXCannotCorroborate(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: divergence.OracleSourceReflectorDEX, price: 1.10},
		&stubReference{name: divergence.OracleSourceReflectorCEX, price: 1.03},
		&stubReference{name: "coingecko", price: 1.17},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		WarningPersistence:   -1,
	})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.10, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := cached.Sources[divergence.OracleSourceReflectorDEX]; ok {
		t.Error("reflector-dex voted as an independent reference")
	}
	if !cached.WarningFired {
		t.Errorf("WarningFired = false with no independent reference within 5%% (sources %v)", cached.Sources)
	}
}
