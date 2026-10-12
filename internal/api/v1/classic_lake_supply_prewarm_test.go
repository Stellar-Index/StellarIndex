// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// classic_lake_supply.go warms itself from the request path only: a listing request kicks a
// detached refresh for classicLakeSupplyBatch of the assets it asked about and serves the
// trustline sum for the rest. With no consumer traffic the cache never converges, entries expire
// unread at classicLakeSupplyTTL, and /v1/assets and /v1/rwa/assets fall back to the
// trustline-only sum, which is blind to claimable-balance, LP-reserve and SAC-held supply and so
// understates supply badly. Every test here asks whether the cache is filled without any request.
//
// The CETES shape from classic_lake_supply_test.go supplies the numbers: a trustline sum of
// 100.0000000 against a true supply of 136.6000000.

// discardLakeLogger keeps the prewarm's debug lines out of the test output.
func discardLakeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// prewarmLakeStub answers the bulk lake-flows read and records each read as a batch, so a test can assert the background driver
// respects classicLakeSupplyBatch instead of requesting the whole population in one query.
type prewarmLakeStub struct {
	mu         sync.Mutex
	byContract map[string]clickhouse.TokenSupply
	batches    [][]string
	err        error
}

func (l *prewarmLakeStub) TokenSupply(_ context.Context, contractID string) (clickhouse.TokenSupply, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return clickhouse.TokenSupply{}, l.err
	}
	return l.byContract[contractID], nil
}

func (l *prewarmLakeStub) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("not used by these tests")
}

func (l *prewarmLakeStub) TokenSupplyForContracts(
	_ context.Context, ids []string,
) (map[string]clickhouse.TokenSupply, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batches = append(l.batches, append([]string(nil), ids...))
	if l.err != nil {
		return nil, l.err
	}
	out := make(map[string]clickhouse.TokenSupply, len(ids))
	for _, id := range ids {
		if sup, ok := l.byContract[id]; ok {
			out[id] = sup
		}
	}
	return out, nil
}

// batchSizes returns the length of each read the stub served.
func (l *prewarmLakeStub) batchSizes() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	sizes := make([]int, 0, len(l.batches))
	for _, b := range l.batches {
		sizes = append(sizes, len(b))
	}
	return sizes
}

// contractsAsked returns every contract id the stub has been asked about.
func (l *prewarmLakeStub) contractsAsked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, b := range l.batches {
		out = append(out, b...)
	}
	return out
}

// prewarmListingAssets is the listing reader the prewarm interrogates for its
// population. It serves a fixed page (truncated to opts.Limit, the way a real
// listing query is) and records the options it was asked for.
type prewarmListingAssets struct {
	AssetsReader // nil embedded — the prewarm calls ListAssetsExt only
	rows         []timescale.AssetRow

	mu   sync.Mutex
	seen []timescale.ListAssetsOptions
}

func (a *prewarmListingAssets) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	a.mu.Lock()
	a.seen = append(a.seen, opts)
	a.mu.Unlock()
	n := opts.Limit
	if n <= 0 || n > len(a.rows) {
		n = len(a.rows)
	}
	return a.rows[:n], nil
}

// classicListingRow builds one classic listing row with a $1.00 price, the
// shape /v1/assets serves.
func classicListingRow(code string) timescale.AssetRow {
	price := "1.00"
	return timescale.AssetRow{
		AssetID:       code + "-" + cetesIssuer,
		Code:          code,
		IssuerGStrkey: cetesIssuer,
		PriceUSD:      &price,
	}
}

