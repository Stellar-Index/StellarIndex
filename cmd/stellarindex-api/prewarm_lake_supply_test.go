// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// The lake-flows supply cache (internal/api/v1/classic_lake_supply.go) warms
// itself from the REQUEST path — 32 assets per request — so a service with no
// consumer traffic never converges: entries expire unread and /v1/assets and
// /v1/rwa/assets fall back to the trustline-only sum, which cannot see supply
// held in claimable balances, LP reserves or SAC contract_data. Measured on r1
// 2026-09-12, ~19 h after the last request, PYUSD served 3,149,454 against a
// lake reading of 11,778,001 and XRF 21,895,149 against 118,333,629.
//
// The behaviour of the fill lives with the fill (see
// classic_lake_supply_prewarm_test.go). What THIS package owns is the pair of
// wiring properties a prewarm dies of:
//
//  1. It runs at boot rather than one cadence later. That is the same defect
//     TestPrewarmCaches_FiresBothPassesBeforeTheFirstTick pins for the other
//     loop, and it recurs on every deploy.
//  2. It asks about the population the listing actually serves. The prewarm
//     and the handler compute their arguments independently, and three
//     production bugs in this file came from those two drifting.

// bulkLakeStub is the token-supply reader with the bulk capability
// classicLakeSupplyReader type-asserts for. Production:
// *clickhouse.SupplyReader via clickhouse.NewSupplyReaderAuth.
type bulkLakeStub struct{}

func (bulkLakeStub) TokenSupply(context.Context, string) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, errors.New("not used by this test")
}

func (bulkLakeStub) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("not used by this test")
}

func (bulkLakeStub) TokenSupplyForContracts(
	context.Context, []string,
) (map[string]clickhouse.TokenSupply, error) {
	return map[string]clickhouse.TokenSupply{}, nil
}

// listingShapeRecorder answers the listing read the lake prewarm uses to
// discover its population, recording the exact options tuple. It serves no
// rows: this test is about the arguments, not the fill.
type listingShapeRecorder struct {
	stubAssetsReader
	ch chan timescale.ListAssetsOptions
}

func (l *listingShapeRecorder) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	select {
	case l.ch <- opts:
	default: // a slow reader must not wedge the prewarm goroutine
	}
	return nil, nil
}

// TestPrewarmClassicLakeSupply_AsksForTheShapesTheListingPrewarmWarms is the
// drift guard, and it deliberately does NOT restate the shape set. It runs the
// real loop against a real v1.Server built by the production constructor
// (v1.New — the same call cmd/stellarindex-api/main.go makes at run()'s
// AssetsReader/TokenSupply wiring) and compares what the loop asked the
// listing for against assetListingPrewarmOptions() itself.
//
// That is the whole point: the lake cache is per-ASSET, so the only way to
// know which assets to warm is to ask which assets the warmed listing pages
// serve. A hand-maintained id list here, or a second copy of the limit
// arithmetic, is precisely how prewarmLight's MarketsOrderPair, /v1/pools
// filter and ?limit=50 phantom slots happened — each warmed a slot nothing
// looked up, logged success, and left every request paying the cold fill.
func TestPrewarmClassicLakeSupply_AsksForTheShapesTheListingPrewarmWarms(t *testing.T) {
	t.Parallel()

	want := assetListingPrewarmOptions()
	recorder := &listingShapeRecorder{ch: make(chan timescale.ListAssetsOptions, 4*len(want))}
	srv := v1.New(v1.Options{
		Logger:       discardLogger(),
		AssetsReader: recorder,
		TokenSupply:  bulkLakeStub{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		prewarmClassicLakeSupply(ctx, srv)
	}()

	// The first sweep must complete well inside classicLakeSupplySweepGap;
	// a loop that waits for its first sleep leaves the API publishing the
	// understated figure for that whole window after every deploy.
	got := make([]timescale.ListAssetsOptions, 0, len(want))
	deadline := time.After(20 * time.Second)
	for len(got) < len(want) {
		select {
		case opts := <-recorder.ch:
			got = append(got, opts)
		case <-deadline:
			t.Fatalf("the lake-supply prewarm read %d of the %d listing shapes within 20s "+
				"(sweep gap is %s). A sweep that fires only after its first sleep leaves "+
				"every deploy serving trustline-only supply until it does.\n  read: %+v",
				len(got), len(want), classicLakeSupplySweepGap, got)
		}
	}

	sortOpts := func(o []timescale.ListAssetsOptions) {
		sort.Slice(o, func(i, j int) bool {
			if o[i].Order != o[j].Order {
				return o[i].Order < o[j].Order
			}
			return o[i].Limit < o[j].Limit
		})
	}
	sortOpts(got)
	sortOpts(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the lake-supply prewarm asked the listing for shapes that are not the "+
			"ones prewarmAssetListings keeps warm.\n  prewarm read : %+v\n  warmed slots : %+v\n\n"+
			"The two must be the same set or the lake cache is warmed for assets no page "+
			"serves while the assets every page serves stay cold.", got, want)
	}

	// And it must return on cancellation rather than leak: run() tracks it on
	// the shutdown WaitGroup, so a loop that ignores ctx.Done() hangs the exit.
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("prewarmClassicLakeSupply did not return within 20s of context cancellation")
	}
}

// TestPrewarmClassicLakeSupply_NilServerIsSafe — the loop runs on a detached
// goroutine started before anything has proven its dependencies exist, and an
// unrecovered panic there takes the whole API process down.
func TestPrewarmClassicLakeSupply_NilServerIsSafe(t *testing.T) {
	t.Parallel()
	prewarmClassicLakeSupply(context.Background(), nil)
}

// TestPrewarmClassicLakeSupply_IsActuallyStarted closes the gap every other
// test in this file leaves open: they all call the loop themselves, so all of
// them stay green if run() never starts it. A warmer that exists, is tested,
// and is never spawned is indistinguishable from no warmer at all — and it is
// worse, because the tests report otherwise.
//
// Derived from the source rather than from a boot, for the reason
// TestBackgroundWorkersRecover gives: run() needs a database, a Redis and a
// ClickHouse before it reaches this line.
func TestPrewarmClassicLakeSupply_IsActuallyStarted(t *testing.T) {
	t.Parallel()

	sites, err := guardscan.ScanFile("main.go", guardscan.Config{
		Guards: []string{"recoverBackgroundWorker"},
	})
	if err != nil {
		t.Fatalf("scan main.go: %v", err)
	}
	started := 0
	for _, s := range sites {
		if s.Calls("prewarmClassicLakeSupply") {
			started++
		}
	}
	if started != 1 {
		t.Errorf("main.go starts %d detached goroutines calling prewarmClassicLakeSupply, "+
			"want exactly 1. Without one, /v1/assets and /v1/rwa/assets keep serving the "+
			"trustline-only supply whenever nobody has looked recently; with more than "+
			"one, two sweeps race for the same single-flight slot.", started)
	}
}
