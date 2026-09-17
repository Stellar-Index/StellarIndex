//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// timescaleBucketOrigin is time_bucket's default origin: Monday
// 2000-01-03 00:00 UTC. Every fixed-width bucket Timescale produces
// is origin + k×width, which is why 1w and 2w buckets start on
// Mondays and 3d buckets sit on a 3-day grid anchored there rather
// than at any particular month.
var timescaleBucketOrigin = time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC)

// timeBucket mirrors Postgres time_bucket(width, ts) for the fixed
// widths the OHLC routes fold by.
func timeBucket(width time.Duration, ts time.Time) time.Time {
	return timescaleBucketOrigin.Add(ts.Sub(timescaleBucketOrigin).Truncate(width))
}

// foldWidth parses the `N unit` INTERVAL literal a folded route
// carries, so the expectation is built from the same text the query
// interpolates.
func foldWidth(t *testing.T, lit string) time.Duration {
	t.Helper()
	f := strings.Fields(lit)
	if len(f) != 2 {
		t.Fatalf("fold literal %q is not `N unit`", lit)
	}
	n, err := strconv.Atoi(f[0])
	if err != nil {
		t.Fatalf("fold literal %q: %v", lit, err)
	}
	units := map[string]time.Duration{
		"minutes": time.Minute, "hours": time.Hour,
		"days": 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
	}
	u, ok := units[f[1]]
	if !ok {
		t.Fatalf("fold literal %q: unknown unit", lit)
	}
	return time.Duration(n) * u
}

// TestOHLCFoldIntervals_ExecuteAgainstTheirSourceView runs every folded
// /v1/ohlc route ([timescale.OHLCRoutes]) through OHLCSeriesReBucketed
// against a real TimescaleDB and checks the fold's arithmetic and
// alignment against trades placed by hand. 2h, 12h, 3d and 2w never
// executed in production before this test existed: their literals
// were routed but not allow-listed, so the store refused them before
// composing SQL and the API answered 500 (launch plan W8-17). A fold
// literal that Postgres would not accept, or a bucket that does not
// land where the API promises (`t` aligned to UTC interval
// boundaries, weeks on Monday), fails here rather than at the edge.
func TestOHLCFoldIntervals_ExecuteAgainstTheirSourceView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	// The window is one 2-week bucket, anchored on a 2w boundary at
	// least six weeks back so the widest fold's bucket is closed under
	// the ADR-0015 guard (bucket + width <= now()). Every narrower
	// fold's grid nests inside it.
	const window = 14 * 24 * time.Hour
	t0 := timeBucket(window, time.Now().UTC().Add(-6*7*24*time.Hour))
	if t0.Weekday() != time.Monday {
		t.Fatalf("2w anchor %v is a %v, want Monday — origin arithmetic is wrong", t0, t0.Weekday())
	}

	// Four trades, all in the requested orientation, at prices that
	// make open/high/low/close distinguishable per fold. Base is a
	// constant 1e6 so volumes are exact multiples.
	type seed struct {
		at    time.Duration
		quote int64 // price = quote / 1e6
	}
	seeds := []seed{
		{1 * time.Hour, 1_000_000},                // 1.0
		{1*time.Hour + 30*time.Minute, 1_500_000}, // 1.5 — same 2h bucket as the first
		{3*24*time.Hour + 5*time.Hour, 2_000_000}, // 2.0 — day 4, first 12h half
		{10*24*time.Hour + 13*time.Hour, 500_000}, // 0.5 — week 2, second 12h half
	}
	for i, s := range seeds {
		if err := store.InsertTrade(ctx, mkAPITrade(i+1, t0.Add(s.at), pair, 1_000_000, s.quote)); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	// Materialise every view a folded route reads from. Each prices_<g>
	// aggregates the trades hypertable directly (migration 0147), so
	// order does not matter.
	sources := map[timescale.HistoryGranularity]bool{}
	for _, r := range timescale.OHLCRoutes {
		if r.Folded() {
			sources[r.Source] = true
		}
	}
	for g := range sources {
		if _, err := store.DB().ExecContext(ctx,
			"CALL refresh_continuous_aggregate('prices_"+string(g)+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh prices_%s: %v", g, err)
		}
	}

	executed := 0
	for _, route := range timescale.OHLCRoutes {
		if !route.Folded() {
			continue
		}
		executed++
		t.Run(route.Interval, func(t *testing.T) {
			width := foldWidth(t, route.Fold)
			bars, err := store.OHLCSeriesReBucketed(ctx, pair, route.Source, route.Fold, t0, t0.Add(window), 0)
			if err != nil {
				t.Fatalf("OHLCSeriesReBucketed(%s, %q): %v", route.Source, route.Fold, err)
			}

			// Expectation: group the seeds by the fold's bucket, in
			// time order, and roll each group up the way the query
			// documents (open = first, close = last, high/low = extremes,
			// volumes = sums, n = count).
			type expect struct {
				open, high, low, close float64
				quote, n               int64
			}
			var order []time.Time
			want := map[time.Time]*expect{}
			for _, s := range seeds {
				b := timeBucket(width, t0.Add(s.at))
				px := float64(s.quote) / 1e6
				e, ok := want[b]
				if !ok {
					e = &expect{open: px, high: px, low: px}
					want[b] = e
					order = append(order, b)
				}
				e.close = px
				e.high = max(e.high, px)
				e.low = min(e.low, px)
				e.quote += s.quote
				e.n++
			}
			if len(bars) != len(order) {
				t.Fatalf("got %d bars, want %d (buckets %v): %+v", len(bars), len(order), order, bars)
			}
			for i, bar := range bars {
				b := order[i]
				e := want[b]
				if !bar.Bucket.Equal(b) {
					t.Errorf("bar[%d].Bucket = %v, want %v (time_bucket('%s') of the seeds)", i, bar.Bucket, b, route.Fold)
				}
				if !bar.Bucket.Equal(timeBucket(width, bar.Bucket)) {
					t.Errorf("bar[%d].Bucket = %v is not aligned to a %v grid from the Timescale origin", i, bar.Bucket, width)
				}
				if route.Source == timescale.Granularity1w && bar.Bucket.Weekday() != time.Monday {
					t.Errorf("bar[%d].Bucket = %v is a %v; a fold of prices_1w must start on its Monday", i, bar.Bucket, bar.Bucket.Weekday())
				}
				if bar.TradeCount != e.n {
					t.Errorf("bar[%d].TradeCount = %d, want %d", i, bar.TradeCount, e.n)
				}
				for name, got := range map[string]struct{ have, want float64 }{
					"Open":        {mustFloat(t, bar.Open), e.open},
					"High":        {mustFloat(t, bar.High), e.high},
					"Low":         {mustFloat(t, bar.Low), e.low},
					"Close":       {mustFloat(t, bar.Close), e.close},
					"BaseVolume":  {mustFloat(t, bar.BaseVolume), float64(e.n) * 1e6},
					"QuoteVolume": {mustFloat(t, bar.QuoteVolume), float64(e.quote)},
				} {
					if diff := got.have - got.want; diff > 1e-6 || diff < -1e-6 {
						t.Errorf("bar[%d].%s = %v, want %v", i, name, got.have, got.want)
					}
				}
			}
		})
	}
	if executed < 7 {
		t.Fatalf("executed %d folded routes, want at least the 7 the API serves", executed)
	}
	t.Logf("executed %d of %d folded routes against TimescaleDB", executed, executed)
}