// lakeSupplyPrewarmServer builds a Server through the production constructor [New] with the three seams main.go wires
// (TokenSupply, AssetsReader, Explorer). Constructed, not struct-literalled, because the wiring is what is under test.
func lakeSupplyPrewarmServer(
	t *testing.T, rows []timescale.AssetRow, lakeTotals map[string]string,
) (*Server, *prewarmLakeStub, *prewarmListingAssets) {
	t.Helper()

	lake := &prewarmLakeStub{byContract: map[string]clickhouse.TokenSupply{}}
	for assetID, raw := range lakeTotals {
		sac, err := mustSACFor(assetID)
		if err != nil {
			t.Fatalf("derive SAC for %s: %v", assetID, err)
		}
		total, ok := new(big.Int).SetString(raw, 10)
		if !ok {
			t.Fatalf("bad lake total %q for %s", raw, assetID)
		}
		lake.byContract[sac] = clickhouse.TokenSupply{
			ContractID: sac,
			Total:      total,
			Mint:       total,
			Burn:       big.NewInt(0),
			Clawback:   big.NewInt(0),
			FlowCount:  42,
		}
	}

	trustlines := make(map[string]string, len(rows))
	for _, row := range rows {
		trustlines[row.AssetID] = cetesTrustlineOnly
	}
	assets := &prewarmListingAssets{rows: rows}

	s := New(Options{
		Logger:       discardLakeLogger(),
		TokenSupply:  lake,
		AssetsReader: assets,
		Explorer:     &trustlineOnlyExplorer{supply: trustlines},
	})
	return s, lake, assets
}

// oneShapeOpts is the single listing shape these tests warm. Production passes assetListingPrewarmOptions(), whose parity with
// the handler's options is pinned in cmd/stellarindex-api; this package owns that any handed shape ends up cached.
var oneShapeOpts = []timescale.ListAssetsOptions{{
	Limit: 501, Order: timescale.AssetsOrderObservationCountDesc,
}}

// TestPrewarmClassicLakeSupplyFillsTheCacheWithNoRequest pins that the cache holds the complete
// lake reading before anything is served; s.lakeSupply otherwise stays empty until a request
// populates it, which never happens without consumer traffic.
func TestPrewarmClassicLakeSupplyFillsTheCacheWithNoRequest(t *testing.T) {
	t.Parallel()

	rows := []timescale.AssetRow{classicListingRow("CETES")}
	s, lake, _ := lakeSupplyPrewarmServer(t, rows, map[string]string{cetesAsset: cetesTrueSupply})

	s.lakeSupplyMu.Lock()
	cold := len(s.lakeSupply)
	s.lakeSupplyMu.Unlock()
	if cold != 0 {
		t.Fatalf("cache is not cold at construction (%d entries) — this test proves "+
			"nothing unless it starts from the state a freshly booted API is in", cold)
	}

	s.PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)

	s.lakeSupplyMu.Lock()
	entry, cached := s.lakeSupply[cetesAsset]
	s.lakeSupplyMu.Unlock()
	if !cached {
		t.Fatalf("no cache entry for %s after a prewarm pass. The served figure still "+
			"depends on somebody having looked recently: on r1, ~19 h after the last "+
			"request, that meant PYUSD served 3,149,454 against a lake reading of "+
			"11,778,001.", cetesAsset)
	}
	if entry.value != cetesTrueSupply {
		t.Errorf("cached lake reading = %q, want %q (the trustline sum is %s — 36.6%% "+
			"below the true supply, which is the understatement this closes)",
			entry.value, cetesTrueSupply, cetesTrustlineOnly)
	}

	// The contract read must be the deterministically derived SAC, as asset_supply.go resolves
	// it. A mis-derived address returns nothing and degrades silently, indistinguishable from
	// success unless checked.
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	if asked := lake.contractsAsked(); len(asked) != 1 || asked[0] != sac {
		t.Errorf("prewarm asked the lake about %v, want exactly [%s]", asked, sac)
	}
}

