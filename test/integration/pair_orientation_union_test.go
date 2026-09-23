//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// orientationFixture seeds two markets into prices_1m against real
// TimescaleDB: TWO/native trading both ways in one closed minute, and
// FLIP/native stored ONLY as (native, FLIP). Every trade prices the
// non-native leg at 0.5 native and carries $100 of USD volume.
type orientationFixture struct {
	store              *timescale.Store
	twoXLM, flipXLM    c.Pair
	twoMinute, flipMin time.Time
}

func seedOrientationFixture(ctx context.Context, t *testing.T) orientationFixture {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	two, err := c.NewClassicAsset("TWO", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	flip, err := c.NewClassicAsset("FLIP", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	f := orientationFixture{store: store}
	f.twoXLM, _ = c.NewPair(two, c.NativeAsset())
	xlmTwo, _ := c.NewPair(c.NativeAsset(), two)
	f.flipXLM, _ = c.NewPair(flip, c.NativeAsset())
	xlmFlip, _ := c.NewPair(c.NativeAsset(), flip)

	now := time.Now().UTC().Truncate(time.Minute)
	f.twoMinute = now.Add(-30 * time.Minute)
	f.flipMin = now.Add(-20 * time.Minute)
	for _, tr := range []c.Trade{
		mkAPITrade(1, f.twoMinute.Add(5*time.Second), f.twoXLM, 1_000_000, 500_000),
		mkAPITrade(2, f.twoMinute.Add(10*time.Second), xlmTwo, 500_000, 1_000_000),
		mkAPITrade(3, f.flipMin.Add(5*time.Second), xlmFlip, 500_000, 1_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE source = 'integ-api'`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	return f
}

// TestPairReadersFoldBothOrientationsAsUnion executes the six pair
// readers whose orientation fold moved from one OR disjunction to two
// UNION ALL arms (GH-866), and pins that the rewrite still reads both
// stored directions: a flipped-only market is served, a two-sided one
// is counted once per row, and an absent pair is still a clean miss.
func TestPairReadersFoldBothOrientationsAsUnion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := seedOrientationFixture(ctx, t)
	near := func(name, got string, want float64) {
		t.Helper()
		if v := mustFloat(t, got); v < want-1e-9 || v > want+1e-9 {
			t.Errorf("%s = %s, want %v", name, got, want)
		}
	}

	latest, err := f.store.LatestClosedVWAP1mForPair(ctx, f.flipXLM)
	if err != nil {
		t.Fatalf("LatestClosedVWAP1mForPair(flipped-only): %v", err)
	}
	near("latest VWAP, flipped-only market", latest.VWAP, 0.5)

	at, err := f.store.ClosedVWAPAtOrBefore(ctx, f.flipXLM, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("ClosedVWAPAtOrBefore(flipped-only): %v", err)
	}
	near("VWAP at-or-before, flipped-only market", at.VWAP, 0.5)
	if !at.Bucket.Equal(f.flipMin) {
		t.Errorf("at-or-before bucket = %s, want %s", at.Bucket, f.flipMin)
	}

	pts, err := f.store.TimedVWAPs1mForChangeSummary(ctx, f.twoXLM, f.twoMinute.Add(-time.Hour), f.twoMinute.Add(time.Minute))
	if err != nil {
		t.Fatalf("TimedVWAPs1mForChangeSummary: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("change-summary points = %d, want 1 (both directions of one bucket fold to one point)", len(pts))
	}
	near("change-summary VWAP, two-sided bucket", pts[0].Value, 0.5)

	sub, err := f.store.PairMarketSubstance(ctx, f.twoXLM, 24*time.Hour)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	if mustFloat(t, sub.VolumeUSD) != 200 || sub.Buckets != 1 {
		t.Errorf("substance = {%s, %d buckets}, want {200, 1}: both directions, one bucket", sub.VolumeUSD, sub.Buckets)
	}
	subAt, err := f.store.PairMarketSubstanceAt(ctx, f.flipXLM, time.Now().UTC(), 24*time.Hour, timescale.Granularity1m)
	if err != nil {
		t.Fatalf("PairMarketSubstanceAt: %v", err)
	}
	if mustFloat(t, subAt.VolumeUSD) != 100 || subAt.Buckets != 1 {
		t.Errorf("substance-at = {%s, %d buckets}, want {100, 1} from the flipped row", subAt.VolumeUSD, subAt.Buckets)
	}

	absent, _ := c.NewPair(f.twoXLM.Base, f.flipXLM.Base)
	if _, err := f.store.LatestClosedVWAP1mForPair(ctx, absent); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("absent pair: err = %v, want sql.ErrNoRows", err)
	}
}

// TestMarketsSparklineMatchesListingVolume executes the /v1/markets
// sparkline read against the listing it decorates (GH-962). The handler
// keys the batch by the listing's CANONICAL rows; FLIP/native is stored
// only as (native, FLIP), so a read of the stored key alone drew 24 zero
// bars beside a $100 volume_24h_usd. Every seeded trade sits inside the
// last hour, so the 24 hourly bars must sum to the headline exactly.
func TestMarketsSparklineMatchesListingVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := seedOrientationFixture(ctx, t)

	rows, _, err := f.store.DistinctPairs(ctx, "", 50)
	if err != nil {
		t.Fatalf("DistinctPairs: %v", err)
	}
	rat := func(s string) *big.Rat {
		t.Helper()
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("not a decimal: %q", s)
		}
		return r
	}
	want := map[string]*big.Rat{}
	var pairs [][2]string
	for _, m := range rows {
		if m.Volume24hUSD == nil {
			continue
		}
		key := m.Pair.Base.String() + "|" + m.Pair.Quote.String()
		want[key] = rat(*m.Volume24hUSD)
		pairs = append(pairs, [2]string{m.Pair.Base.String(), m.Pair.Quote.String()})
	}
	flipKey := f.flipXLM.Base.String() + "|" + f.flipXLM.Quote.String()
	twoKey := f.twoXLM.Base.String() + "|" + f.twoXLM.Quote.String()
	if want[flipKey] == nil || want[flipKey].Cmp(big.NewRat(100, 1)) != 0 ||
		want[twoKey] == nil || want[twoKey].Cmp(big.NewRat(200, 1)) != 0 {
		t.Fatalf("fixture: listing volumes = %v, want %s=100 and %s=200", want, flipKey, twoKey)
	}

	hist, err := f.store.GetPairsVolumeHistory24hBatch(ctx, pairs)
	if err != nil {
		t.Fatalf("GetPairsVolumeHistory24hBatch: %v", err)
	}
	for key, headline := range want {
		series := hist[key]
		if len(series) != 24 {
			t.Errorf("%s: %d bars, want 24", key, len(series))
			continue
		}
		sum := new(big.Rat)
		for _, p := range series {
			sum.Add(sum, rat(p.VolumeUSD))
		}
		if sum.Cmp(headline) != 0 {
			t.Errorf("%s: sparkline sums to %s, listing volume_24h_usd is %s",
				key, sum.FloatString(7), headline.FloatString(7))
		}
	}
}
