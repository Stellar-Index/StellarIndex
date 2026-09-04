//go:build integration

package integration_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestEarliestBucket_AgainstTimescale executes the coverage-floor probe
// (timescale.Store.EarliestBucket, the read behind the API's
// `coverage_from` / `outside_coverage` signal) against a real
// TimescaleDB with the migration chain applied.
//
// It exists because the probe fails SILENTLY by design: a read error
// yields "no signal" plus a warning, never a 5xx. The unit tests in
// internal/storage/timescale stop at the guards that run before the
// query, so a column rename, a plan the CAGG's index cannot serve, or a
// bind-shape mismatch would ship the feature inert with every gate
// green. This test is the one place the SQL is run.
//
// Four properties are pinned on the both-orientations read, each the
// thing that would otherwise make the served floor too LATE — the one
// direction of error that turns a quiet window into a false "before
// the history held":
//
//   - alias fold: a market stored under the CEX spelling (crypto:XLM)
//     is found by its native spelling, and vice versa;
//   - direction fold: a market stored only in the reverse orientation
//     is found by the requested one, with the same floor;
//   - lower bound: a `from` above the first bucket returns the next
//     one, so the window really is applied;
//   - closed-bucket guard: a pair whose only daily bucket is still
//     open reports no floor at all.
//
// The stored-orientation read (EarliestBucketAsStored) is then pinned
// to span ONE orientation, and the quote-literal read
// (EarliestBucketLiteralQuote) to span ONE quote spelling — the read
// the fiat-quoted /v1/ohlc series takes, whose combine names each
// constituent's quote in a single form. Both narrowings are executed
// against the migrated schema for the same reason the wide read is: the
// bound arrays differ, and a bind PostgreSQL rejects would be swallowed
// as "no signal".
//
// Finally the reads are exercised through the served surfaces:
// /v1/ohlc measures a fiat-quoted pair over its USD-pegged
// constituents, and /v1/history measures the orientation its page read
// spans — against the real prices_1d, not a double.
func TestEarliestBucket_AgainstTimescale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	mustAsset := func(code string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	usdc := mustAsset("USDC")
	aqua := mustAsset("AQUA")
	fresh := mustAsset("FRSH")
	other := mustAsset("OTHR")
	cryptoXLM, err := c.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()

	mustPair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	const day = 24 * time.Hour
	now := time.Now().UTC()

	// Market A lives ONLY under the CEX spelling (crypto:XLM / USDC), in
	// the requested direction, across two closed days.
	xlmFirst := now.Add(-5 * day)
	xlmSecond := xlmFirst.Add(day)
	// Market B lives ONLY in the reverse orientation (USDC / AQUA), as
	// the SDEX decoder records whichever way the venue quoted it.
	aquaFirst := now.Add(-3 * day)
	// Market C has exactly one trade, inside TODAY's bucket — a bucket
	// that has not closed. Clamped to the bucket's own start so the case
	// is deterministic across a UTC-midnight boundary.
	openTS := now.Add(-time.Second)
	if start := now.Truncate(day); openTS.Before(start) {
		openTS = start
	}

	for i, tr := range []c.Trade{
		mkAPITrade(1, xlmFirst, mustPair(cryptoXLM, usdc), 1_000_000_000, 12_000_000),
		mkAPITrade(2, xlmSecond, mustPair(cryptoXLM, usdc), 1_000_000_000, 12_100_000),
		mkAPITrade(3, aquaFirst, mustPair(usdc, aqua), 10_000_000, 4_000_000_000),
		mkAPITrade(4, openTS, mustPair(fresh, usdc), 1_000_000_000, 5_000_000),
		// Market D is quoted in ONE spelling of XLM (crypto:XLM), so a
		// quote-alias-folded probe finds it from `native` and a
		// quote-literal one does not. XLM's three canonical forms need
		// no registry, which keeps this case free of process-global
		// fixture state.
		mkAPITrade(5, aquaFirst, mustPair(other, cryptoXLM), 10_000_000, 4_000_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}
	// Background workers are off in this harness (see startTimescale),
	// so the daily rung is materialised by hand — the same rung the API
	// probes on, and the one the served floor is a property of.
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1d: %v", err)
	}

	// The API's probe window: the network's first possible bucket up to
	// a Go-side now.
	epoch := time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)
	xlmFloor := xlmFirst.Truncate(day)
	aquaFloor := aquaFirst.Truncate(day)

	cases := []struct {
		name      string
		pair      c.Pair
		from      time.Time
		want      time.Time
		wantFound bool
	}{
		// Market A, every spelling and both orientations → one floor.
		{"stored spelling, stored direction", mustPair(cryptoXLM, usdc), epoch, xlmFloor, true},
		{"alias fold: native reads the crypto:XLM market", mustPair(native, usdc), epoch, xlmFloor, true},
		{"alias fold and flipped direction", mustPair(usdc, native), epoch, xlmFloor, true},
		{"flipped direction under the stored spelling", mustPair(usdc, cryptoXLM), epoch, xlmFloor, true},
		// Market B, stored reverse only → found through the flipped arm.
		{"reverse-stored market by the requested direction", mustPair(aqua, usdc), epoch, aquaFloor, true},
		{"reverse-stored market by its stored direction", mustPair(usdc, aqua), epoch, aquaFloor, true},
		// The lower bound is applied: a `from` above the first bucket
		// returns the next one, not the first.
		{"lower bound excludes the first bucket", mustPair(native, usdc), xlmFloor.Add(day), xlmSecond.Truncate(day), true},
		{"lower bound above every bucket", mustPair(native, usdc), now.Add(-day), time.Time{}, false},
		// No rows at all for the pair.
		{"pair absent from the rung", mustPair(other, aqua), epoch, time.Time{}, false},
		// A bucket that has not closed is not a floor.
		{"open bucket only", mustPair(fresh, usdc), epoch, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := store.EarliestBucket(ctx, tc.pair, timescale.Granularity1d, tc.from, now)
			if err != nil {
				t.Fatalf("EarliestBucket(%s/%s): %v", tc.pair.Base, tc.pair.Quote, err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v (bucket %v), want %v", found, got, tc.wantFound)
			}
			if !found {
				if !got.IsZero() {
					t.Errorf("bucket = %v on a not-found return, want zero", got)
				}
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("floor = %v, want %v", got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("floor location = %v, want UTC", got.Location())
			}
		})
	}

	// The stored-orientation read: market B is found under the
	// orientation it was stored in and under NO other; the alias fold
	// on each leg still applies (market A under its native spelling).
	storedCases := []struct {
		name      string
		pair      c.Pair
		want      time.Time
		wantFound bool
	}{
		{"reverse-stored market by its stored direction", mustPair(usdc, aqua), aquaFloor, true},
		{"reverse-stored market by the requested direction", mustPair(aqua, usdc), time.Time{}, false},
		{"alias fold still applies within the orientation", mustPair(native, usdc), xlmFloor, true},
		{"flipped alias spelling is not found", mustPair(usdc, native), time.Time{}, false},
	}
	for _, tc := range storedCases {
		t.Run("stored orientation/"+tc.name, func(t *testing.T) {
			got, found, err := store.EarliestBucketAsStored(ctx, tc.pair, timescale.Granularity1d, epoch, now)
			if err != nil {
				t.Fatalf("EarliestBucketAsStored(%s/%s): %v", tc.pair.Base, tc.pair.Quote, err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v (bucket %v), want %v", found, got, tc.wantFound)
			}
			if found && !got.Equal(tc.want) {
				t.Errorf("floor = %v, want %v", got, tc.want)
			}
		})
	}

	// The quote-literal read: the base leg keeps its alias family and
	// both directions are still folded, but the quote leg is the
	// spelling that was asked for and nothing else. Market D is stored
	// under `crypto:XLM` as its quote.
	literalCases := []struct {
		name      string
		pair      c.Pair
		want      time.Time
		wantFound bool
	}{
		{"the stored quote spelling is found", mustPair(other, cryptoXLM), aquaFloor, true},
		{"a sibling quote spelling is not", mustPair(other, native), time.Time{}, false},
		{"the base leg still folds its family", mustPair(cryptoXLM, usdc), xlmFloor, true},
		{"and so does the direction", mustPair(usdc, cryptoXLM), xlmFloor, true},
	}
	for _, tc := range literalCases {
		t.Run("literal quote/"+tc.name, func(t *testing.T) {
			got, found, err := store.EarliestBucketLiteralQuote(ctx, tc.pair, timescale.Granularity1d, epoch, now)
			if err != nil {
				t.Fatalf("EarliestBucketLiteralQuote(%s/%s): %v", tc.pair.Base, tc.pair.Quote, err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v (bucket %v), want %v", found, got, tc.wantFound)
			}
			if found && !got.Equal(tc.want) {
				t.Errorf("floor = %v, want %v", got, tc.want)
			}
		})
	}
	// The contrast that makes the narrowing observable: the wide read
	// DOES reach market D from the sibling spelling.
	t.Run("literal quote/the wide read still folds the quote family", func(t *testing.T) {
		got, found, err := store.EarliestBucket(ctx, mustPair(other, native), timescale.Granularity1d, epoch, now)
		if err != nil {
			t.Fatalf("EarliestBucket: %v", err)
		}
		if !found || !got.Equal(aquaFloor) {
			t.Errorf("EarliestBucket(OTHR/native) = (%v, %v), want (%v, true) — the fold under test is unobservable", got, found, aquaFloor)
		}
	})

	// The served surfaces, wired the way the binary wires them, against
	// this prices_1d. The request window ends two days before market B's
	// only bucket, so every answer below is empty and the floor is the
	// whole signal.
	srv := v1.New(v1.Options{
		History:           apiHistoryAdapter{s: store},
		CoverageFloor:     apiCoverageFloorAdapter{s: store},
		USDPeggedClassics: []c.Asset{usdc},
	})
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)
	window := "&from=" + now.Add(-10*day).Format(time.RFC3339) + "&to=" + now.Add(-5*day).Format(time.RFC3339)

	type served struct {
		CoverageFrom *time.Time `json:"coverage_from"`
		Flags        struct {
			OutsideCoverage bool `json:"outside_coverage"`
		} `json:"flags"`
	}

	// AQUA/fiat:USD holds no bucket under its literal pair; its
	// /v1/ohlc series is combined from the USD-pegged constituents, one
	// of which (AQUA/USDC, stored as USDC/AQUA) does. The floor must be
	// that constituent's, and the window is below it.
	t.Run("served//v1/ohlc fiat quote measures the constituent set", func(t *testing.T) {
		var got served
		getJSON(t, api.URL+"/v1/ohlc?base="+aqua.String()+"&quote=fiat:USD&interval=1d"+window, &got)
		if got.CoverageFrom == nil {
			t.Fatalf("coverage_from absent; want the USDC constituent's %s", aquaFloor.Format(time.RFC3339))
		}
		if !got.CoverageFrom.Equal(aquaFloor) {
			t.Errorf("coverage_from = %s, want %s", got.CoverageFrom.Format(time.RFC3339), aquaFloor.Format(time.RFC3339))
		}
		if !got.Flags.OutsideCoverage {
			t.Errorf("flags.outside_coverage = false for a window ending below the constituent floor")
		}
	})

	// /v1/history reads one stored orientation: AQUA/USDC has no rows
	// under that orientation, so it carries no floor, while USDC/AQUA
	// carries the floor and the flag.
	t.Run("served//v1/history measures the requested orientation only", func(t *testing.T) {
		var flipped served
		getJSON(t, api.URL+"/v1/history?base="+aqua.String()+"&quote="+usdc.String()+window, &flipped)
		if flipped.CoverageFrom != nil || flipped.Flags.OutsideCoverage {
			t.Errorf("AQUA/USDC page carried coverage_from=%v outside=%v; the page read never returns the USDC/AQUA rows, so it must carry no floor",
				flipped.CoverageFrom, flipped.Flags.OutsideCoverage)
		}
		var stored served
		getJSON(t, api.URL+"/v1/history?base="+usdc.String()+"&quote="+aqua.String()+window, &stored)
		if stored.CoverageFrom == nil || !stored.CoverageFrom.Equal(aquaFloor) {
			t.Errorf("USDC/AQUA page coverage_from = %v, want %s", stored.CoverageFrom, aquaFloor.Format(time.RFC3339))
		}
		if !stored.Flags.OutsideCoverage {
			t.Errorf("USDC/AQUA page flags.outside_coverage = false for a window ending below the floor")
		}
	})
}

// apiCoverageFloorAdapter mirrors cmd/stellarindex-api/main.go's
// storeCoverageFloorReader so the served-surface cases above exercise
// the same read path production does.
type apiCoverageFloorAdapter struct{ s *timescale.Store }

func (a apiCoverageFloorAdapter) EarliestBucket(ctx context.Context, pair c.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return a.s.EarliestBucket(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

func (a apiCoverageFloorAdapter) EarliestBucketAsStored(ctx context.Context, pair c.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return a.s.EarliestBucketAsStored(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

func (a apiCoverageFloorAdapter) EarliestBucketLiteralQuote(ctx context.Context, pair c.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return a.s.EarliestBucketLiteralQuote(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}
