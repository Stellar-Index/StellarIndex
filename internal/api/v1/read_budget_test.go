package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── read budgets ───────────────────────────────────────────────────
//
// A handler can go from ONE reader call to TWENTY-FOUR under an unchanged 8s deadline with every
// other gate green: fixtures answer in microseconds, so no latency assertion sees a fan-out.
// A read-count assertion is the missing axis.
//
// The handlers below walk a LIST of source pairs and stop early only when the merge covers the
// requested window ([Server.chartStablecoinFallback]), so a request costs the LENGTH of that list
// whenever the store answers nothing, the common case for assets carried by a proxy read. The
// walk's own guard, chartWalkBudget (2s wall clock), a fixture can never exhaust.
//
// The numbers are MEASURED against the code, not chosen. Raising one is allowed; raising one
// silently is what this file refuses.

// countingHistoryReader is a decorator, not a mock: it counts calls and delegates to a real fixture reader.
// Every [v1.HistoryReader] method is overridden so a method that grows a new caller is counted from day one.
type countingHistoryReader struct {
	v1.HistoryReader

	tradesInRange        int
	tradesInRangeAfter   int
	historyPoints        int
	historyPointsInRange int
	twapPointsInRange    int
	ohlcSeries           int
	latestTradePerSource int
}

func (c *countingHistoryReader) TradesInRange(
	ctx context.Context, pair canonical.Pair, from, to time.Time, limit int,
) ([]canonical.Trade, error) {
	c.tradesInRange++
	return c.HistoryReader.TradesInRange(ctx, pair, from, to, limit)
}

