package aggregate

import (
	"math/big"
	"testing"
)

// rat parses a decimal string into a *big.Rat for test fixtures.
func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad rat literal %q", s)
	}
	return r
}

// rats builds a newest-first trailing slice from decimal strings.
func rats(t *testing.T, ss ...string) []*big.Rat {
	t.Helper()
	out := make([]*big.Rat, len(ss))
	for i, s := range ss {
		out[i] = rat(t, s)
	}
	return out
}

// repeatRat returns n copies of value v.
func repeatRat(t *testing.T, v string, n int) []*big.Rat {
	t.Helper()
	out := make([]*big.Rat, n)
	for i := range out {
		out[i] = rat(t, v)
	}
	return out
}

func TestGuardServedVWAP_NormalBucketPasses(t *testing.T) {
	// Flat ~1.0 history; candidate a hair above → sane, serve candidate.
	trailing := repeatRat(t, "1.0", 10)
	accept, lkg := GuardServedVWAP(rat(t, "1.01"), trailing)
	if !accept || lkg != -1 {
		t.Fatalf("normal bucket: accept=%v lkg=%d, want accept=true lkg=-1", accept, lkg)
	}
}

func TestGuardServedVWAP_FatFingerRejected(t *testing.T) {
	trailing := repeatRat(t, "1.0", 10)
	cases := []struct {
		name string
		cand string
	}{
		{"100x_extra_two_zeros", "100.0"},
		{"10x_extra_zero", "10.0001"},
		{"hundredth", "0.01"},
		{"thousandth", "0.001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accept, lkg := GuardServedVWAP(rat(t, tc.cand), trailing)
			if accept {
				t.Fatalf("candidate %s should be rejected (accept=true)", tc.cand)
			}
			if lkg < 0 || lkg >= len(trailing) {
				t.Fatalf("rejected candidate must yield a valid lkg index, got %d", lkg)
			}
			// The served last-known-good MUST itself be a real recent value.
			if trailing[lkg].Cmp(rat(t, "1.0")) != 0 {
				t.Fatalf("lkg value = %s, want a clean trailing value 1.0", trailing[lkg].FloatString(4))
			}
		})
	}
}

func TestGuardServedVWAP_DepegNotHidden(t *testing.T) {
	// A stablecoin depeg (USDT 1.0 → 0.968) is NEWS, not manipulation.
	// The guard must serve it (within an order of magnitude of the peg).
	trailing := repeatRat(t, "1.0", 12)
	accept, lkg := GuardServedVWAP(rat(t, "0.968"), trailing)
	if !accept || lkg != -1 {
		t.Fatalf("depeg 0.968: accept=%v lkg=%d, want accept=true (never hide a real depeg)", accept, lkg)
	}
	// Even a severe depeg to 0.5 must pass — still a real, order-of-
	// magnitude-plausible price, not a fat finger.
	if accept, _ := GuardServedVWAP(rat(t, "0.5"), trailing); !accept {
		t.Fatal("severe depeg 0.5 must still be served, not filtered")
	}
}

func TestGuardServedVWAP_VolatilePairRealMovePasses(t *testing.T) {
	// A genuinely volatile pair: recent buckets span a wide range. A real
	// continuation of that volatility must pass (the MAD band earns it the
	// latitude; the ratio band alone already covers <=10x).
	trailing := rats(t, "8.0", "7.0", "6.5", "6.0", "5.5", "5.0", "4.5", "4.0", "3.5", "3.0")
	for _, cand := range []string{"9.0", "2.5", "10.0", "2.0"} {
		if accept, lkg := GuardServedVWAP(rat(t, cand), trailing); !accept {
			t.Fatalf("volatile-pair real move %s wrongly rejected (lkg=%d)", cand, lkg)
		}
	}
}

func TestGuardServedVWAP_ModerateMovePasses(t *testing.T) {
	// A 3x move on an otherwise steady pair is within the wide ratio band
	// (<=10x): serve it. We favour serving a real price over over-filtering.
	trailing := repeatRat(t, "2.0", 8)
	if accept, _ := GuardServedVWAP(rat(t, "6.0"), trailing); !accept {
		t.Fatal("3x move (2.0 -> 6.0) should pass the conservative guard")
	}
	if accept, _ := GuardServedVWAP(rat(t, "0.7"), trailing); !accept {
		t.Fatal("~3x drop (2.0 -> 0.7) should pass the conservative guard")
	}
}

func TestGuardServedVWAP_InsufficientBaselineFailsOpen(t *testing.T) {
	// Fewer than guardMinSamples usable trailing buckets → no robust
	// baseline → fail open (serve candidate) even for an extreme value.
	trailing := repeatRat(t, "1.0", guardMinSamples-1)
	accept, lkg := GuardServedVWAP(rat(t, "1000.0"), trailing)
	if !accept || lkg != -1 {
		t.Fatalf("thin baseline: accept=%v lkg=%d, want fail-open accept=true lkg=-1", accept, lkg)
	}
}

