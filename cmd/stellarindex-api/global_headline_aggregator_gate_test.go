package main

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// aggregatorTierStore has no prices_1m bucket, so the headline must come
// from tier 2: it holds one fresh aggregator quote for every pair asked.
type aggregatorTierStore struct{ calls int }

func (*aggregatorTierStore) LatestClosedVWAP1mForPair(context.Context, canonical.Pair) (timescale.Vwap1mRow, error) {
	return timescale.Vwap1mRow{}, sql.ErrNoRows
}

func (*aggregatorTierStore) RecentClosedVWAP1mCombined(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return nil, nil
}

func (s *aggregatorTierStore) LatestAggregatorPricesForPair(
	_ context.Context, base, quote canonical.Asset, _ []string,
) ([]canonical.OracleUpdate, error) {
	s.calls++
	return []canonical.OracleUpdate{{
		Source: "coingecko", Asset: base, Quote: quote, Timestamp: time.Now(),
		Price: canonical.NewAmount(big.NewInt(723_000)), Decimals: 8,
	}}, nil
}

func aggregatorTierReader(t *testing.T, flagged map[string]bool) (globalPriceReader, *aggregatorTierStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := &aggregatorTierStore{}
	return globalPriceReader{
		s:         store,
		tri:       redisTriangulatedLooker{rdb: rdb},
		pkPairFor: canonical.NewPair,
		scam:      pricingguard.NewScamGate(&flaggingScamDirectory{flagged: flagged}, pricingguard.ScamGateOptions{}),
	}, store
}

func aggregatorTierOptions() aggregate.GlobalPriceOptions {
	opts := aggregate.DefaultGlobalPriceOptions()
	opts.AggregatorSources = []string{"coingecko"}
	return opts
}

// Tier 2 of the GlobalAssetView headline must withhold a directory-flagged
// issuer exactly as tiers 1 and 3 do; the store holds a fresh quote, so
// only the gate stands between it and the asset page.
func TestGlobalHeadlineAggregatorTierWithholdsFlaggedIssuer(t *testing.T) {
	base, quote := headlinePair(t)
	reader, store := aggregatorTierReader(t, map[string]bool{headlineFlaggedIssuer: true})

	rows, err := reader.LatestAggregatorPrices(context.Background(), base, quote, []string{"coingecko"})
	if err != nil {
		t.Fatalf("LatestAggregatorPrices: %v", err)
	}
	if len(rows) != 0 || store.calls != 0 {
		t.Errorf("tier 2 returned %d rows (store reads %d) for a directory-flagged issuer; want none",
			len(rows), store.calls)
	}

	res, err := aggregate.ComputeGlobalPrice(context.Background(), base, quote, reader, aggregatorTierOptions())
	if !errors.Is(err, aggregate.ErrNoPrice) {
		t.Errorf("ComputeGlobalPrice = (%q via %s, %v), want ErrNoPrice for a flagged issuer",
			res.Price, res.Authority, err)
	}
}

// Non-vacuity: the same fixture with an unflagged directory still serves
// the aggregator headline, so the test above is not passing on a dead tier.
func TestGlobalHeadlineAggregatorTierServesUnflaggedIssuer(t *testing.T) {
	base, quote := headlinePair(t)
	reader, _ := aggregatorTierReader(t, map[string]bool{})

	res, err := aggregate.ComputeGlobalPrice(context.Background(), base, quote, reader, aggregatorTierOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice: %v", err)
	}
	if res.Authority != aggregate.AuthorityAggregatorAvg || res.Price != "0.00723000000000" {
		t.Errorf("headline = (%q via %s), want (\"0.00723000000000\" via %s)",
			res.Price, res.Authority, aggregate.AuthorityAggregatorAvg)
	}
}
