package main

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/orchestrator"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	return s.store.InsertPriceSourceContributions(ctx, contributionRows(rec))
}

// contributionScale is the fractional digits a weight and a USD volume
// are rendered at: exact for any USD amount scaled by up to 10^18, and a
// weight's rounding error stays below 1e-18.
const contributionScale = 18

// contributionRows maps one record to its storage rows. rec.Window is
// carried onto every row: it is the only thing telling the 5m, 1h and
// 24h breakdowns of one pair apart.
func contributionRows(rec orchestrator.ContributionRecord) []timescale.PriceSourceContribution {
	// Read per-source USD volume directly from the post-filter
	// breakdown the orchestrator supplies. SourceUSDVolume sums
	// per-trade USD over the same surviving trade slice that computed
	// Contributions[].Weight, so the persisted `volume_usd` matches the
	// published contribution set even when outliers/class-filter
	// dropped rows. Sources with no positive SourceUSDVolume entry get
	// NULL rather than a fabricated value.
	rows := make([]timescale.PriceSourceContribution, 0, len(rec.Contributions))
	for _, c := range rec.Contributions {
		row := timescale.PriceSourceContribution{
			AssetID:    rec.Pair.Base.String(),
			QuoteID:    rec.Pair.Quote.String(),
			Window:     rec.Window,
			Bucket:     rec.ComputedAt,
			Source:     c.Source,
			Weight:     c.Weight.FloatString(contributionScale),
			TradeCount: c.TradeCount,
		}
		if v, ok := rec.SourceUSDVolume[c.Source]; ok && v.Sign() > 0 {
			vol := v.FloatString(contributionScale)
			row.VolumeUSD = &vol
		}
		rows = append(rows, row)
	}
	return rows
}
