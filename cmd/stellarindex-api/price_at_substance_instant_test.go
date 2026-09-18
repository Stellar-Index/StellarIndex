package main

// Finding T038, proven through the PRODUCTION seam: storePriceAtReader
// is the one reader behind /v1/price/at and every /v1/price/changes
// horizon, and it asked the thin-market gate about the trailing 24h
// ending NOW while serving the bucket at-or-before a past `ts`.
//
// storePriceAtReader.s is a concrete *timescale.Store with an unexported
// db field (see price_at_guard_wiring_test.go), so this package cannot
// script the price read itself. It does not need to: the withholding
// decision is made BEFORE the store is touched, so with a nil store the
// two outcomes are distinguishable without one — a withheld read returns
// v1.ErrPriceWithheld, and an allowed read goes on to the store (and,
// here, panics on it, which the helper reports as "reached the store").

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	outcomeWithheld     = "withheld"
	outcomeReachedStore = "allowed (went on to read the store)"
)

// historyAwareSubstance is a market whose substance changed over time.
// trailing is what a window ending now sees; atInstant is what a window
// ending at the requested instant sees.
type historyAwareSubstance struct {
	trailing  timescale.MarketSubstance
	atInstant timescale.MarketSubstance
	askedAt   []time.Time
}

func (h *historyAwareSubstance) PairMarketSubstance(context.Context, canonical.Pair, time.Duration) (timescale.MarketSubstance, error) {
	return h.trailing, nil
}

func (h *historyAwareSubstance) PairMarketSubstanceAt(
	_ context.Context, _ canonical.Pair, asOf time.Time, _ time.Duration, _ timescale.HistoryGranularity,
) (timescale.MarketSubstance, error) {
	h.askedAt = append(h.askedAt, asOf)
	return h.atInstant, nil
}

func priceAtOutcome(t *testing.T, r storePriceAtReader, pair canonical.Pair, ts time.Time) (outcome string) {
	t.Helper()
	defer func() {
		if recover() != nil {
			outcome = outcomeReachedStore
		}
	}()
	_, _, _, err := r.PriceAt(context.Background(), pair, ts, 24*time.Hour)
	if errors.Is(err, v1.ErrPriceWithheld) {
		return outcomeWithheld
	}
	return fmt.Sprintf("unexpected result from a nil store: err=%v", err)
}

func thinMarketTestPair(t *testing.T) canonical.Pair {
	t.Helper()
	token, err := canonical.NewClassicAsset("SEED", "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	pair, err := canonical.NewPair(token, canonical.NativeAsset())
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return pair
}

func TestPriceAtSeamJudgesTheMarketAtTheRequestedInstant(t *testing.T) {
	thick := timescale.MarketSubstance{VolumeUSD: "250000.5", Buckets: 900, SpanSeconds: 23 * 3600}
	dust := timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 1, SpanSeconds: 0}
	empty := timescale.MarketSubstance{VolumeUSD: "0"}
	pair := thinMarketTestPair(t)

	cases := []struct {
		name string
		mkt  *historyAwareSubstance
		ts   time.Time
		want string
		why  string
	}{
		{
			name: "thick today, attacker-seeded dust at ts",
			mkt:  &historyAwareSubstance{trailing: thick, atInstant: dust},
			ts:   time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC),
			want: outcomeWithheld,
			why: "the read serves the 2021 bucket, and the 2021 market was one $8.57 burst — " +
				"today's volume says nothing about it; this is the manipulated price the gate exists to refuse",
		},
		{
			name: "deep at ts, dormant today",
			mkt:  &historyAwareSubstance{trailing: empty, atInstant: thick},
			ts:   time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC),
			want: outcomeReachedStore,
			why: "the 2024 market cleared every leg of the floor; withholding it because the pair is " +
				"quiet today 404s a cost-basis read for data we hold and trust",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := pricingguard.NewSubstanceGate(tc.mkt, pricingguard.SubstanceGateOptions{})
			reader := storePriceAtReader{substance: gate} // nil store: see the file comment

			if got := priceAtOutcome(t, reader, pair, tc.ts); got != tc.want {
				t.Errorf("PriceAt(ts=%s) = %s, want %s — %s",
					tc.ts.Format(time.RFC3339), got, tc.want, tc.why)
			}
			if len(tc.mkt.askedAt) == 0 {
				t.Fatal("the gate never asked the store about the requested instant")
			}
			for _, asOf := range tc.mkt.askedAt {
				// Hour grain: measured as of the top of ts's hour.
				if !asOf.Equal(tc.ts.Truncate(time.Hour)) {
					t.Errorf("substance measured as of %s, want the requested instant %s", asOf, tc.ts)
				}
			}
		})
	}
}