// TestPrewarmedCacheServesTheFirstListingWithoutReadingTheLake is the arg-parity guard as behaviour: a prewarm only helps if it
// warms the slot the handler looks up. It prewarms, runs the real listing fill, and requires no further lake read, which only
// holds if the keys coincide.
func TestPrewarmedCacheServesTheFirstListingWithoutReadingTheLake(t *testing.T) {
	t.Parallel()

	rows := []timescale.AssetRow{classicListingRow("CETES")}
	s, lake, assets := lakeSupplyPrewarmServer(t, rows, map[string]string{cetesAsset: cetesTrueSupply})
	s.PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)
	afterPrewarm := len(lake.batchSizes())

	// The population comes from the listing pages the caller named, read
	// through the call the handler makes — not from a set of ids assembled
	// here. Anything else and the warmed population would drift from the
	// pages callers are actually served.
	assets.mu.Lock()
	asked := append([]timescale.ListAssetsOptions(nil), assets.seen...)
	assets.mu.Unlock()
	if !reflect.DeepEqual(asked, oneShapeOpts) {
		t.Errorf("prewarm read listing shapes %+v, want exactly the shapes it was "+
			"handed, %+v", asked, oneShapeOpts)
	}

	details := []AssetDetail{assetDetailFromAssetRow(rows[0])}
	s.fillMarketCapsFromSupply(context.Background(), details, map[string]int{})

	if details[0].CirculatingSupply == nil {
		t.Fatalf("no circulating supply served; row=%+v", details[0])
	}
	if got := *details[0].CirculatingSupply; got != cetesTrueSupply {
		t.Errorf("first listing after a prewarm served %s, want %s — the prewarm warmed "+
			"a slot the handler does not read", got, cetesTrueSupply)
	}
	if got := len(lake.batchSizes()); got != afterPrewarm {
		t.Errorf("the listing fill issued %d further lake read(s) against a prewarmed "+
			"cache. The prewarm and the handler derive the cache key independently; when "+
			"they disagree the prewarm warms a phantom slot and every request still pays "+
			"the cold fill.", got-afterPrewarm)
	}
}

// TestPrewarmClassicLakeSupplyCoversTheWholeListingPageInBoundedBatches: a page covers up to 500 assets but the request path
// warms classicLakeSupplyBatch; one prewarm pass must cover the whole page in bounded batches, since supply_flows sums scale
// with flow count.
func TestPrewarmClassicLakeSupplyCoversTheWholeListingPageInBoundedBatches(t *testing.T) {
	t.Parallel()

	const assets = classicLakeSupplyBatch + 5 // two batches, second one short
	rows := make([]timescale.AssetRow, 0, assets)
	totals := make(map[string]string, assets)
	for i := range assets {
		row := classicListingRow(fmt.Sprintf("TKN%03d", i))
		rows = append(rows, row)
		totals[row.AssetID] = cetesTrueSupply
	}
	s, lake, _ := lakeSupplyPrewarmServer(t, rows, totals)

	s.PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)

	s.lakeSupplyMu.Lock()
	cached := len(s.lakeSupply)
	s.lakeSupplyMu.Unlock()
	if cached != assets {
		t.Errorf("prewarm left %d of %d listed assets uncached. One batch per pass is "+
			"the request path's behaviour and it is why the tail of the listing never "+
			"warms on a quiet service.", assets-cached, assets)
	}
	for i, size := range lake.batchSizes() {
		if size > classicLakeSupplyBatch {
			t.Errorf("lake read %d asked about %d contracts, above the %d cap. The batch "+
				"size is the only handle either caller has on what a supply_flows sum "+
				"costs.", i, size, classicLakeSupplyBatch)
		}
	}
}

// TestPrewarmClassicLakeSupplyStopsOnLakeFailure: a refresh caches nothing on a read error, so the pass must be abandoned rather
// than walk the remaining assets into the same failure, the outage shape classicLakeSupplyRetryGap prevents on the request path.
func TestPrewarmClassicLakeSupplyStopsOnLakeFailure(t *testing.T) {
	t.Parallel()

	const assets = classicLakeSupplyBatch * 3
	rows := make([]timescale.AssetRow, 0, assets)
	for i := range assets {
		rows = append(rows, classicListingRow(fmt.Sprintf("TKN%03d", i)))
	}
	s, lake, _ := lakeSupplyPrewarmServer(t, rows, nil)
	lake.err = errors.New("clickhouse down")

	s.PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)

	if got := len(lake.batchSizes()); got != 1 {
		t.Errorf("prewarm made %d lake reads against a failing lake, want 1 — a pass "+
			"that keeps going turns one slow query into sustained load", got)
	}
	s.lakeSupplyMu.Lock()
	cached := len(s.lakeSupply)
	s.lakeSupplyMu.Unlock()
	if cached != 0 {
		t.Errorf("a failed lake read cached %d entries; it is a fact about the lake "+
			"being unavailable, not about the assets", cached)
	}
}

