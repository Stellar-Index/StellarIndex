// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Arg-parity between prewarmCaches and the handlers (#340 item 3).
//
// The prewarm loop and the request handlers are two independent callers
// of the SAME cached readers. A cache slot is selected by the exact
// argument tuple — [v1.CachedMarketsReader.AllPools] keys on
// (Sources, Base, Quote, Asset, cursor, limit, order), sorting Sources
// (cachekey.go strSet) — so the prewarm only warms a handler's slot if
// it passes byte-identical arguments.
//
// When they drift, NOTHING fails. The cache reports healthy, the
// prewarm logs success, and every user request still pays the full cold
// read against the trades hypertable. It is a phantom slot: it costs a
// query per cycle and warms a key nothing looks up. Three separate
// production incidents were this exact shape, all recorded in
// prewarmLight's own comments:
//
//   - ORDER. /v1/pools defaults to MarketsOrderVolume24hDesc; the
//     prewarm passed MarketsOrderPair. Live on 2026-05-09:
//     /v1/pools?source=sdex 27s, soroswap 16s, phoenix 12s, comet 11s.
//   - SOURCES. The unfiltered /v1/pools handler builds
//     `PoolsFilter{Sources: DexSourceNames()}`, not `Sources: nil`. The
//     prewarm passed the zero filter, whose key fragment is `[]` rather
//     than `[aquarius comet phoenix sdex soroswap]`.
//   - LIMIT. /v1/markets?source=… is fired by the explorer at
//     limit=200; the prewarm covered only the unfiltered pair list, so
//     every /exchanges/{name} visit paid the 8s ceiling (R-002).
//
// A test that re-states the prewarm's own constants cannot catch any of
// these — it would agree with the prewarm and be wrong in the same
// direction. So this test derives the expected set from the REAL
// HANDLERS: it drives actual HTTP requests through v1.Server and asserts
// every reader call they make was also made by prewarmLight.

// ─── recording readers ───────────────────────────────────────────

// callLog records normalised reader-call signatures. The normalisation
// mirrors the cache key exactly (sorted Sources), so two entries are
// equal iff they select the same cache slot.
type callLog struct {
	mu    sync.Mutex
	calls map[string]int
}

func newCallLog() *callLog { return &callLog{calls: map[string]int{}} }

func (l *callLog) add(sig string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls[sig]++
}

func (l *callLog) has(sig string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[sig] > 0
}

