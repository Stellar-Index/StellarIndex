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
// THE DEFECT THIS FILE EXISTS FOR (2026-09-07). A chart fix took a
// handler from ONE reader call to TWENTY-FOUR under an unchanged 8s
// deadline, and every gate passed: build, vet, lint, the full package
// suite, nine tests proven red. Fixtures answer in microseconds, so a
// fan-out costs zero test-milliseconds and no assertion in the package
// could see it. It reached production and turned a 1.1s request into
// 8.6s cold; only a live probe found it.
//
// A read-count assertion is the missing axis. The handlers below walk a
// LIST of source pairs and stop early only when the merge already
// covers the requested window ([Server.chartStablecoinFallback]) — so
// the cost of a request is the LENGTH of that list whenever the store
// answers nothing, which on this deployment is the common case: 59 of
// the 60 largest assets are carried entirely by a proxy read, and every
// proxy read therefore runs on an empty merge. The walk's own guard is
// chartWalkBudget, two seconds of WALL CLOCK, which a fixture can never
// exhaust.
//
// The numbers here are MEASURED against the code as it stands, not
// chosen. Each is stated with what it counts. Raising one is allowed;
// raising one silently is what this file refuses.

// countingHistoryReader is a DECORATOR, not a mock: it counts calls and
// delegates to a real fixture reader, so the counts come from the
// handler's own control flow and the data still comes from the stub the
// rest of the package uses.
//
// Every method of [v1.HistoryReader] is overridden rather than left to
// the embedded value, so a method that grows a new caller is counted
// from the day it does.
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

// total is every store round-trip the request made, whichever method
// carried it. Pinned beside the per-method counts so a fan-out that
// MOVES between methods (a walk switching from prices_<gran> to
// twap_<gran>, say) cannot come out even.
func (c *countingHistoryReader) total() int {
	return c.tradesInRange + c.tradesInRangeAfter + c.historyPoints +
		c.historyPointsInRange + c.twapPointsInRange + c.ohlcSeries +
		c.latestTradePerSource
}

// countingCoverageFloorReader is the same decorator for the
// coverage-floor probe, which is a SECOND fan-out on the same
// request: a floor is consulted only when the serving read came back
// empty, which is exactly the case that also pays the full walk.
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

// r1SACWrapperUSDC is the SAC wrapper r1 declares for the operator's
// one USD peg, copied from `[supply.sac_wrappers]` in
// /etc/stellarindex.toml (read 2026-09-07). It is asserted equal to the
// value derived from the classic asset in
// TestReadBudget_FixtureMatchesDeployedConfiguration, so this constant
// cannot drift into a fiction.
const r1SACWrapperUSDC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

// productionShapedServer builds the server the way cmd/stellarindex-api
// does for the surfaces measured here, which is the whole point of the
// exercise: several tests in this package construct a server whose peg
// list is empty and whose alias registry is the XLM-only baseline, and
// a budget measured on one of those counts a fan-out no deployment
// runs.
//
// Two pieces of operator configuration decide the walk's length, and
// both are taken from r1:
//
//   - trades.usd_pegged_classic_assets — exactly one entry, Circle's
//     classic USDC, which is also this package's testUSDCIssuer;
//   - supply.sac_wrappers — which maps that peg's SAC contract back to
//     the classic asset, so canonical.AssetAliases returns TWO forms of
//     the peg instead of one and the walk's held-back pass has
//     something to read.
//
// Without the registry the same request issues 21 reads instead of 24;
// TestReadBudget_AliasRegistryWidensTheWalk pins that difference so the
// gap between a bare unit test and the deployment stays visible.
//
// The reader is wrapped in v1.NewCachedHistoryReader at production's 2m
// TTL. That wrapper caches LatestTradePerSource ONLY and passes every
// other method through, so it must not move any count here; the
// no-cache/cache equality is asserted rather than assumed.
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

// installProductionAliasRegistry publishes r1's declared SAC wrappers
// as the process-wide alias registry for the duration of one test,
// mirroring cmd/stellarindex-api's start-up install and the same
// install/restore idiom internal/canonical and internal/aggregate use.
//
// Not parallel-safe by construction (the registry is process-wide), so
// no test that calls it may call t.Parallel(); Go runs the sequential
// tests to completion before releasing the parallel ones, so the
// package's existing parallel tests never observe the installed
// registry.
func installProductionAliasRegistry(t *testing.T) {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(map[string]string{
		r1SACWrapperUSDC: "USDC:" + testUSDCIssuer,
	})
	if err != nil {
		t.Fatalf("build alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
}

// TestReadBudget_FixtureMatchesDeployedConfiguration pins the claim the
// rest of this file rests on: that the registry installed here is the
// one r1 runs. The SAC contract id is a hash of the classic asset, so
// deriving it and comparing against the deployed literal proves the
// fixture is the deployment's own wrapper and not a plausible-looking
// string.
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

// TestReadBudget_ColdRequestFanOut pins how many store round-trips each
// chart/history surface makes when the store answers NOTHING — the
// state that costs the most and the one the 8s ceiling is spent in.
//
// Every number below was measured against this tree, under the
// production-shaped construction above. What produces them:
//
//	3 spellings of the base (native, crypto:XLM and XLM's SAC —
//	  canonical.AssetAliases' unconditional three-way split)
//	× 1 literal fiat quote                                    =  3 alias reads
//	3 spellings of the base
//	× (1 classic peg + 5 abstract USD backers)                = 18 proxy reads
//	3 spellings of the base
//	× 1 held-back spelling (the peg's SAC wrapper)            =  3 proxy reads
//	                                                           ────
//	                                                             24
//
// A crypto quote has no proxy list at all, which is why it costs 3.
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

// TestReadBudget_CoveredWindowCostsOneRead is the other half of the
// pair the defect sits between: the SAME handler, the same walk, one
// read. The walk stops as soon as the merge covers the requested
// window, so a pair whose literal spelling holds a full series never
// reaches the proxy list.
//
// This is the "1" in "1 reader call to 24". Pinning only the cold
// number would let a change that broke the early stop — making every
// warm request pay the full walk — pass unnoticed.
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

// TestReadBudget_AliasRegistryWidensTheWalk pins the gap between what a
// bare unit test measures and what the deployment runs.
//
// A test that installs no alias registry resolves against the XLM-only
// baseline, where the operator's classic peg has ONE canonical form and
// the walk's held-back pass reads nothing: 21 calls, not 24. Every
// other test in this package runs in exactly that state. Pinning both
// numbers keeps the difference an asserted fact rather than a trap the
// next budget walks into.
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

// TestReadBudget_ProductionCacheWrapperIsTransparent pins the reason
// the counts above are the counts production pays.
//
// cmd/stellarindex-api wires History as
// v1.NewCachedHistoryReader(store, 2*time.Minute). That wrapper caches
// LatestTradePerSource ONLY (#29) and embeds the reader for everything
// else, so the chart's reads reach the database one for one. If a
// future change caches a chart method there, this assertion is where
// the budgets above have to be re-derived rather than quietly
// over-counting.
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