// TestPrewarmClassicLakeSupplyNeverLowersTheServedFigure: the prewarm must not route around the
// floor guard. An under-seeded lake reading warmed in the background must leave the listing
// serving the trustline sum, as on the request path.
func TestPrewarmClassicLakeSupplyNeverLowersTheServedFigure(t *testing.T) {
	t.Parallel()

	rows := []timescale.AssetRow{classicListingRow("CETES")}
	s, _, _ := lakeSupplyPrewarmServer(t, rows, map[string]string{cetesAsset: "3"})

	s.PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)

	details := []AssetDetail{assetDetailFromAssetRow(rows[0])}
	s.fillMarketCapsFromSupply(context.Background(), details, map[string]int{})

	if details[0].CirculatingSupply == nil {
		t.Fatalf("no circulating supply served; row=%+v", details[0])
	}
	if got := *details[0].CirculatingSupply; got != cetesTrustlineOnly {
		t.Errorf("circulating_supply = %s, want the trustline floor %s — every trustline "+
			"balance was minted, so a lake total below the sum is under-seeding and must "+
			"never lower the served figure", got, cetesTrustlineOnly)
	}
}

// TestPrewarmClassicLakeSupplyDegradesWhenNoReaderIsWired: the prewarm runs on a detached
// goroutine at startup, so any absent seam (test builds, no lake) must not panic it, since an
// unrecovered panic there takes the whole API process down.
func TestPrewarmClassicLakeSupplyDegradesWhenNoReaderIsWired(t *testing.T) {
	t.Parallel()

	// No TokenSupply, no AssetsReader.
	New(Options{Logger: discardLakeLogger()}).
		PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)

	// A TokenSupply reader without the bulk capability — the optional-seam
	// idiom classicLakeSupplyReader relies on.
	New(Options{
		Logger:       discardLakeLogger(),
		TokenSupply:  narrowTokenSupply{},
		AssetsReader: &prewarmListingAssets{rows: []timescale.AssetRow{classicListingRow("CETES")}},
	}).PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)
}

// narrowTokenSupply implements TokenSupplyReader and nothing more, so the
// classicLakeSupplyReader type assertion fails the way it does for every stub
// that predates the bulk read.
type narrowTokenSupply struct{}

func (narrowTokenSupply) TokenSupply(context.Context, string) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, errors.New("not wired")
}

func (narrowTokenSupply) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("not wired")
}

// TestPrewarmClassicLakeSupplyReturnsOnCancellation — it is started detached
// and tracked by the shutdown WaitGroup, so a pass that ignores ctx.Done()
// holds the process open for its whole run.
func TestPrewarmClassicLakeSupplyReturnsOnCancellation(t *testing.T) {
	t.Parallel()

	const assets = classicLakeSupplyBatch * 4
	rows := make([]timescale.AssetRow, 0, assets)
	totals := make(map[string]string, assets)
	for i := range assets {
		row := classicListingRow(fmt.Sprintf("TKN%03d", i))
		rows = append(rows, row)
		totals[row.AssetID] = cetesTrueSupply
	}
	s, _, _ := lakeSupplyPrewarmServer(t, rows, totals)

	// Cancelled before the first batch: the pass must not run the population
	// to completion regardless.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.PrewarmClassicLakeSupply(ctx, oneShapeOpts)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("PrewarmClassicLakeSupply did not return on a cancelled context")
	}
}
