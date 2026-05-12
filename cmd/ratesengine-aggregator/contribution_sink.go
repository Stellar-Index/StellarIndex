package main

import (
	"context"

	"github.com/RatesEngine/rates-engine/internal/aggregate/orchestrator"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// contributionSink adapts the timescale Store to the
// orchestrator.ContributionSink interface. Lives in the binary
// rather than the storage package to avoid an import cycle
// (storage already imports nothing from aggregate; the orchestrator
// imports storage for triangulate, so a storage→orchestrator import
// would close the loop).
//
// Translates each [orchestrator.ContributionRecord] into a batch
// of [timescale.PriceSourceContribution] rows and forwards to the
// store.
type contributionSink struct {
	store *timescale.Store
}

func newContributionSink(s *timescale.Store) *contributionSink {
	return &contributionSink{store: s}
}

func (s *contributionSink) RecordContributions(ctx context.Context, rec orchestrator.ContributionRecord) error {
	if len(rec.Contributions) == 0 {
		return nil
	}
	// F-1242 (codex audit-2026-05-12): persist per-source USD
	// volume when the orchestrator's pre-rewrite usdVolumePerSource
	// map carries it. Pre-fix the row stored Weight + TradeCount
	// only; volume_usd was schema-reserved but never populated, so
	// the source-breakdown donut on the explorer couldn't render
	// dollar tooltips without recomputing from raw trades. The
	// total-USD-volume for the window is split per-source by
	// each contribution's Weight (proportional to that source's
	// quote-volume share — the natural decomposition).
	rows := make([]timescale.PriceSourceContribution, 0, len(rec.Contributions))
	for _, c := range rec.Contributions {
		row := timescale.PriceSourceContribution{
			AssetID:    rec.Pair.Base.String(),
			QuoteID:    rec.Pair.Quote.String(),
			Bucket:     rec.ComputedAt,
			Source:     c.Source,
			Weight:     c.Weight,
			TradeCount: c.TradeCount,
		}
		if rec.USDVolumeTotal > 0 {
			volumeUSD := rec.USDVolumeTotal * c.Weight
			row.VolumeUSD = &volumeUSD
		}
		rows = append(rows, row)
	}
	return s.store.InsertPriceSourceContributions(ctx, rows)
}
