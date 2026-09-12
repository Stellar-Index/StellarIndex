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

// The measured defect this file guards (r1, 2026-09-12).
//
// classic_lake_supply.go warms itself from the REQUEST path only: a listing
// request kicks a detached refresh for classicLakeSupplyBatch of the assets it
// asked about and serves the trustline sum for the rest. Under sustained
// traffic that converges, and the served figures match Horizon's all-component
// totals to within 0.012%. r1 carries no consumer traffic, so it never
// converges there — entries expire unread at classicLakeSupplyTTL and both
// /v1/assets and /v1/rwa/assets fall back to the trustline-only sum, which is
// blind to claimable-balance, LP-reserve and SAC-held supply by construction.
// Measured ~19 h after the last request:
//
//	PYUSD  served  3,149,454   lake  11,778,001   (73% understated)
//	XRF    served 21,895,149   lake 118,333,629   (82% understated)
//
// Nothing about the readings or the preference chain was wrong; the warming
// was. So every test here asks the same question in a different way: is the
// cache filled WITHOUT anyone having made a request?
//
// The CETES shape from classic_lake_supply_test.go is reused for the numbers
// (a trustline sum of 100.0000000 against a true supply of 136.6000000, a
// 36.6% uplift sitting where a trustline query structurally cannot look).

// discardLakeLogger keeps the prewarm's debug lines out of the test output.
func discardLakeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// prewarmLakeStub answers the bulk lake-flows read and records each read as a
// BATCH rather than a flat list, so a test can assert the background driver
// respects classicLakeSupplyBatch instead of asking for the whole population
// in one query.
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

// lakeSupplyPrewarmServer builds a Server through the PRODUCTION constructor
// ([New]) with the same three seams cmd/stellarindex-api/main.go wires:
// TokenSupply (production: *clickhouse.SupplyReader via NewSupplyReaderAuth),
// AssetsReader (production: *v1.CachedAssetsReader over *timescale.Store) and
// Explorer (production: *clickhouse.ExplorerReader, here the trustline-only
// sum it really is).
//
// Constructed rather than struct-literalled deliberately. A prewarm that works
// against a hand-assembled Server and not against New(Options{…}) is the
// green-suite-dangerous-branch shape this project has been bitten by: the
// wiring IS the thing under test here.
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

// oneShapeOpts is the single listing shape these tests warm. Production passes
// assetListingPrewarmOptions() (cmd/stellarindex-api), whose byte-identity
// with the handler's own options is pinned by prewarm_parity_test.go there;
// what THIS package owns is that whatever shapes it is handed, the assets
// those pages serve end up cached.
var oneShapeOpts = []timescale.ListAssetsOptions{{
	Limit: 501, Order: timescale.AssetsOrderObservationCountDesc,
}}

// TestPrewarmClassicLakeSupplyFillsTheCacheWithNoRequest is the regression for
// the measured defect: the cache must hold the complete lake reading with
// nothing having been served.
//
// Red before the prewarm existed — s.lakeSupply stays empty until a request
// populates it, which on a service with no consumer traffic is never.
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

	// The contract read must be the deterministically derived SAC — the same
	// resolution asset_supply.go performs for GET /v1/assets/{id}/supply. A
	// mis-derived address returns nothing and degrades silently to today's
	// behaviour, which is exactly the failure this test cannot distinguish
	// from success unless it checks.
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	if asked := lake.contractsAsked(); len(asked) != 1 || asked[0] != sac {
		t.Errorf("prewarm asked the lake about %v, want exactly [%s]", asked, sac)
	}
}

// TestPrewarmedCacheServesTheFirstListingWithoutReadingTheLake is the
// arg-parity guard, expressed as behaviour rather than as restated arithmetic.
//
// A prewarm only helps if it warms the slot the HANDLER looks up. Three
// production bugs in this codebase came from a prewarm computing its key
// independently and drifting (Order, Sources, Limit) — each warmed a phantom
// slot, logged success, and left every real request paying the cold fill. A
// test that recomputed the expected key here would agree with the prewarm and
// be wrong in the same direction, so instead: prewarm, then run the real
// listing fill, and require that it needed NO further lake read. That can only
// hold if the keys coincide.
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

// TestPrewarmClassicLakeSupplyCoversTheWholeListingPageInBoundedBatches — a
// listing page asks about up to 500 assets and the request path only ever
// warms classicLakeSupplyBatch of them per request, which is why the tail
// never warms without traffic. One prewarm pass must cover the whole page,
// and it must still do it a bounded batch at a time: the cost of a
// supply_flows sum scales with the flow count of the contracts in it, and a
// mature token carries millions of rows.
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

// TestPrewarmClassicLakeSupplyStopsOnLakeFailure — a refresh caches an entry
// for every asset in its batch on success and nothing at all on a read error,
// so a failing lake is a pass that makes no progress. It must abandon the pass
// rather than walk the rest of the population into the same failure: retrying
// a failing supply_flows read per unit of work is the outage shape
// classicLakeSupplyRetryGap exists to prevent on the request path.
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

// TestPrewarmClassicLakeSupplyNeverLowersTheServedFigure — the floor guard is
// load-bearing and the prewarm must not route around it. An under-seeded lake
// reading warmed in the background has to leave the listing serving the
// trustline sum, exactly as an under-seeded reading fetched on the request
// path does.
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

// TestPrewarmClassicLakeSupplyDegradesWhenNoReaderIsWired — the prewarm runs
// on a detached goroutine at startup, before anything has proven the readers
// exist. Every seam it needs may be absent (test builds, a deployment without
// a lake) and none of them may panic that goroutine: an unrecovered panic
// there takes the whole API process with it.
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
