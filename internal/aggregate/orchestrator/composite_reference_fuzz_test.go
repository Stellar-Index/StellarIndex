package orchestrator

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestReleaseCorroborated_CrossOracleBandIsInclusive pins the 5 % cross-
// oracle release band at its edge on both sides of the median: a candidate
// exactly 5 % away agrees, one a hair further does not.
func TestReleaseCorroborated_CrossOracleBandIsInclusive(t *testing.T) {
	c := confidenceComputation{CrossOracleMedian: 100}
	for _, tc := range []struct {
		cand *big.Rat
		want bool
	}{
		{big.NewRat(105, 1), true},
		{big.NewRat(95, 1), true},
		{big.NewRat(10501, 100), false},
		{big.NewRat(9499, 100), false},
	} {
		if got := releaseCorroborated(c, tc.cand, compositeReference{}, DefaultCompositeReferenceReleaseBandPct); got != tc.want {
			t.Errorf("candidate %s vs median 100: release=%v, want %v", tc.cand.FloatString(2), got, tc.want)
		}
	}
}

// FuzzResolveCompositeReference checks the current-bucket reference
// against an exact big.Rat model: the composite is the exact product of
// the legs, the verdict is corroborated iff |direct − composite| /
// composite <= ToleranceBps in exact arithmetic (either side of the
// composite), a thin or dispersed leg or an FX snap older than FXMaxAge
// is unavailable, and a direct price far past 2^63 is judged on its value.
func FuzzResolveCompositeReference(f *testing.F) {
	// directNum, directDen, directShift, legNum, legDen, fxNum, fxDen, fxAgeSec, legSources, dispersionBps
	f.Add(uint64(8), uint64(100), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(3600), uint8(3), uint16(0))
	f.Add(uint64(80600), uint64(1_000_000), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(3600), uint8(3), uint16(0))
	f.Add(uint64(79400), uint64(1_000_000), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(3600), uint8(3), uint16(0))
	f.Add(uint64(79392), uint64(1_000_000), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(3600), uint8(3), uint16(0))
	f.Add(uint64(4), uint64(100), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(3600), uint8(3), uint16(0))
	f.Add(uint64(8), uint64(100), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(76*3600), uint8(3), uint16(0))
	f.Add(uint64(8), uint64(100), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(76*3600+1), uint8(3), uint16(0))
	f.Add(uint64(8), uint64(100), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(3600), uint8(1), uint16(0))
	f.Add(uint64(8), uint64(100), uint8(0), uint32(10), uint32(100), uint32(80), uint32(100), int64(3600), uint8(3), uint16(76))
	f.Add(uint64(1)<<63, uint64(1), uint8(40), uint32(1)<<31, uint32(1), uint32(1)<<31, uint32(1), int64(0), uint8(2), uint16(75))
	f.Fuzz(func(t *testing.T, directNum, directDen uint64, directShift uint8, legNum, legDen, fxNum, fxDen uint32,
		fxAgeSec int64, legSources uint8, dispersionBps uint16,
	) {
		if directDen == 0 || legNum == 0 || legDen == 0 || fxNum == 0 || fxDen == 0 || fxAgeSec < 0 || fxAgeSec > 1<<32 {
			return
		}
		xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
		usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
		xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
		window := time.Minute
		now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		legPrice := new(big.Rat).SetFrac(new(big.Int).SetUint64(uint64(legNum)), new(big.Int).SetUint64(uint64(legDen)))
		fxPrice := new(big.Rat).SetFrac(new(big.Int).SetUint64(uint64(fxNum)), new(big.Int).SetUint64(uint64(fxDen)))
		num := new(big.Int).Lsh(new(big.Int).SetUint64(directNum), uint(directShift%96))
		direct := new(big.Rat).SetFrac(num, new(big.Int).SetUint64(directDen))

		o := New(nil, nil, Config{
			Windows:            []time.Duration{window},
			Triangulations:     []TriangulationChain{{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}},
			FXStore:            &fakeFXStore{quote: fxPrice, observedAt: now.Add(-time.Duration(fxAgeSec) * time.Second), source: "massive"},
			CompositeReference: CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmGBP}},
		})
		lr := legRef{price: legPrice, sources: int(legSources % 5)}
		if dispersionBps > 0 {
			lr.dispersion = big.NewRat(int64(dispersionBps), 10_000)
		}
		o.tickLegRefs = map[time.Duration]map[string]legRef{window: {xlmUSD.String(): lr}}

		ref := o.resolveCompositeReference(context.Background(), xlmGBP, window, now, direct)

		wantUnavailable := ""
		switch {
		case direct.Sign() <= 0:
			wantUnavailable = "direct_non_positive"
		case lr.sources < DefaultCompositeReferenceMinLegSources:
			wantUnavailable = fmt.Sprintf("leg_sources=%d", lr.sources)
		case lr.dispersion != nil && lr.dispersion.Cmp(big.NewRat(DefaultCompositeReferenceToleranceBps, 10_000)) > 0:
			wantUnavailable = "leg_dispersion="
		case time.Duration(fxAgeSec)*time.Second > DefaultCompositeReferenceFXMaxAge:
			wantUnavailable = "fx_stale"
		}
		if wantUnavailable != "" {
			if ref.verdict != compositeVerdictUnavailable || !strings.HasPrefix(ref.unavailable, wantUnavailable) || ref.price != nil {
				t.Fatalf("verdict %q unavailable=%q price=%v, want unavailable %q", ref.verdict, ref.unavailable, ref.price, wantUnavailable)
			}
			return
		}
		composite := new(big.Rat).Mul(legPrice, fxPrice)
		cf, _ := composite.Float64()
		df, _ := direct.Float64()
		if cf <= 0 || df <= 0 || math.IsInf(cf, 0) || math.IsInf(df, 0) {
			if ref.verdict != compositeVerdictUnavailable || ref.unavailable != "non_finite" {
				t.Fatalf("non-finite float view: verdict %q unavailable=%q", ref.verdict, ref.unavailable)
			}
			return
		}
		if ref.price == nil || ref.price.Cmp(composite) != 0 {
			t.Fatalf("composite price %v, want exact product %v", ref.price, composite)
		}
		dev := new(big.Rat).Sub(direct, composite)
		dev.Abs(dev).Quo(dev, composite)
		want := compositeVerdictRefuted
		if dev.Cmp(big.NewRat(DefaultCompositeReferenceToleranceBps, 10_000)) <= 0 {
			want = compositeVerdictCorroborated
		}
		if ref.verdict != want {
			t.Fatalf("direct %s vs composite %s (dev %s): verdict %q, want %q",
				direct.FloatString(12), composite.FloatString(12), dev.FloatString(8), ref.verdict, want)
		}
		if ref.divergencePct < 0 || math.IsNaN(ref.divergencePct) {
			t.Fatalf("divergencePct %v must be a non-negative magnitude", ref.divergencePct)
		}
	})
}
