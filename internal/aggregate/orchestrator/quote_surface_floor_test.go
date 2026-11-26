package orchestrator

import (
	"math/big"
	"testing"
	"time"
)

// alignedEqual reports whether two readings are the same aligned reading: decided alone, same slots, coverage and gap.
func alignedEqual(a, b legRef) bool {
	return a.surfaceAligned && b.surfaceAligned && a.surfaceOverlap == b.surfaceOverlap &&
		a.surfaceCoverage.Cmp(b.surfaceCoverage) == 0 && a.surfaceGap.Cmp(b.surfaceGap) == 0
}

// A third venue under the venue floor (under 1/10 of the slot's own base, or under 10k USD an hour) is not part of the reference:
// pricing it 5 % away neither spoils the slots' eligibility nor moves their gap.
func TestQuoteSurface_SubFloorVenueChangesNothing(t *testing.T) {
	window := 24 * time.Hour
	cases := map[string]struct {
		honest func(off time.Duration) surfaceTick
		extra  func(off time.Duration) *big.Int
	}{
		"under 1/10 of own base (trend, shifted volume)": {
			honest: func(off time.Duration) surfaceTick {
				pb, ob := shiftedVolume(off)
				return surfaceTick{proxyPrice: ramp(off), ownPrice: ramp(off), proxyBase: pb, ownBase: ob}
			},
			extra: func(off time.Duration) *big.Int { // 1/13 of own base, >= 15k USD an hour
				_, ob := shiftedVolume(off)
				return new(big.Int).Quo(ob, big.NewInt(12))
			},
		},
		"under 10k USD an hour (flat)": {
			honest: func(off time.Duration) surfaceTick {
				return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
			},
			extra: func(time.Duration) *big.Int { return xlm(12) }, // 12/112 of own base, 8.6k USD an hour
		},
	}
	for name, tc := range cases {
		clean := readSurfaceFor(t, window, tc.honest, nil)
		dusted := readSurfaceFor(t, window, tc.honest, tc.extra)
		if !clean.surfaceAligned || clean.surfaceOverlap != 24 {
			t.Fatalf("%s: clean window aligned=%v overlap=%d, want decided alone over 24 slots", name, clean.surfaceAligned,
				clean.surfaceOverlap)
		}
		if !alignedEqual(clean, dusted) {
			t.Errorf("%s: sub-floor venue moved the reading: %s, want %s", name, dusted.surfaceString(), clean.surfaceString())
		}
	}
}

// readSurfaceFor reads buildSurface at 10-minute steps with an optional extraVenue print 5 % above the own-quote price.
func readSurfaceFor(t *testing.T, window time.Duration, honest func(time.Duration) surfaceTick, extra func(time.Duration) *big.Int) legRef {
	t.Helper()
	trades, proxied := buildSurface(t, window, 10*time.Minute, func(off time.Duration) surfaceTick {
		tk := honest(off)
		if extra != nil {
			tk.extraPrice, tk.extraBase = basis(tk.ownPrice, 500), extra(off)
		}
		return tk
	})
	return readSurface(t, window, trades, proxied)
}