func (l *callLog) signatures() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.calls))
	for k := range l.calls {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// poolsSig renders an AllPools call the way the cache key does.
func poolsSig(filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) string {
	sources := append([]string(nil), filter.Sources...)
	sort.Strings(sources)
	return fmt.Sprintf("AllPools(sources=[%s] base=%q quote=%q asset=%q cursor=%q limit=%d order=%d)",
		strings.Join(sources, " "), filter.Base, filter.Quote, filter.Asset, cursor, limit, int(order))
}

func pairsSig(cursor string, limit int, order timescale.MarketsOrder) string {
	return fmt.Sprintf("DistinctPairsExt(cursor=%q limit=%d order=%d)", cursor, limit, int(order))
}

func sourceMarketsSig(source, cursor string, limit int, order timescale.MarketsOrder) string {
	return fmt.Sprintf("SourceMarkets(source=%q cursor=%q limit=%d order=%d)", source, cursor, limit, int(order))
}

// recordingMarketsReader is a v1.MarketsReader that records every call
// and returns empty results. It is NOT a mock whose behaviour is under
// test — it is a probe on the argument tuple, which is the only thing
// the cache key is built from.
type recordingMarketsReader struct{ log *callLog }

func (r *recordingMarketsReader) DistinctPairsExt(_ context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	r.log.add(pairsSig(cursor, limit, order))
	return nil, "", nil
}

func (r *recordingMarketsReader) SourceMarkets(_ context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	r.log.add(sourceMarketsSig(source, cursor, limit, order))
	return nil, "", nil
}

func (r *recordingMarketsReader) AssetMarkets(_ context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	r.log.add(fmt.Sprintf("AssetMarkets(asset=%q cursor=%q limit=%d order=%d)", asset, cursor, limit, int(order)))
	return nil, "", nil
}

func (r *recordingMarketsReader) AllPools(_ context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Pool, string, error) {
	r.log.add(poolsSig(filter, cursor, limit, order))
	return nil, "", nil
}

func (r *recordingMarketsReader) PairMarket(context.Context, canonical.Asset, canonical.Asset) (v1.Market, bool, error) {
	return v1.Market{}, false, nil
}

func (r *recordingMarketsReader) FirstTradeBatch(context.Context, [][2]string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func (r *recordingMarketsReader) GetPairsVolumeHistory24hBatch(context.Context, [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return map[string][]timescale.PairVolumePoint{}, nil
}

// newRecordingMarkets returns a cached reader over a recorder. TTL is 0
// so every call passes straight through: the log then holds the
// complete call sequence rather than one entry per cache slot, which is
// what makes a MISSING prewarm visible instead of merely deduplicated.
func newRecordingMarkets() (*v1.CachedMarketsReader, *callLog) {
	log := newCallLog()
	return v1.NewCachedMarketsReader(&recordingMarketsReader{log: log}, 0), log
}

// ─── the parity assertion ────────────────────────────────────────

// hotRequests are the request shapes production actually serves, taken
// from the incident notes in prewarmLight and from the explorer's own
// fetches. Each one must land on a slot the prewarm warmed.
//
// Adding a shape here that the prewarm does not cover is a legitimate
// way to fail this test: it means the explorer fires a request nothing
// warms, which is the R-001 / R-002 class.
var hotRequests = []struct {
	name string
	path string
	// why records the incident or caller this shape comes from.
	why string
}{
	{
		name: "pools-unfiltered-default",
		path: "/v1/pools?limit=100",
		why:  "explorer /pools landing; the Sources-drift bug (PoolsFilter{} vs DexSourceNames())",
	},
	{
		name: "pools-per-dex",
		path: "/v1/pools?source=soroswap&limit=100",
		why:  "explorer /dexes/{source}; the order-drift bug (27s live on 2026-05-09)",
	},
	{
		name: "markets-default-order",
		path: "/v1/markets?limit=100",
		why:  "OpenAPI default limit; /v1/markets defaults to volume-desc since 2026-05-10",
	},
	{
		name: "markets-explorer-order",
		path: "/v1/markets?limit=200&order_by=volume_24h_usd_desc",
		why:  "home page, HomeTopMarkets, sitemap and embed/pair routes all pass this",
	},
	{
		name: "markets-alphabetical",
		path: "/v1/markets?limit=100&order_by=pair",
		why:  "stable-keyset order a full-catalogue walker passes explicitly",
	},
	{
		name: "markets-per-cex",
		path: "/v1/markets?source=binance&limit=200",
		why:  "explorer /exchanges/{name} PairsTable.tsx (R-002: 8s ceiling per cold visit)",
	},
}

// TestPrewarmLight_WarmsEveryKeyTheHotHandlersLookUp is the drift guard.
//
// It runs the two callers of CachedMarketsReader against separate
// recorders and asserts containment: every argument tuple a hot handler
// produces must also have been produced by prewarmLight. The expected
// set is never written down — it is READ OFF THE HANDLERS — so the test
// cannot agree with a prewarm that has drifted.
func TestPrewarmLight_WarmsEveryKeyTheHotHandlersLookUp(t *testing.T) {
	t.Parallel()

	// 1. What the prewarm warms.
	warmed := runPrewarmLightForParity(t)

	// 2. What the handlers ask for, one sub-test per shape so a
	//    failure names the exact route that went cold.
	for _, hr := range hotRequests {
		t.Run(hr.name, func(t *testing.T) {
			t.Parallel()
			asked := handlerCallsForParity(t, hr.path)
			if len(asked) == 0 {
				t.Fatalf("GET %s made no markets-reader call — the test's premise is broken "+
					"(a handler short-circuit, or the route moved)", hr.path)
			}
			for _, sig := range asked {
				if warmed.has(sig) {
					continue
				}
				t.Errorf("GET %s looks up a cache slot the prewarm never warms.\n"+
					"  handler asks for: %s\n"+
					"  source of shape : %s\n"+
					"  prewarm warms   :\n    %s\n\n"+
					"This is the phantom-slot class: the cache reports healthy, the prewarm "+
					"logs success, and every request to this route still pays the full cold "+
					"scan. Fix the PREWARM to mirror the handler — do not relax this test.",
					hr.path, sig, hr.why, strings.Join(warmed.signatures(), "\n    "))
			}
		})
	}
}

// runPrewarmLightForParity runs one prewarmLight pass and returns the
// markets-reader calls it made.
func runPrewarmLightForParity(t *testing.T) *callLog {
	t.Helper()

	markets, log := newRecordingMarkets()
	assets := v1.NewCachedAssetsReader(&stubAssetsReader{}, 0)
	issuers := v1.NewCachedIssuersReader(&stubIssuersReader{}, 0)

	prewarmLight(context.Background(), discardLogger(), markets, assets, issuers, nil, nil)
	return log
}

// handlerCallsForParity fires one real request through a real v1.Server
// and returns the markets-reader calls the handler made.
func handlerCallsForParity(t *testing.T, path string) []string {
	t.Helper()

	markets, log := newRecordingMarkets()
	srv := v1.New(v1.Options{Logger: discardLogger(), Markets: markets})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d, want 200 — the shape under test must be a request the "+
			"API actually serves", path, resp.StatusCode)
	}
	return log.signatures()
}

// TestPrewarmLight_AssetListingWarmsAreHandlerReachable is the assets
// half of the same parity property, derived from the REAL handler
// rather than from the prewarm's own arithmetic.
//
// prewarm_assets_test.go already pins the `+1` and the limit set, by
// recomputing them. That catches a change to one side of the
// arithmetic, but it cannot catch a change to the HANDLER — if
// handleAssetListFromAssets stopped overfetching, or parseAssetsOrder's
// default moved off AssetsOrderObservationCountDesc, the prewarm and
// that test would still agree with each other and both be wrong. This
// test asks the handler instead.
func TestPrewarmLight_AssetListingWarmsAreHandlerReachable(t *testing.T) {
	t.Parallel()

	// The two orders the prewarm covers, and the query parameter that
	// selects each. "" is `order_by` absent, which parseAssetsOrder maps
	// to AssetsOrderObservationCountDesc.
	orderParam := map[timescale.AssetsOrder]string{
		timescale.AssetsOrderObservationCountDesc: "",
		timescale.AssetsOrderVolume24hUSDDesc:     "volume_24h_usd_desc",
	}

	warmed := map[string]bool{}
	for _, opts := range assetListingPrewarmOptions() {
		warmed[listAssetsSig(opts)] = true
	}

	for _, userLimit := range assetListingPrewarmLimits {
		for order, param := range orderParam {
			path := fmt.Sprintf("/v1/assets?limit=%d", userLimit)
			if param != "" {
				path += "&order_by=" + param
			}
			t.Run(fmt.Sprintf("limit=%d/order=%d", userLimit, int(order)), func(t *testing.T) {
				t.Parallel()

				log := newCallLog()
				assets := v1.NewCachedAssetsReader(&stubAssetsReader{log: log}, 0)
				srv := v1.New(v1.Options{Logger: discardLogger(), AssetsReader: assets})
				ts := httptest.NewServer(srv.Handler())
				t.Cleanup(ts.Close)

				resp, err := http.Get(ts.URL + path)
				if err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
				_ = resp.Body.Close()

				asked := listAssetsSigs(log)
				if len(asked) == 0 {
					t.Fatalf("GET %s made no ListAssetsExt call; calls were %v", path, log.signatures())
				}
				for _, sig := range asked {
					if warmed[sig] {
						continue
					}
					t.Errorf("GET %s looks up %s, which assetListingPrewarmOptions() does not warm.\n"+
						"warmed set:\n    %s\n\n"+
						"The handler and the prewarm compute this tuple independently; when they "+
						"disagree the cache reports healthy and every request still pays the cold "+
						"listAssetsBaseSelect scan. Fix the PREWARM, not this test.",
						path, sig, strings.Join(sortedKeys(warmed), "\n    "))
				}
			})
		}
	}
}

// listAssetsSig renders a ListAssetsOptions the way stubAssetsReader
// records it, so the warmed set and the observed calls are comparable.
func listAssetsSig(o timescale.ListAssetsOptions) string {
	return fmt.Sprintf("ListAssetsExt(order=%d limit=%d cursor=%q issuer=%q code=%q q=%q)",
		int(o.Order), o.Limit, o.Cursor, o.Issuer, o.Code, o.Q)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestPrewarmLight_StandaloneListingWarmIsTheLimit198Slot documents what
// prewarmLight's ONE hardcoded ListAssetsExt call actually warms.
//
// The call is `ListAssetsOptions{Limit: 199}` with every other field at
// its zero value, so Order is AssetsOrderObservationCountDesc. Its
// comment explained 199 as "/v1/coins?limit=200, minus the row
// prependNative splices in". BOTH of those are gone: the /v1/coins route
// was removed in rc.48 (see the AssetsReader field comment in
// internal/api/v1/server.go) and `prependNative` no longer exists
// anywhere in the tree. The slot the call warms TODAY is
// /v1/assets?limit=198 with no order_by — a limit that is not in
// assetListingPrewarmLimits and that no known caller sends.
//
// This test states that correspondence so the constant is not
// mysterious, and so whoever next touches it can see what it buys. It
// deliberately does NOT assert that warming 198 is worthwhile: that is a
// product call, recorded in #340, not an invariant.
func TestPrewarmLight_StandaloneListingWarmIsTheLimit198Slot(t *testing.T) {
	t.Parallel()

	log := newCallLog()
	assets := v1.NewCachedAssetsReader(&stubAssetsReader{log: log}, 0)
	srv := v1.New(v1.Options{Logger: discardLogger(), AssetsReader: assets})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/assets?limit=198")
	if err != nil {
		t.Fatalf("GET /v1/assets?limit=198: %v", err)
	}
	_ = resp.Body.Close()

	want := listAssetsSig(timescale.ListAssetsOptions{Limit: 199})
	found := false
	asked := listAssetsSigs(log)
	for _, sig := range asked {
		if sig == want {
			found = true
		}
	}
	if !found {
		t.Errorf("/v1/assets?limit=198 asks for %v, but prewarmLight's hardcoded warm is %s.\n"+
			"The standalone 199 warm would then correspond to no request at all — it was "+
			"already orphaned once, by the rc.48 removal of /v1/coins. If this fails, delete "+
			"the call rather than re-guessing a constant.", asked, want)
	}
}

func listAssetsSigs(l *callLog) []string {
	var out []string
	for _, sig := range l.signatures() {
		if strings.HasPrefix(sig, "ListAssetsExt(") {
			out = append(out, sig)
		}
	}
	return out
}