func (c *countingHistoryReader) TradesInRangeAfter(
	ctx context.Context, pair canonical.Pair, from, to, afterTs time.Time,
	afterLedger uint32, afterTxHash, afterSource string, afterOpIndex uint32, limit int,
) ([]canonical.Trade, error) {
	c.tradesInRangeAfter++
	return c.HistoryReader.TradesInRangeAfter(ctx, pair, from, to, afterTs,
		afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
}

func (c *countingHistoryReader) HistoryPoints(
	ctx context.Context, pair canonical.Pair, granularity string, limit int,
) ([]v1.HistoryPoint, error) {
	c.historyPoints++
	return c.HistoryReader.HistoryPoints(ctx, pair, granularity, limit)
}

func (c *countingHistoryReader) HistoryPointsInRange(
	ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	c.historyPointsInRange++
	return c.HistoryReader.HistoryPointsInRange(ctx, pair, granularity, from, to, limit)
}

func (c *countingHistoryReader) TWAPPointsInRange(
	ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	c.twapPointsInRange++
	return c.HistoryReader.TWAPPointsInRange(ctx, pair, granularity, from, to, limit)
}

func (c *countingHistoryReader) OHLCSeries(
	ctx context.Context, pair canonical.Pair, interval string, from, to time.Time, limit int,
) ([]v1.OHLCSeriesBar, error) {
	c.ohlcSeries++
	return c.HistoryReader.OHLCSeries(ctx, pair, interval, from, to, limit)
}

func (c *countingHistoryReader) LatestTradePerSource(
	ctx context.Context, pair canonical.Pair, sourceFilter string,
) ([]canonical.Trade, error) {
	c.latestTradePerSource++
	return c.HistoryReader.LatestTradePerSource(ctx, pair, sourceFilter)
}

// total is every store round-trip the request made. Pinned beside per-method counts so a fan-out that moves between
// methods (e.g. prices_<gran> to twap_<gran>) cannot come out even.
func (c *countingHistoryReader) total() int {
	return c.tradesInRange + c.tradesInRangeAfter + c.historyPoints +
		c.historyPointsInRange + c.twapPointsInRange + c.ohlcSeries +
		c.latestTradePerSource
}

// countingCoverageFloorReader is the same decorator for the coverage-floor probe, a second fan-out on the same request:
// a floor is consulted only when the serving read came back empty, which also pays the full walk.
type countingCoverageFloorReader struct{ calls int }

func (c *countingCoverageFloorReader) EarliestBucket(
	_ context.Context, _ canonical.Pair, _ string, _, _ time.Time,
) (time.Time, bool, error) {
	c.calls++
	return time.Time{}, false, nil
}

func (c *countingCoverageFloorReader) EarliestBucketAsStored(
	_ context.Context, _ canonical.Pair, _ string, _, _ time.Time,
) (time.Time, bool, error) {
	c.calls++
	return time.Time{}, false, nil
}

func (c *countingCoverageFloorReader) EarliestBucketLiteralQuote(
	_ context.Context, _ canonical.Pair, _ string, _, _ time.Time,
) (time.Time, bool, error) {
	c.calls++
	return time.Time{}, false, nil
}

// r1SACWrapperUSDC is the SAC wrapper r1 declares for the one USD peg, from `[supply.sac_wrappers]` in
// /etc/stellarindex.toml; TestReadBudget_FixtureMatchesDeployedConfiguration asserts it equals the derived value.
const r1SACWrapperUSDC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

// productionShapedServer builds the server the way cmd/stellarindex-api does for the surfaces
// measured here. Other tests use an empty peg list and the XLM-only alias registry, and a budget
// measured on those counts a fan-out no deployment runs.
//
// Two pieces of operator configuration (from r1) decide the walk's length:
//
//   - trades.usd_pegged_classic_assets: one entry, Circle's classic USDC (testUSDCIssuer);
//   - supply.sac_wrappers: maps that peg's SAC back to the classic asset, so
//     canonical.AssetAliases returns TWO forms and the held-back pass has something to read.
//
// Without the registry the same request issues 21 reads instead of 24;
// TestReadBudget_AliasRegistryWidensTheWalk pins that.
//
// The reader is wrapped in v1.NewCachedHistoryReader at production's 2m TTL. It caches
// LatestTradePerSource ONLY, so it must not move any count; no-cache/cache equality is asserted.
func productionShapedServer(
	t *testing.T, reader v1.HistoryReader, floor v1.CoverageFloorReader, cached bool,
) *testServer {
	t.Helper()
	usdc, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("build the operator's USD peg: %v", err)
	}
	history := reader
	if cached {
		history = v1.NewCachedHistoryReader(reader, 2*time.Minute)
	}
	srv := v1.New(v1.Options{
		History:           history,
		CoverageFloor:     floor,
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	return httpTestServer(t, srv)
}

// installProductionAliasRegistry publishes r1's SAC wrappers as the process-wide alias registry for one test, mirroring
// cmd/stellarindex-api start-up. Not parallel-safe: no caller may t.Parallel(); sequential tests finish before parallel
// ones start.
func installProductionAliasRegistry(t *testing.T) {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{
		r1SACWrapperUSDC: "USDC:" + testUSDCIssuer,
	})
	if err != nil {
		t.Fatalf("build alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
}

// TestReadBudget_FixtureMatchesDeployedConfiguration pins that the installed registry is r1's: the SAC id is a hash of the
// classic asset, so deriving it and comparing to the deployed literal proves the fixture is the real wrapper.
func TestReadBudget_FixtureMatchesDeployedConfiguration(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("build the operator's USD peg: %v", err)
	}
	derived, err := usdc.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC contract id: %v", err)
	}
	if derived != r1SACWrapperUSDC {
		t.Errorf("SAC wrapper for USDC-%s = %q, want %q (the [supply.sac_wrappers] entry deployed on r1)",
			testUSDCIssuer, derived, r1SACWrapperUSDC)
	}
}

// TestReadBudget_ColdRequestFanOut pins store round-trips per chart/history surface when the store answers NOTHING, measured
// under the production-shaped server: 3 base spellings x (1 literal fiat quote + 6 proxy pegs) + 3 held-back SAC reads = 24. A
// crypto quote has no proxy list, so it costs 3.
func TestReadBudget_ColdRequestFanOut(t *testing.T) {
	installProductionAliasRegistry(t)

	for _, tc := range []struct {
		name string
		path string
		// wantMethod is the count on the ONE reader method the surface
		// uses; wantTotal is every method summed, so a fan-out that
		// migrates between methods still moves a number.
		method     func(*countingHistoryReader) int
		wantMethod int
		wantTotal  int
		wantFloor  int
		what       string
	}{
		{
			name:       "chart_vwap_fiat_quote",
			path:       "/v1/chart?asset=native&quote=fiat:USD&timeframe=24h",
			method:     func(c *countingHistoryReader) int { return c.historyPointsInRange },
			wantMethod: 24,
			wantTotal:  24,
			wantFloor:  7,
			what: "prices_<gran> reads: 3 base spellings × (1 literal quote + 6 proxy quotes + 1 held-back SAC quote), " +
				"then 7 coverage-floor probes because the series came back empty",
		},
		{
			name:       "chart_twap_fiat_quote",
			path:       "/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&price_type=twap",
			method:     func(c *countingHistoryReader) int { return c.twapPointsInRange },
			wantMethod: 24,
			wantTotal:  24,
			wantFloor:  0,
			what:       "twap_<gran> reads over the SAME source-pair walk; the TWAP path consults no coverage floor",
		},
		{
			name:       "chart_vwap_crypto_quote",
			path:       "/v1/chart?asset=native&quote=crypto:USDT&timeframe=24h",
			method:     func(c *countingHistoryReader) int { return c.historyPointsInRange },
			wantMethod: 3,
			wantTotal:  3,
			wantFloor:  1,
			what:       "a non-fiat quote has no stablecoin-proxy list, so the walk is the 3 base spellings and nothing more",
		},
		{
			name:       "history_since_inception",
			path:       "/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d",
			method:     func(c *countingHistoryReader) int { return c.historyPoints },
			wantMethod: 24,
			wantTotal:  24,
			wantFloor:  0,
			what:       "the same walk with no lower bound — one definition, so it cannot drift from /v1/chart's",
		},
		{
			name:       "ohlc_series",
			path:       "/v1/ohlc?base=native&quote=fiat:USD&interval=1h",
			method:     func(c *countingHistoryReader) int { return c.ohlcSeries },
			wantMethod: 24,
			wantTotal:  24,
			wantFloor:  8,
			what:       "the OHLC series walks the same constituent set over the ohlc_<interval> CAGGs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &countingHistoryReader{HistoryReader: &stubHistoryReader{}}
			floor := &countingCoverageFloorReader{}
			ts := productionShapedServer(t, reader, floor, true)

			resp := mustGet(t, ts.URL+tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, tc.path)
			}
			if got := tc.method(reader); got != tc.wantMethod {
				t.Errorf("reader calls = %d, want %d — %s\n"+
					"a change to this number is a change to the request's cost under an 8s deadline; "+
					"re-measure it against the deployment before moving it",
					got, tc.wantMethod, tc.what)
			}
			if got := reader.total(); got != tc.wantTotal {
				t.Errorf("total reader calls (all methods) = %d, want %d", got, tc.wantTotal)
			}
			if got := floor.calls; got != tc.wantFloor {
				t.Errorf("coverage-floor probes = %d, want %d — the floor is a SECOND fan-out on the "+
					"same empty-answer path", got, tc.wantFloor)
			}
		})
	}
}