// R̂ spans the own-quote VWAPs too: a USD-only hour 10 % above the rest widens the band past tolerance at 96 % coverage, so the
// reading may not decide alone, while the proxy VWAPs alone (a 60 bps range) would let it.
func TestQuoteSurface_OwnQuotePricesWidenTheEnvelope(t *testing.T) {
	window := 24 * time.Hour
	trades, proxied := buildSurface(t, window, 10*time.Minute, func(off time.Duration) surfaceTick {
		tk := surfaceTick{proxyPrice: basis(flat(off), -60), ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
		switch {
		case off < time.Hour: // ineligible: one own venue, 1/24 of proxy volume
			tk.soleOwn = true
		case off >= 5*time.Hour && off < 6*time.Hour: // USD-only hour 10 % up
			tk.proxyBase, tk.ownPrice = nil, basis(flat(off), 1000)
		}
		return tk
	})
	lr := readSurface(t, window, trades, proxied)
	if lr.surfaceAligned || lr.surfaceOverlap != 22 {
		t.Errorf("%s, want the whole-window fallback over 22 eligible slots: R̂ must include own-quote VWAPs", lr.surfaceString())
	}
}

// A slot VWAP that cannot be computed (a zero-volume proxy print alone in its slot) fails the leg closed, never "ok".
func TestQuoteSurface_UncomputableSlotFailsClosed(t *testing.T) {
	window := 24 * time.Hour
	trades, proxied := buildSurface(t, window, 10*time.Minute, func(off time.Duration) surfaceTick {
		if off < time.Hour {
			return surfaceTick{ownPrice: flat(off), ownBase: xlm(100)}
		}
		return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
	})
	empty := surfacePrint(mkPair(t, "crypto", "XLM", "fiat", "USD"), proxyVenue, surfaceEnd.Add(-window), new(big.Int), flat(0))
	trades = append(trades, empty)
	proxied[empty.ID()] = struct{}{}
	lr := readSurface(t, window, trades, proxied)
	if !lr.surfaceUncomputable || lr.quoteSurfaceRefusal(surfaceTol) == "" {
		t.Errorf("%q (uncomputable=%v), want the leg refused as uncomputable", lr.surfaceString(), lr.surfaceUncomputable)
	}
}

// Every slot clears its floors at ~30k USD an hour a side, but the window carries under 50k an hour a side: it may not decide alone.
// Doubled, it does.
func TestQuoteSurface_WindowFloorGatesDecidingAlone(t *testing.T) {
	window := 24 * time.Hour
	read := func(n int64) legRef {
		return readSurfaceFor(t, window, func(off time.Duration) surfaceTick {
			return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(n), ownBase: xlm(n)}
		}, nil)
	}
	if lr := read(42); lr.surfaceAligned || lr.surfaceOverlap != 24 {
		t.Errorf("30k USD an hour: %s, want 24 eligible slots judged on the whole-window fallback", lr.surfaceString())
	}
	if lr := read(84); !lr.surfaceAligned {
		t.Errorf("60k USD an hour: %s, want decided alone", lr.surfaceString())
	}
}

// At 1 USD per XLM, 100k XLM an hour a side clears every quote floor but not the 150k XLM base floor; 200k does.
func TestQuoteSurface_BaseFloorIsInXLM(t *testing.T) {
	window := 24 * time.Hour
	dollar := func(time.Duration) *big.Rat { return big.NewRat(1, 1) }
	read := func(perTick int64) legRef {
		return readSurfaceFor(t, window, func(off time.Duration) surfaceTick {
			base := new(big.Int).Mul(big.NewInt(perTick), big.NewInt(100_000_000))
			return surfaceTick{proxyPrice: dollar(off), ownPrice: dollar(off), proxyBase: base, ownBase: base}
		}, nil)
	}
	if lr := read(100_000 / 6); lr.surfaceOverlap != 0 {
		t.Errorf("100k XLM an hour: %s, want no eligible slot", lr.surfaceString())
	}
	if lr := read(200_000 / 6); lr.surfaceOverlap != 24 {
		t.Errorf("200k XLM an hour: %s, want 24 eligible slots", lr.surfaceString())
	}
}

// The base floor is in XLM, so a leg with another base has no eligible slot and is the whole-window compare exactly.
func TestQuoteSurface_NonXLMBaseIsTheWholeWindowCompare(t *testing.T) {
	window := 24 * time.Hour
	trades, proxied := buildSurface(t, window, 10*time.Minute, func(off time.Duration) surfaceTick {
		pb, ob := shiftedVolume(off)
		return surfaceTick{proxyPrice: ramp(off), ownPrice: ramp(off), proxyBase: pb, ownBase: ob}
	})
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{Windows: []time.Duration{window}})
	lr := o.newLegRef(mkPair(t, "crypto", "BTC", "fiat", "USD"), window, big.NewRat(12, 100), trades, proxied)
	if want := surfaceOracleOf(trades, proxied).whole; lr.surfaceOverlap != 0 || lr.surfaceAligned || lr.surfaceGap.Cmp(want) != 0 {
		t.Errorf("BTC base: %s, want the whole-window gap %.1f bps with no eligible slot", lr.surfaceString(), ratBps(want))
	}
}
