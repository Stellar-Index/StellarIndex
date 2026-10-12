// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	// tradesDriftSamples windows of tradesDriftWindow are compared across a refreshed span (the first and last on its edges); a shorter
	// span is compared whole.
	tradesDriftSamples = 8
	tradesDriftWindow  = time.Hour
	// prices1mSettle is prices_1m's policy start_offset (migration 0165, 15 minutes) plus margin: newer minutes may hold a live insert not
	// yet materialised, not drift.
	prices1mSettle = 20 * time.Minute
	// tradesDriftShown caps the drifting pairs printed per window.
	tradesDriftShown = 20
)

// tradesDriftStore is the slice of *timescale.Store the check needs.
type tradesDriftStore interface {
	TradesPrices1mDrift(ctx context.Context, from, to time.Time) ([]timescale.TradesPrices1mDrift, error)
}

// tradesDriftSampleWindows returns the whole-minute windows to compare over [from, to]: one over a short span, else tradesDriftSamples
// spread evenly; everything at or after cutoff is left out.
func tradesDriftSampleWindows(from, to, cutoff time.Time) [][2]time.Time {
	const (
		n     = tradesDriftSamples
		width = tradesDriftWindow
	)
	lo := from.Truncate(time.Minute)
	hi := to.Truncate(time.Minute).Add(time.Minute)
	var wins [][2]time.Time
	if span := hi.Sub(lo); span <= time.Duration(n)*width || n < 2 {
		wins = [][2]time.Time{{lo, hi}}
	} else {
		step := (span - width) / time.Duration(n-1)
		for i := range n {
			start := lo.Add(step * time.Duration(i)).Truncate(time.Minute)
			if i == n-1 {
				start = hi.Add(-width)
			}
			wins = append(wins, [2]time.Time{start, start.Add(width)})
		}
	}
	cut := cutoff.Truncate(time.Minute)
	out := wins[:0]
	for _, w := range wins {
		if w[1].After(cut) {
			w[1] = cut
		}
		if w[0].Before(w[1]) {
			out = append(out, w)
		}
	}
	return out
}

// checkTradesPrices1mDrift compares `trades` with prices_1m over sampled windows of [from, to], prints drifting pairs, fails on any
// disagreement and returns the windows compared; after a refresh a difference means prices_1m and its dependent views still serve rows
// `trades` no longer holds.
func checkTradesPrices1mDrift(ctx context.Context, s tradesDriftStore, from, to, now time.Time, out io.Writer) (int, error) {
	wins := tradesDriftSampleWindows(from, to, now.Add(-prices1mSettle))
	drifted := 0
	for _, w := range wins {
		rows, err := s.TradesPrices1mDrift(ctx, w[0], w[1])
		if err != nil {
			return 0, fmt.Errorf("compare trades with prices_1m over %s: %w", fmtDriftWindow(w), err)
		}
		if len(rows) == 0 {
			continue
		}
		drifted++
		if err := printTradesDrift(out, w, rows); err != nil {
			return 0, err
		}
	}
	if drifted > 0 {
		return 0, fmt.Errorf("prices_1m disagrees with trades in %d of %d sampled window(s) over [%s,%s] after the refresh (pairs above)",
			drifted, len(wins), from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	return len(wins), nil
}

// tradesDriftCheckPrefix starts every line the check prints.
const tradesDriftCheckPrefix = "trades-cagg-refresh: drift check:"

func printTradesDrift(out io.Writer, w [2]time.Time, rows []timescale.TradesPrices1mDrift) error {
	if _, err := fmt.Fprintf(out, "%s DRIFT over %s in %d pair(s)\n", tradesDriftCheckPrefix, fmtDriftWindow(w), len(rows)); err != nil {
		return err
	}
	for i, d := range rows {
		if i == tradesDriftShown {
			_, err := fmt.Fprintf(out, "  … %d more\n", len(rows)-i)
			return err
		}
		if _, err := fmt.Fprintf(out, "  %s/%s trades n=%s vol=%s usd=%s  prices_1m n=%s vol=%s usd=%s\n",
			d.BaseAsset, d.QuoteAsset, d.TradeCount, d.TradeVolume, d.TradeUSD,
			d.CAGGCount, d.CAGGVolume, d.CAGGUSD); err != nil {
			return err
		}
	}
	return nil
}

func fmtDriftWindow(w [2]time.Time) string {
	return fmt.Sprintf("[%s,%s)", w[0].UTC().Format(time.RFC3339), w[1].UTC().Format(time.RFC3339))
}