// TestReadBudget_CoveredWindowCostsOneRead: the same walk costs one read once the merge covers the window.
// Pinning only the cold number would let a broken early stop (every warm request paying the full walk) pass.
func TestReadBudget_CoveredWindowCostsOneRead(t *testing.T) {
	installProductionAliasRegistry(t)

	// A dense 1m series covering the whole 24h window, under the
	// literal pair, so the first alias read claims every bucket.
	now := time.Now().UTC().Truncate(time.Minute)
	points := make([]v1.HistoryPoint, 0, 1500)
	for i := 1500; i >= 1; i-- {
		points = append(points, v1.HistoryPoint{
			Bucket:    now.Add(-time.Duration(i) * time.Minute),
			VWAP:      "0.1234567",
			VolumeUSD: ptr("100"),
		})
	}

	for _, tc := range []struct {
		name   string
		path   string
		method func(*countingHistoryReader) int
	}{
		{
			"vwap", "/v1/chart?asset=native&quote=fiat:USD&timeframe=24h",
			func(c *countingHistoryReader) int { return c.historyPointsInRange },
		},
		{
			"twap", "/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&price_type=twap",
			func(c *countingHistoryReader) int { return c.twapPointsInRange },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &countingHistoryReader{HistoryReader: &stubHistoryReader{
				points: points, twapPoints: points,
			}}
			floor := &countingCoverageFloorReader{}
			ts := productionShapedServer(t, reader, floor, true)

			resp := mustGet(t, ts.URL+tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := tc.method(reader); got != 1 {
				t.Errorf("reader calls = %d, want 1 — a series that already covers the requested "+
					"window must stop the source walk at its first read", got)
			}
			if got := floor.calls; got != 0 {
				t.Errorf("coverage-floor probes = %d, want 0 — a non-empty series asks no floor", got)
			}
		})
	}
}

