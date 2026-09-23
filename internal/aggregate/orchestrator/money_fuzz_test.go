package orchestrator

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fuzzAssets is a small closed alphabet so fuzzed chains and edges collide
// often enough to exercise the equality branches.
func fuzzAssets(t *testing.T) []canonical.Asset {
	t.Helper()
	out := make([]canonical.Asset, 0, 5)
	for _, s := range []string{"crypto:XLM", "crypto:BTC", "fiat:USD", "fiat:EUR", "fiat:GBP"} {
		a, err := canonical.ParseAsset(s)
		if err != nil {
			t.Fatalf("ParseAsset %s: %v", s, err)
		}
		out = append(out, a)
	}
	return out
}

// fuzzPairs decodes byte pairs into canonical pairs over fuzzAssets,
// skipping self-pairs NewPair rejects.
func fuzzPairs(t *testing.T, assets []canonical.Asset, raw []byte) []canonical.Pair {
	t.Helper()
	var out []canonical.Pair
	for i := 0; i+1 < len(raw); i += 2 {
		p, err := canonical.NewPair(assets[int(raw[i])%len(assets)], assets[int(raw[i+1])%len(assets)])
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// FuzzValidateTriangulationChain checks the chain validator against its
// structural definition: at least two legs, endpoints matching the target,
// and every adjacent pair of legs sharing its pivot asset.
func FuzzValidateTriangulationChain(f *testing.F) {
	f.Add(byte(0), byte(4), []byte{0, 2, 2, 4})
	f.Add(byte(0), byte(4), []byte{0, 2, 3, 4})
	f.Add(byte(0), byte(4), []byte{0, 1, 1, 2, 2, 4})
	f.Add(byte(0), byte(4), []byte{0, 1, 1, 2, 3, 4})
	f.Add(byte(0), byte(2), []byte{0, 2})
	f.Fuzz(func(t *testing.T, tb, tq byte, rawLegs []byte) {
		assets := fuzzAssets(t)
		target, err := canonical.NewPair(assets[int(tb)%len(assets)], assets[int(tq)%len(assets)])
		if err != nil {
			return
		}
		legs := fuzzPairs(t, assets, rawLegs)
		want := len(legs) >= 2 &&
			legs[0].Base.Equal(target.Base) &&
			legs[len(legs)-1].Quote.Equal(target.Quote)
		for i := 1; want && i < len(legs); i++ {
			want = legs[i-1].Quote.Equal(legs[i].Base)
		}
		got := ValidateTriangulationChain(TriangulationChain{Target: target, Legs: legs}) == nil
		if got != want {
			t.Fatalf("ValidateTriangulationChain(%s, %v) valid=%v, want %v", target, legs, got, want)
		}
	})
}

// FuzzExcludeDirectEdge checks the composite never prices through the
// target's own market in either direction, and that every other edge
// survives in order.
func FuzzExcludeDirectEdge(f *testing.F) {
	f.Add(byte(0), byte(2), []byte{0, 2, 2, 0, 0, 1, 1, 2})
	f.Add(byte(1), byte(3), []byte{3, 1, 1, 3, 1, 3})
	f.Fuzz(func(t *testing.T, tb, tq byte, rawEdges []byte) {
		assets := fuzzAssets(t)
		target, err := canonical.NewPair(assets[int(tb)%len(assets)], assets[int(tq)%len(assets)])
		if err != nil {
			return
		}
		var edges []aggregate.RouteLeg
		for i := 0; i+1 < len(rawEdges); i += 2 {
			edges = append(edges, aggregate.RouteLeg{
				From: assets[int(rawEdges[i])%len(assets)], To: assets[int(rawEdges[i+1])%len(assets)],
				Price: big.NewRat(int64(i+1), 1),
			})
		}
		direct := func(e aggregate.RouteLeg) bool {
			return (e.From.Equal(target.Base) && e.To.Equal(target.Quote)) ||
				(e.From.Equal(target.Quote) && e.To.Equal(target.Base))
		}
		got := excludeDirectEdge(edges, target)
		j := 0
		for _, e := range edges {
			if direct(e) {
				continue
			}
			if j >= len(got) || got[j].Price.Cmp(e.Price) != 0 {
				t.Fatalf("edge %v→%v missing or reordered", e.From, e.To)
			}
			j++
		}
		if j != len(got) {
			t.Fatalf("excludeDirectEdge kept %d edges, want %d", len(got), j)
		}
	})
}

// FuzzPhase2FreezeFires checks the ADR-0019 3-signal AND: agreement with
// the strict/inclusive comparisons as written in the ADR, and monotonicity
// (a bucket that is less confident, more anomalous, or thinner than a
// firing one also fires).
func FuzzPhase2FreezeFires(f *testing.F) {
	f.Add(0.0, 0.0, 0, 0.1, 6.0, 1, 0.1, 1.0, 1)
	f.Add(0.25, 5.0, 1, 0.25, 5.0, 1, 0.0, 0.0, 0)
	f.Add(0.5, 3.0, 2, 0.49, 3.01, 2, 0.01, 0.5, 1)
	f.Fuzz(func(t *testing.T, cMax, zMin float64, nMax int, conf, z float64, n int, dConf, dZ float64, dN int) {
		th := Phase2Thresholds{ConfidenceMaxFreeze: cMax, ZScoreMinFreeze: zMin, SourceCountMaxFreeze: nMax}
		eff := th.withDefaults()
		in := confidenceWithSourceCount{Confidence: conf, ZScore: z, SourceCount: n}
		got := phase2FreezeFires(in, th)
		want := conf < eff.ConfidenceMaxFreeze && z > eff.ZScoreMinFreeze && n <= eff.SourceCountMaxFreeze
		if got != want {
			t.Fatalf("phase2FreezeFires(%+v, %+v) = %v, want %v", in, eff, got, want)
		}
		if !got || math.IsNaN(dConf) || math.IsNaN(dZ) {
			return
		}
		worse := confidenceWithSourceCount{
			Confidence:  conf - math.Abs(dConf),
			ZScore:      z + math.Abs(dZ),
			SourceCount: n - int(uint(dN)%4),
		}
		if worse.SourceCount > n || !phase2FreezeFires(worse, th) {
			t.Fatalf("fires at %+v but not at the worse bucket %+v", in, worse)
		}
	})
}

// FuzzReleaseCorroborated checks the release lens: a no-lens bucket never
// releases; each lens agrees symmetrically (a candidate x% above its
// reference and one x% below read the same); and agreement is monotone —
// shrinking the disagreement never turns a release into a hold.
func FuzzReleaseCorroborated(f *testing.F) {
	f.Add(true, 3.0, 0.0, 0.0, false, 2.0)
	f.Add(true, 5.0, 0.1, 0.04, false, 0.0)
	f.Add(true, -5.0, 0.1, -0.06, false, 0.0)
	f.Add(true, -6.0, 0.1, 0.0, false, 0.0)
	f.Add(false, 0.0, 250.0, 0.05, true, 2.0)
	f.Add(true, -4.9, 100.0, 0.051, true, 2.1)
	f.Fuzz(func(t *testing.T, triChecked bool, triPct, median, relOff float64, refResolved bool, refPct float64) {
		for _, v := range []float64{triPct, median, relOff, refPct} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e12 {
				return
			}
		}
		ref := compositeReference{verdict: compositeVerdictUnavailable}
		if refResolved {
			ref = compositeReference{verdict: compositeVerdictRefuted, divergencePct: math.Abs(refPct)}
		}
		c := confidenceComputation{TriangulationChecked: triChecked, TriangulationDivergencePct: triPct}
		band := DefaultCompositeReferenceReleaseBandPct

		// Triangulation / composite lens alone (no cross-oracle median).
		got := releaseCorroborated(c, nil, ref, band)
		want := (refResolved && math.Abs(refPct) <= band) ||
			(!refResolved && triChecked && math.Abs(triPct) <= releaseAgreementMaxPct)
		if got != want {
			t.Fatalf("lens-only release=%v, want %v (tri %v/%v ref %v/%v)", got, want, triChecked, triPct, refResolved, refPct)
		}
		mirrored := c
		mirrored.TriangulationDivergencePct = -triPct
		if releaseCorroborated(mirrored, nil, ref, band) != got {
			t.Fatalf("triangulation lens not symmetric in the sign of %v", triPct)
		}

		// Cross-oracle lens alone: candidate = median × (1 ± relOff).
		if median < 1e-12 || math.Abs(relOff) >= 1 {
			return
		}
		m := median
		up := releaseCorroborated(confidenceComputation{CrossOracleMedian: m}, floatRat(t, m*(1+math.Abs(relOff))), compositeReference{}, band)
		down := releaseCorroborated(confidenceComputation{CrossOracleMedian: m}, floatRat(t, m*(1-math.Abs(relOff))), compositeReference{}, band)
		exact := releaseCorroborated(confidenceComputation{CrossOracleMedian: m}, floatRat(t, m), compositeReference{}, band)
		if !exact {
			t.Fatalf("candidate equal to the cross-oracle median %v did not release", m)
		}
		pct := math.Abs(relOff) * 100
		if pct < releaseAgreementMaxPct*0.999 && (!up || !down) {
			t.Fatalf("candidate %.4f%% off median %v must release both sides (up=%v down=%v)", pct, m, up, down)
		}
		if pct > releaseAgreementMaxPct*1.001 && (up || down) {
			t.Fatalf("candidate %.4f%% off median %v must hold both sides (up=%v down=%v)", pct, m, up, down)
		}
		if releaseCorroborated(confidenceComputation{CrossOracleMedian: m}, nil, compositeReference{}, band) {
			t.Fatal("a nil candidate released on the cross-oracle lens")
		}
	})
}

func floatRat(t *testing.T, v float64) *big.Rat {
	t.Helper()
	r := new(big.Rat)
	if r.SetFloat64(v) == nil {
		t.Fatalf("SetFloat64(%v)", v)
	}
	return r
}

// FuzzLegDispersion checks the leg-dispersion statistic against an exact
// big.Rat reference (max |venueVWAP − legVWAP| / legVWAP), and that it is
// invariant under scaling every amount by 2^k — amounts are canonical
// big.Int and a scale well past 2^63 must not change the answer (ADR-0003).
func FuzzLegDispersion(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1}, []byte{10, 20, 30, 40}, []byte{15, 15, 45, 60}, uint8(0))
	f.Add([]byte{0, 1, 2}, []byte{1, 1, 1}, []byte{100, 101, 99}, uint8(70))
	f.Add([]byte{0, 0}, []byte{5, 7}, []byte{9, 9}, uint8(0))
	// A dominant venue at 1.0 and a thin one at 0.5: the worst deviation
	// is BELOW the leg VWAP, so it only shows as a magnitude.
	f.Add([]byte{0, 1}, []byte{99, 9}, []byte{99, 4}, uint8(0))
	f.Fuzz(func(t *testing.T, venues, bases, quotes []byte, shift uint8) {
		n := min(len(venues), len(bases), len(quotes), 12)
		if n == 0 {
			return
		}
		names := []string{"kraken", "coinbase", "binance"}
		pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
		scale := new(big.Int).Lsh(big.NewInt(1), uint(shift%100))
		build := func(mult *big.Int) []canonical.Trade {
			out := make([]canonical.Trade, 0, n)
			for i := 0; i < n; i++ {
				tr := makeTradeOn(t, pair, names[int(venues[i])%len(names)], int64(bases[i])+1, int64(quotes[i])+1, time.Unix(0, 0))
				tr.BaseAmount = canonical.NewAmount(new(big.Int).Mul(tr.BaseAmount.BigInt(), mult))
				tr.QuoteAmount = canonical.NewAmount(new(big.Int).Mul(tr.QuoteAmount.BigInt(), mult))
				out = append(out, tr)
			}
			return out
		}
		o := New(nil, nil, Config{})
		trades := build(big.NewInt(1))
		vwap, err := o.computeNormalizedVWAP(trades, pair)
		if err != nil {
			t.Fatalf("computeNormalizedVWAP: %v", err)
		}
		got, uncomputable := o.legDispersion(pair, trades, vwap)
		if uncomputable {
			t.Fatal("positive-amount trades reported uncomputable")
		}

		byVenue := map[string][]canonical.Trade{}
		for _, tr := range trades {
			byVenue[tr.Source] = append(byVenue[tr.Source], tr)
		}
		if len(byVenue) < 2 {
			if got != nil {
				t.Fatalf("single-venue bucket reported dispersion %v", got)
			}
			return
		}
		want := new(big.Rat)
		for _, vt := range byVenue {
			v, err := o.computeNormalizedVWAP(vt, pair)
			if err != nil {
				t.Fatalf("venue VWAP: %v", err)
			}
			dev := new(big.Rat).Sub(v, vwap)
			dev.Abs(dev).Quo(dev, vwap)
			if dev.Cmp(want) > 0 {
				want = dev
			}
		}
		if got == nil || got.Cmp(want) != 0 {
			t.Fatalf("dispersion %v, want %v", got, want)
		}

		scaled := build(scale)
		svwap, err := o.computeNormalizedVWAP(scaled, pair)
		if err != nil {
			t.Fatalf("scaled VWAP: %v", err)
		}
		if svwap.Cmp(vwap) != 0 {
			t.Fatalf("VWAP changed under ×2^%d amount scaling: %v -> %v", shift%100, vwap, svwap)
		}
		sgot, _ := o.legDispersion(pair, scaled, svwap)
		if sgot == nil || sgot.Cmp(got) != 0 {
			t.Fatalf("dispersion changed under ×2^%d amount scaling: %v -> %v", shift%100, got, sgot)
		}
	})
}

