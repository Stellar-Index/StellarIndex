// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// orderLog records call names in the order they happen across both the
// assets and markets fakes below. callLog (prewarm_parity_test.go) only
// counts occurrences per signature, which cannot express "before/after";
// this can.
type orderLog struct {
	mu    sync.Mutex
	names []string
}

func (o *orderLog) add(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.names = append(o.names, name)
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.names...)
}

// orderedAssetsReader wraps stubAssetsReader, additionally recording the
// native/verified-asset calls T653 is about into a shared, cross-reader
// order log.
type orderedAssetsReader struct {
	stubAssetsReader
	order *orderLog
}

func (r *orderedAssetsReader) GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error) {
	r.order.add("GetNativeAssetRow")
	return r.stubAssetsReader.GetNativeAssetRow(ctx)
}

func (r *orderedAssetsReader) GetAssetByAssetID(ctx context.Context, assetID string) (timescale.AssetRow, error) {
	r.order.add("GetAssetByAssetID:" + assetID)
	return r.stubAssetsReader.GetAssetByAssetID(ctx, assetID)
}

// orderedMarketsReader wraps recordingMarketsReader, recording the
// markets/pools calls prewarmLight makes against the SEPARATE 5-minute
// mkCtx budget (as opposed to assetsReaderCtx's 20s one).
type orderedMarketsReader struct {
	recordingMarketsReader
	order *orderLog
}

func (r *orderedMarketsReader) DistinctPairsExt(ctx context.Context, cursor string, limit int, ord timescale.MarketsOrder) ([]v1.Market, string, error) {
	r.order.add("DistinctPairsExt")
	return r.recordingMarketsReader.DistinctPairsExt(ctx, cursor, limit, ord)
}

func (r *orderedMarketsReader) AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, ord timescale.MarketsOrder) ([]v1.Pool, string, error) {
	r.order.add("AllPools")
	return r.recordingMarketsReader.AllPools(ctx, filter, cursor, limit, ord)
}

func (r *orderedMarketsReader) SourceMarkets(ctx context.Context, source, cursor string, limit int, ord timescale.MarketsOrder) ([]v1.Market, string, error) {
	r.order.add("SourceMarkets")
	return r.recordingMarketsReader.SourceMarkets(ctx, source, cursor, limit, ord)
}

// TestPrewarmLight_NativeAndVerifiedAssetWarmsRunBeforeMarketsLoops is
// the regression guard for T653.
//
// assetsReaderCtx carries a 20s budget, wholly separate from the
// markets/pools/per-DEX/per-CEX work's 5-minute mkCtx. If the
// native-asset and verified-asset-detail prewarm calls run AFTER that
// ~95-line block of markets-reader calls (as they did pre-fix), most or
// all of the 20s budget is gone by the time they run on a cold cache —
// so on a real cold start they lose the race against assetsReaderCtx's
// deadline. This asserts they instead run before any markets-reader
// call fires, alongside the other assetsReaderCtx-scoped work.
func TestPrewarmLight_NativeAndVerifiedAssetWarmsRunBeforeMarketsLoops(t *testing.T) {
	order := &orderLog{}

	assets := v1.NewCachedAssetsReader(&orderedAssetsReader{order: order}, 0)
	markets := v1.NewCachedMarketsReader(&orderedMarketsReader{
		recordingMarketsReader: recordingMarketsReader{log: newCallLog()},
		order:                  order,
	}, 0)
	issuers := v1.NewCachedIssuersReader(&stubIssuersReader{}, 0)

	prewarmLight(context.Background(), discardLogger(), markets, assets, issuers,
		[]string{"USDC-GDHUXCJQVGYUYVYEPCTAZ7WMHNMTZJWKUANE2LFXTYUZ3YPDN2PDM26"}, nil)

	names := order.snapshot()

	nativeIdx := -1
	firstMarketsIdx := -1
	for i, n := range names {
		if n == "GetNativeAssetRow" && nativeIdx == -1 {
			nativeIdx = i
		}
		if firstMarketsIdx == -1 && (n == "DistinctPairsExt" || n == "AllPools" || n == "SourceMarkets") {
			firstMarketsIdx = i
		}
	}
	if nativeIdx == -1 {
		t.Fatal("prewarmLight never called GetNativeAssetRow — test premise broken")
	}
	if firstMarketsIdx == -1 {
		t.Fatal("prewarmLight never called a markets-reader method — test premise broken")
	}
	if nativeIdx > firstMarketsIdx {
		t.Errorf("GetNativeAssetRow ran at position %d, after the first markets-reader call "+
			"(%s) at position %d. On a cold cache the markets/pools loops run against a "+
			"separate 5-minute mkCtx and can consume assetsReaderCtx's whole 20s budget by "+
			"elapsed wall-clock time alone, so native/verified-asset prewarm must run BEFORE "+
			"them, not after.\nfull call order: %v", nativeIdx, names[firstMarketsIdx], firstMarketsIdx, names)
	}
}
