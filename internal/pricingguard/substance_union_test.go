// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pricingguard

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// minuteRow is one prices_1m row: a closed minute of one stored spelling.
type minuteRow struct {
	base, quote string
	bucket      time.Time
	usd         int64
}

// minuteStore models prices_1m as rows and answers the substance question
// the way the SQL does: distinct buckets, summed volume and span over
// every row the request selects, in both stored orientations. It is a
// model of the table, not a canned answer, so it can only agree with a
// gate that asks for the union in one read.
type minuteStore struct{ rows []minuteRow }

func (m *minuteStore) measure(match func(base, quote string) bool) timescale.MarketSubstance {
	vol := new(big.Rat)
	seen := map[time.Time]bool{}
	var lo, hi time.Time
	for _, r := range m.rows {
		if !match(r.base, r.quote) && !match(r.quote, r.base) {
			continue
		}
		vol.Add(vol, new(big.Rat).SetInt64(r.usd))
		seen[r.bucket] = true
		if lo.IsZero() || r.bucket.Before(lo) {
			lo = r.bucket
		}
		if r.bucket.After(hi) {
			hi = r.bucket
		}
	}
	return timescale.MarketSubstance{
		VolumeUSD:   vol.FloatString(0),
		Buckets:     int64(len(seen)),
		SpanSeconds: int64(hi.Sub(lo) / time.Second),
	}
}

func (m *minuteStore) PairMarketSubstance(
	_ context.Context, bases, quotes []canonical.Asset, _ time.Duration,
) (timescale.MarketSubstance, error) {
	in := func(set []canonical.Asset, s string) bool {
		for _, a := range set {
			if a.String() == s {
				return true
			}
		}
		return false
	}
	return m.measure(func(b, q string) bool { return in(bases, b) && in(quotes, q) }), nil
}

// PairMarketSubstanceAt satisfies [SubstanceStore]; these fixtures only
// exercise the live path ([SubstanceGate.Verdict]), so it ignores the
// point-in-time parameters and answers the same union as the live read.
func (m *minuteStore) PairMarketSubstanceAt(
	ctx context.Context, bases, quotes []canonical.Asset, _ time.Time, window time.Duration, _ timescale.HistoryGranularity,
) (timescale.MarketSubstance, error) {
	return m.PairMarketSubstance(ctx, bases, quotes, window)
}

// XLM's SDEX leg (native) and CEX leg (crypto:XLM) trading in the SAME ten
// minutes are ten minutes of market, not twenty. Counting each spelling's
// distinct minutes and adding them let a market clear the 20-minute floor
// on half the persistence it demands.
func TestSubstanceGate_AliasUnionCountsSharedMinutesOnce(t *testing.T) {
	usdc := mustAsset(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := &minuteStore{}
	for i := 0; i < 10; i++ {
		minute := start.Add(time.Duration(i) * 47 * time.Minute) // spans 7h03m
		for _, spelling := range []string{"native", "crypto:XLM"} {
			store.rows = append(store.rows, minuteRow{spelling, usdc.String(), minute, 5000})
		}
	}
	gate := NewSubstanceGate(store, SubstanceGateOptions{Policy: testPolicy()})

	allowed, measured := gate.Verdict(context.Background(), canonical.NativeAsset(), usdc, "test")
	if !measured {
		t.Fatal("verdict unmeasured")
	}
	if allowed {
		t.Fatal("10 distinct minutes quoted under two XLM spellings cleared a 20-distinct-minute floor")
	}
}

// The span leg is the union's wall-clock reach too: one spelling trading
// early in the window and another late is one market active across the
// whole stretch, not two short-lived ones.
func TestSubstanceGate_AliasUnionSpanCoversEverySpelling(t *testing.T) {
	usdc := mustAsset(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := &minuteStore{}
	for i := 0; i < 15; i++ {
		early := start.Add(time.Duration(i) * 12 * time.Minute)          // 0h00 – 2h48
		late := start.Add(4*time.Hour + time.Duration(i)*12*time.Minute) // 4h00 – 6h48
		store.rows = append(store.rows,
			minuteRow{"native", usdc.String(), early, 5000},
			minuteRow{"crypto:XLM", usdc.String(), late, 5000})
	}
	gate := NewSubstanceGate(store, SubstanceGateOptions{Policy: testPolicy()})

	allowed, measured := gate.Verdict(context.Background(), canonical.NativeAsset(), usdc, "test")
	if !measured || !allowed {
		t.Fatalf("30 distinct minutes spanning 6h48m across two spellings: allowed=%v measured=%v, want served",
			allowed, measured)
	}
}