// FuzzRefreshOrder checks the refresh reordering is a stable partition of
// cfg.Pairs: same multiset, allow-listed targets' non-FX legs first, each
// group in its original order, and the identity when the mechanism is off.
func FuzzRefreshOrder(f *testing.F) {
	f.Add(true, []byte{0, 4, 0, 2, 2, 4, 1, 2}, []byte{0, 2, 2, 4}, byte(0), byte(4))
	f.Add(false, []byte{0, 4, 0, 2, 2, 4}, []byte{0, 2, 2, 4}, byte(0), byte(4))
	f.Fuzz(func(t *testing.T, enabled bool, rawPairs, rawLegs []byte, tb, tq byte) {
		assets := fuzzAssets(t)
		target, err := canonical.NewPair(assets[int(tb)%len(assets)], assets[int(tq)%len(assets)])
		if err != nil {
			return
		}
		cfg := Config{
			Pairs:              fuzzPairs(t, assets, rawPairs),
			Triangulations:     []TriangulationChain{{Target: target, Legs: fuzzPairs(t, assets, rawLegs)}},
			CompositeReference: CompositeReferenceConfig{Enabled: enabled, Targets: []canonical.Pair{target}},
		}
		got := refreshOrder(cfg)
		if !enabled {
			if len(got) != len(cfg.Pairs) {
				t.Fatalf("disabled reorder changed length %d -> %d", len(cfg.Pairs), len(got))
			}
			for i := range got {
				if got[i] != cfg.Pairs[i] {
					t.Fatalf("disabled reorder moved %s", got[i])
				}
			}
			return
		}
		isLeg := func(p canonical.Pair) bool {
			for _, l := range cfg.Triangulations[0].Legs {
				if l.Base.Equal(p.Base) && l.Quote.Equal(p.Quote) && !isFXLeg(l) {
					return true
				}
			}
			return false
		}
		var want []canonical.Pair
		for _, p := range cfg.Pairs {
			if isLeg(p) {
				want = append(want, p)
			}
		}
		for _, p := range cfg.Pairs {
			if !isLeg(p) {
				want = append(want, p)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("refreshOrder returned %d pairs, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("refreshOrder[%d] = %s, want %s (got %v)", i, got[i], want[i], got)
			}
		}
	})
}

// FuzzEdgeConfidence checks an unscorable edge can never out-rank the
// bootstrap cap, and a scored edge carries its own score unchanged.
func FuzzEdgeConfidence(f *testing.F) {
	f.Add(true, 0.83, []byte{0, 1, 2})
	f.Add(false, 0.83, []byte{0, 1, 2, 3, 4, 5})
	f.Add(false, 0.0, []byte{})
	f.Fuzz(func(t *testing.T, confOK bool, score float64, venues []byte) {
		pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
		trades := make([]canonical.Trade, 0, len(venues))
		for _, v := range venues {
			trades = append(trades, makeTradeOn(t, pair, string(rune('a'+v%26)), 1, 1, time.Unix(0, 0)))
		}
		c := confidenceComputation{Score: confidence.Score{Confidence: score}}
		got := edgeConfidence(c, confOK, trades)
		if confOK {
			if got != score && !(math.IsNaN(got) && math.IsNaN(score)) {
				t.Fatalf("scored edge confidence %v, want its score %v", got, score)
			}
			return
		}
		want := math.Min(confidence.SourceCountFactor(distinctSourceCount(trades)), confidence.BootstrapConfidenceCap)
		if got != want || got > confidence.BootstrapConfidenceCap {
			t.Fatalf("unscored edge confidence %v, want %v (cap %v)", got, want, confidence.BootstrapConfidenceCap)
		}
	})
}
