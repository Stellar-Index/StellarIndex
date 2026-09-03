// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Stub upstreams for the prewarm arg-parity test. They return empty
// results and, where the parity assertion needs it, record the argument
// tuple. Nothing here is under test — see prewarm_parity_test.go for
// why the recorder is a probe on the arguments rather than a mock whose
// behaviour matters.

// ─── AssetsReader ────────────────────────────────────────────────

type stubAssetsReader struct{ log *callLog }

func (s *stubAssetsReader) record(sig string) {
	if s.log != nil {
		s.log.add(sig)
	}
}

func (s *stubAssetsReader) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	// Mirrors the cache key's dimensions: the listing slot is selected
	// by (Order, Limit, Cursor, Issuer, Code, Q) — the same tuple the
	// /v1/assets and /v1/coins handlers each compute differently.
	s.record(fmt.Sprintf("ListAssetsExt(order=%d limit=%d cursor=%q issuer=%q code=%q q=%q)",
		int(opts.Order), opts.Limit, opts.Cursor, opts.Issuer, opts.Code, opts.Q))
	return nil, nil
}

func (s *stubAssetsReader) GetAssetBySlug(context.Context, string) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, timescale.ErrNotFound
}

func (s *stubAssetsReader) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	s.record("GetAssetByAssetID(" + assetID + ")")
	return timescale.AssetRow{}, timescale.ErrNotFound
}

func (s *stubAssetsReader) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	s.record("GetNativeAssetRow()")
	return timescale.AssetRow{}, timescale.ErrNotFound
}

func (s *stubAssetsReader) GetAssetTopMarkets(context.Context, string, int) ([]timescale.AssetTopMarket, error) {
	return nil, nil
}

func (s *stubAssetsReader) GetAssetPriceHistory24h(context.Context, string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *stubAssetsReader) GetAssetPriceHistory7d(context.Context, string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (s *stubAssetsReader) GetAssetsPriceHistory24hBatch(context.Context, []string) (map[string][]timescale.AssetPricePoint, error) {
	return map[string][]timescale.AssetPricePoint{}, nil
}

func (s *stubAssetsReader) GetAssetsPriceHistory7dBatch(context.Context, []string) (map[string][]timescale.AssetPricePoint, error) {
	return map[string][]timescale.AssetPricePoint{}, nil
}

func (s *stubAssetsReader) GetAssetMarketsCount(context.Context, string) (int64, error) {
	return 0, nil
}

func (s *stubAssetsReader) GetAssetATH(context.Context, string) (*timescale.AssetATH, error) {
	return nil, nil
}

func (s *stubAssetsReader) GetAssetsATHBatch(context.Context, []string) (map[string]timescale.AssetATH, error) {
	return map[string]timescale.AssetATH{}, nil
}

func (s *stubAssetsReader) GetAssetTradeCount24h(context.Context, string) (int64, error) {
	return 0, nil
}

// ─── IssuersReader ───────────────────────────────────────────────

type stubIssuersReader struct{ log *callLog }

func (s *stubIssuersReader) GetIssuer(context.Context, string) (timescale.IssuerRow, error) {
	return timescale.IssuerRow{}, timescale.ErrNotFound
}

func (s *stubIssuersReader) ListIssuerAssets(context.Context, string) ([]timescale.IssuerAsset, error) {
	return nil, nil
}

func (s *stubIssuersReader) ListIssuers(_ context.Context, limit int) ([]timescale.IssuerSummary, error) {
	if s.log != nil {
		s.log.add(fmt.Sprintf("ListIssuers(limit=%d)", limit))
	}
	return nil, nil
}

// ─── SourcesStatsReader ──────────────────────────────────────────

type stubSourcesStatsReader struct{ log *callLog }

func (s *stubSourcesStatsReader) record(sig string) {
	if s.log != nil {
		s.log.add(sig)
	}
}

func (s *stubSourcesStatsReader) GetSourceStats(context.Context) ([]timescale.SourceStats, error) {
	s.record("GetSourceStats()")
	return nil, nil
}

func (s *stubSourcesStatsReader) GetSourceVolumeHistory24h(context.Context) ([]timescale.SourceVolumeBucket, error) {
	s.record("GetSourceVolumeHistory24h()")
	return nil, nil
}

func (s *stubSourcesStatsReader) GetSourceVolumeHistory7d(context.Context) ([]timescale.SourceVolumeBucket, error) {
	s.record("GetSourceVolumeHistory7d()")
	return nil, nil
}
