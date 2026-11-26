package orchestrator

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

// hourlyOverlap prints, every hour of a 24 h window at one price, proxyXLM on the proxy venue, ownXLM split across the two own
// venues and extraXLM (0 = none) on extraVenue, and returns how many slots are eligible.
func hourlyOverlap(t *testing.T, price *big.Rat, proxyXLM, ownXLM, extraXLM int64) int {
	t.Helper()
	whole := func(n int64) *big.Int {
		if n == 0 {
			return nil
		}
		return new(big.Int).Mul(big.NewInt(n), big.NewInt(100_000_000))
	}
	trades, proxied := buildSurface(t, 24*time.Hour, time.Hour, func(time.Duration) surfaceTick {
		return surfaceTick{
			proxyPrice: price, ownPrice: price, extraPrice: price,
			proxyBase: whole(proxyXLM), ownBase: whole(ownXLM), extraBase: whole(extraXLM),
		}
	})
	return readSurface(t, 24*time.Hour, trades, proxied).surfaceOverlap
}

// Each per-slot floor binds on its own side with the other side far above it, and a slot exactly at a floor clears it.
func TestQuoteSurface_SlotFloorsBindPerSideAtTheirExactValue(t *testing.T) {
	eighth, dollar := big.NewRat(1, 8), big.NewRat(1, 1)
	cases := []struct {
		name                string
		price               *big.Rat
		proxy, own, overlap int64
	}{
		{"stablecoin quote 24,999/h", eighth, 199_992, 1_000_000, 0},
		{"stablecoin quote 25,000/h", eighth, 200_000, 1_000_000, 24},
		{"USD quote 24,999/h", eighth, 500_000, 199_992, 0},
		{"USD quote 25,000/h", eighth, 500_000, 200_000, 24},
		{"stablecoin base 149,999 XLM/h", dollar, 149_999, 1_000_000, 0},
		{"stablecoin base 150,000 XLM/h", dollar, 150_000, 1_000_000, 24},
		{"USD base 149,999 XLM/h", dollar, 300_000, 149_999, 0},
		{"USD base 150,000 XLM/h", dollar, 300_000, 150_000, 24},
	}
	for _, tc := range cases {
		if got := hourlyOverlap(t, tc.price, tc.proxy, tc.own, 0); int64(got) != tc.overlap {
			t.Errorf("%s: %d eligible slots, want %d", tc.name, got, tc.overlap)
		}
	}
}

// The 1/3 share is of the reference, not of all own-quote volume: a third venue under the venue quote floor (80k XLM = 9.6k USD an
// hour) lifting own base to 38 % of proxy does not make a 30 % reference eligible; a 33.4 % reference is.
func TestQuoteSurface_OwnShareCountsOnlyTheReference(t *testing.T) {
	price := big.NewRat(12, 100)
	if got := hourlyOverlap(t, price, 1_000_000, 300_000, 80_000); got != 0 {
		t.Errorf("30 %% reference plus a filtered venue: %d eligible slots, want 0", got)
	}
	if got := hourlyOverlap(t, price, 1_000_000, 334_000, 80_000); got != 24 {
		t.Errorf("33.4 %% reference: %d eligible slots, want 24", got)
	}
}

// A venue with exactly 1/10 of own base is part of the reference, so pricing it 5 % away spoils agreement; just under 1/10 it is
// ignored.
func TestQuoteSurface_VenueAtExactlyOneTenthCounts(t *testing.T) {
	read := func(extraXLM int64) int {
		trades, proxied := buildSurface(t, 24*time.Hour, time.Hour, func(time.Duration) surfaceTick {
			return surfaceTick{
				proxyPrice: flat(0), ownPrice: flat(0), extraPrice: basis(flat(0), 500),
				proxyBase: xlm(1000), ownBase: xlm(900), extraBase: new(big.Int).Mul(big.NewInt(extraXLM), big.NewInt(100_000_000)),
			}
		})
		return readSurface(t, 24*time.Hour, trades, proxied).surfaceOverlap
	}
	if got := read(100_000); got != 0 {
		t.Errorf("disagreeing venue at exactly 1/10: %d eligible slots, want 0", got)
	}
	if got := read(99_999); got != 24 {
		t.Errorf("disagreeing venue just under 1/10: %d eligible slots, want 24", got)
	}
}

// Volume is compared at the window's common scale: a 7-decimal venue's 300k XLM counts as much as an 8-decimal venue's, so the two
// form a two-venue reference.
func TestQuoteSurface_MixedDecimalVenuesCountAtTheCommonScale(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	start, price := surfaceEnd.Add(-24*time.Hour), big.NewRat(12, 100)
	at := func(n int64, decimals int64) *big.Int {
		return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil))
	}
	var trades []canonical.Trade
	proxied := map[string]struct{}{}
	for h := range 24 {
		ts := start.Add(time.Duration(h) * time.Hour)
		p := surfacePrint(xlmUSD, proxyVenue, ts, at(1_000_000, 8), price)
		proxied[p.ID()] = struct{}{}
		trades = append(trades, p,
			surfacePrint(xlmUSD, ownVenues[0], ts.Add(time.Minute), at(300_000, 8), price),
			surfacePrint(xlmUSD, "sdex", ts.Add(2*time.Minute), at(300_000, 7), price))
	}
	if amountScaleDecimalsFor("sdex") != 7 {
		t.Fatalf("sdex amount scale %d, want 7", amountScaleDecimalsFor("sdex"))
	}
	if lr := readSurface(t, 24*time.Hour, trades, proxied); lr.surfaceOverlap != 24 {
		t.Errorf("%s, want 24 eligible slots", lr.surfaceString())
	}
}