// TestReadBudget_AliasRegistryWidensTheWalk pins that without an alias registry (every other test) the XLM-only baseline
// gives the classic peg one canonical form and the held-back pass reads nothing: 21 calls, not 24.
func TestReadBudget_AliasRegistryWidensTheWalk(t *testing.T) {
	const path = "/v1/chart?asset=native&quote=fiat:USD&timeframe=24h"

	measure := func(t *testing.T) int {
		t.Helper()
		reader := &countingHistoryReader{HistoryReader: &stubHistoryReader{}}
		ts := productionShapedServer(t, reader, &countingCoverageFloorReader{}, true)
		if resp := mustGet(t, ts.URL+path); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		return reader.historyPointsInRange
	}

	t.Run("baseline_registry", func(t *testing.T) {
		if got := measure(t); got != 21 {
			t.Errorf("reader calls = %d, want 21 with no SAC wrappers declared "+
				"(3 base spellings × 7 quote spellings)", got)
		}
	})

	t.Run("deployed_registry", func(t *testing.T) {
		installProductionAliasRegistry(t)
		if got := measure(t); got != 24 {
			t.Errorf("reader calls = %d, want 24 with r1's [supply.sac_wrappers] installed — "+
				"the peg's SAC wrapper adds one held-back quote spelling per base spelling", got)
		}
	})
}

// TestReadBudget_ProductionCacheWrapperIsTransparent pins that NewCachedHistoryReader(store, 2m) caches only
// LatestTradePerSource, so chart reads hit the DB one for one; if a chart method gets cached, re-derive the budgets.
func TestReadBudget_ProductionCacheWrapperIsTransparent(t *testing.T) {
	installProductionAliasRegistry(t)
	const path = "/v1/chart?asset=native&quote=fiat:USD&timeframe=24h"

	counts := make(map[bool]int, 2)
	for _, cached := range []bool{false, true} {
		reader := &countingHistoryReader{HistoryReader: &stubHistoryReader{}}
		ts := productionShapedServer(t, reader, &countingCoverageFloorReader{}, cached)
		if resp := mustGet(t, ts.URL+path); resp.StatusCode != http.StatusOK {
			t.Fatalf("cached=%v: status = %d, want 200", cached, resp.StatusCode)
		}
		counts[cached] = reader.total()
	}
	if counts[false] != counts[true] {
		t.Errorf("reader calls bare = %d, through NewCachedHistoryReader = %d — the production "+
			"wrapper is no longer transparent to the chart path, so every budget in this file "+
			"is measuring something the deployment does not do",
			counts[false], counts[true])
	}
	if counts[true] != 24 {
		t.Errorf("reader calls through the production wrapper = %d, want 24", counts[true])
	}
}
