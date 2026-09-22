// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// slowListingAssetsReader wraps stubAssetsReader, sleeping on every
// ListAssetsExt call — the call prewarmLight's /v1/coins warm and
// prewarmAssetListings (via assetListingPrewarmOptions, 12 variants)
// both make under assetsReaderCtx — and recording ctx.Err() at the
// moment GetNativeAssetRow runs.
type slowListingAssetsReader struct {
	stubAssetsReader
	delay        time.Duration
	nativeCtxErr error
}

func (r *slowListingAssetsReader) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	time.Sleep(r.delay)
	return nil, ctx.Err()
}

func (r *slowListingAssetsReader) GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error) {
	r.nativeCtxErr = ctx.Err()
	return timescale.AssetRow{}, nil
}

// TestPrewarmLight_NativeAssetRowSurvivesSlowListingWarm pins T661.
//
// prewarmLight's native/verified-asset prewarm batch must run under its
// OWN fresh deadline, not the assetsReaderCtx whose 20s budget started
// ticking at the /v1/coins warm and keeps ticking through the 13
// ListAssetsExt calls prewarmAssetListings makes (assetListingPrewarmOptions:
// 2 orders x 6 limits, plus the initial /v1/coins call). On a cold
// cache those calls are exactly the kind of work that can burn a
// meaningful chunk of a shared 20s deadline; if GetNativeAssetRow and
// the verified-asset fan-out reuse that same context, a sufficiently
// slow listing warm leaves them with an already-expired one, so every
// call silently no-ops on context deadline exceeded (swallowed at
// Debug) — the cache reports healthy and the next /v1/assets/native or
// verified-asset request still pays the cold read.
func TestPrewarmLight_NativeAssetRowSurvivesSlowListingWarm(t *testing.T) {
	orig := assetsPrewarmBatchTimeout
	assetsPrewarmBatchTimeout = 30 * time.Millisecond
	t.Cleanup(func() { assetsPrewarmBatchTimeout = orig })

	// 13 calls (1 /v1/coins + 12 assetListingPrewarmOptions variants) at
	// 5ms each comfortably exceeds the 30ms budget above, mirroring a
	// slow cold-cache listing warm without a real 20s wait.
	probe := &slowListingAssetsReader{delay: 5 * time.Millisecond}
	assets := v1.NewCachedAssetsReader(probe, 0)
	markets := v1.NewCachedMarketsReader(&recordingMarketsReader{log: newCallLog()}, 0)
	issuers := v1.NewCachedIssuersReader(&stubIssuersReader{}, 0)

	prewarmLight(context.Background(), discardLogger(), markets, assets, issuers, nil, nil, nil)

	if probe.nativeCtxErr != nil {
		t.Fatalf("GetNativeAssetRow saw ctx.Err() = %v — the native asset-catalogue "+
			"prewarm reused a context whose deadline had already lapsed during the "+
			"/v1/coins + /v1/assets listing warm instead of getting a fresh one",
			probe.nativeCtxErr)
	}
}