func TestGuardServedVWAP_NilCandidateFailsOpen(t *testing.T) {
	trailing := repeatRat(t, "1.0", 10)
	if accept, lkg := GuardServedVWAP(nil, trailing); !accept || lkg != -1 {
		t.Fatalf("nil candidate: accept=%v lkg=%d, want fail-open", accept, lkg)
	}
}

func TestGuardServedVWAP_LKGPicksNewestCleanBucket(t *testing.T) {
	// Newest-first trailing where the two newest are themselves polluted
	// (out of band) plus one nil; the guard must skip them and serve the
	// newest CLEAN bucket.
	trailing := []*big.Rat{
		rat(t, "500.0"), // 0: polluted (also would-be manipulation)
		nil,             // 1: unparseable
		rat(t, "1.0"),   // 2: newest clean  <- expect this
		rat(t, "1.0"),   // 3
		rat(t, "1.0"),   // 4
		rat(t, "1.0"),   // 5
		rat(t, "1.0"),   // 6
		rat(t, "1.0"),   // 7
	}
	accept, lkg := GuardServedVWAP(rat(t, "999.0"), trailing)
	if accept {
		t.Fatal("manipulated candidate 999.0 must be rejected")
	}
	if lkg != 2 {
		t.Fatalf("lkg index = %d, want 2 (newest clean bucket, skipping polluted+nil)", lkg)
	}
}

func TestGuardServedVWAP_RejectionAlwaysHasCleanLKG(t *testing.T) {
	// Property: whenever the guard rejects, the returned lkg is in range
	// and its value is within the robust band (median guarantees a member).
	trailing := repeatRat(t, "1.0", 9)
	// Inject a couple of trailing outliers to prove median robustness.
	trailing[0] = rat(t, "50.0")
	trailing[1] = rat(t, "0.02")
	accept, lkg := GuardServedVWAP(rat(t, "0.0009"), trailing)
	if accept {
		t.Fatal("extreme candidate must be rejected despite noisy trailing")
	}
	lo, hi, ok := robustBand(trailing)
	if !ok {
		t.Fatal("robustBand should be defined for this baseline")
	}
	if !withinBand(trailing[lkg], lo, hi) {
		t.Fatalf("served lkg %s is not within the robust band [%s, %s]",
			trailing[lkg].FloatString(4), lo.FloatString(4), hi.FloatString(4))
	}
}

func TestMedianRat_ExactAndNonMutating(t *testing.T) {
	in := rats(t, "3", "1", "2") // odd
	got := medianRat(in)
	if got.Cmp(rat(t, "2")) != 0 {
		t.Fatalf("median(3,1,2) = %s, want 2", got.RatString())
	}
	// Input order preserved (no mutation).
	if in[0].Cmp(rat(t, "3")) != 0 {
		t.Fatal("medianRat mutated its input slice ordering")
	}
	// Even count → exact average of the two middle values.
	even := rats(t, "1", "2", "3", "4")
	if g := medianRat(even); g.Cmp(rat(t, "5/2")) != 0 {
		t.Fatalf("median(1,2,3,4) = %s, want 5/2", g.RatString())
	}
}

func TestRobustBand_ContainsCentreAndRatioBounds(t *testing.T) {
	// Flat history: MAD = 0, so the band collapses to the ratio band
	// [centre/10, centre*10].
	trailing := repeatRat(t, "1.0", 10)
	lo, hi, ok := robustBand(trailing)
	if !ok {
		t.Fatal("robustBand not ok on a valid baseline")
	}
	if lo.Cmp(rat(t, "1/10")) != 0 {
		t.Fatalf("lo = %s, want 1/10 (centre/R with MAD=0)", lo.RatString())
	}
	if hi.Cmp(rat(t, "10")) != 0 {
		t.Fatalf("hi = %s, want 10 (centre*R with MAD=0)", hi.RatString())
	}
	// Centre is always inside the band.
	if !withinBand(rat(t, "1.0"), lo, hi) {
		t.Fatal("centre must be within its own band")
	}
}

func TestGuardServedVWAP_BigIntScaleNoFloatArtifacts(t *testing.T) {
	// Smallest-unit-scale integer VWAPs (as prices_1m NUMERIC text would
	// serialise for a high-decimal token) must be judged exactly — no
	// float64 rounding in the value path.
	trailing := repeatRat(t, "1234567890123456789", 8)
	// A hair above the median: sane.
	if accept, _ := GuardServedVWAP(rat(t, "1234567890123456800"), trailing); !accept {
		t.Fatal("tiny high-precision delta should pass")
	}
	// 100x: manipulated.
	if accept, lkg := GuardServedVWAP(rat(t, "123456789012345678900"), trailing); accept || lkg < 0 {
		t.Fatalf("100x high-precision candidate should be rejected with a valid lkg (accept=%v lkg=%d)", accept, lkg)
	}
}
