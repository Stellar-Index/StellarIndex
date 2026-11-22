package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// PriceExclusionGate is the operator source-exclusion seam for values the server computes or reads itself.
type PriceExclusionGate interface {
	Withholding(ctx context.Context, base, quote canonical.Asset, sources []string, from, to time.Time) pricingguard.Withholding
	InstantWithholding(ctx context.Context, base, quote canonical.Asset, source string, ts time.Time) pricingguard.Withholding
	UnattributedWithholding(ctx context.Context, base, quote canonical.Asset, from, to time.Time) pricingguard.Withholding
	MultiHopWithholding(ctx context.Context, from, to time.Time) pricingguard.Withholding
}

// exclusionCleanTrades drops trades from an excluded source; the survivors are each one venue's own fill, so a VWAP over them
// is clean. An unreadable exclusion set is a withheld error, never "nothing excluded".
func (s *Server) exclusionCleanTrades(ctx context.Context, asset, quote canonical.Asset, trades []canonical.Trade) ([]canonical.Trade, error) {
	if s.Exclusions == nil {
		return trades, nil
	}
	kept := trades[:0:0]
	for _, t := range trades {
		switch w := s.Exclusions.InstantWithholding(ctx, asset, quote, t.Source, t.Timestamp); w {
		case pricingguard.NotWithheld:
			kept = append(kept, t)
		case pricingguard.WithheldSourceExcluded:
		default:
			return nil, PriceWithheldError(w)
		}
	}
	return kept, nil
}

// exclusionCleanWindow is [Server.exclusionCleanTrades] over a lifted raw-trade window, each trade judged on the book it
// executed on (a fiat-quoted window pools several stablecoin books).
func (s *Server) exclusionCleanWindow(ctx context.Context, window aggregate.ScaledWindow) (aggregate.ScaledWindow, error) {
	if s.Exclusions == nil {
		return window, nil
	}
	refused := pricingguard.NotWithheld
	kept := window.Filter(func(t canonical.Trade) bool {
		w := s.Exclusions.InstantWithholding(ctx, t.Pair.Base, t.Pair.Quote, t.Source, t.Timestamp)
		if w != pricingguard.NotWithheld && w != pricingguard.WithheldSourceExcluded {
			refused = w
		}
		return w == pricingguard.NotWithheld
	})
	if refused != pricingguard.NotWithheld {
		return aggregate.ScaledWindow{}, PriceWithheldError(refused)
	}
	return kept, nil
}

// writeExclusionCleanWindow is [Server.exclusionCleanWindow] for a handler: it writes the withheld problem (an emptied window
// included, which is excluded rather than "no trades") and returns ok=false.
func (s *Server) writeExclusionCleanWindow(w http.ResponseWriter, r *http.Request, base, quote canonical.Asset, window aggregate.ScaledWindow) (aggregate.ScaledWindow, bool) {
	kept, err := s.exclusionCleanWindow(r.Context(), window)
	if err == nil && kept.Len() == 0 && window.Len() > 0 {
		err = PriceWithheldError(pricingguard.WithheldSourceExcluded)
	}
	if err != nil {
		writePriceWithheldProblem(w, r, base, quote, priceWithheldReason(err))
		return aggregate.ScaledWindow{}, false
	}
	return kept, true
}

// fxRateWithheld judges one FX rate, source's own reading of ticker per USD over [from, to); nil when no exclusion covers it.
func (s *Server) fxRateWithheld(ctx context.Context, ticker, source string, from, to time.Time) error {
	if s.Exclusions == nil || ticker == "USD" {
		return nil
	}
	fiat := canonical.Asset{Type: canonical.AssetFiat, Code: ticker}
	if w := s.Exclusions.Withholding(ctx, fiat, defaultPriceQuote, []string{source}, from, to); w != pricingguard.NotWithheld {
		return PriceWithheldError(w)
	}
	return nil
}

// fixingWithheld is [Server.fxRateWithheld] over a bound fixing's bar, which pools the source's readings across it.
func (s *Server) fixingWithheld(ctx context.Context, ticker string, b timescale.FXFixingBinding) error {
	from, to := b.BarStart, b.BarEnd
	if !from.Before(to) {
		from, to = b.BarEnd, b.BarEnd.Add(time.Nanosecond)
	}
	return s.fxRateWithheld(ctx, ticker, b.Source, from, to)
}

// liveRatesWithheld is [Server.liveRateWithheld] over every leg of a cross; the first covered leg withholds it.
func (s *Server) liveRatesWithheld(ctx context.Context, legs []CurrencyEntry, publishedAt time.Time) error {
	for _, c := range legs {
		if err := s.liveRateWithheld(ctx, c, publishedAt); err != nil {
			return err
		}
	}
	return nil
}

// liveRateWithheld is [Server.fxRateWithheld] for a forex snapshot entry at its own update time.
func (s *Server) liveRateWithheld(ctx context.Context, c CurrencyEntry, publishedAt time.Time) error {
	at := c.UpdatedAt
	if at.IsZero() {
		at = publishedAt
	}
	return s.fxRateWithheld(ctx, c.Ticker, fxSourceOf(c), at, at.Add(time.Nanosecond))
}

// priceWithheldUnread reports whether err withholds because the exclusion set could not be read: a transient refusal that a
// cached body must not replay as a clean withheld one.
func priceWithheldUnread(err error) bool {
	var e *withheldError
	return errors.As(err, &e) && e.unread
}

// unattributedWithheld judges a value with no source list that covers [from, now]. multiHop marks a value that may be a
// composite over unrecorded legs (a triangulated cache value, or one whose provenance is unknown): any exclusion withholds it.
func (s *Server) unattributedWithheld(ctx context.Context, asset, quote canonical.Asset, from time.Time, multiHop bool) pricingguard.Withholding {
	if s.Exclusions == nil {
		return pricingguard.NotWithheld
	}
	to := time.Now().Add(time.Nanosecond)
	if multiHop {
		return s.Exclusions.MultiHopWithholding(ctx, from, to)
	}
	return s.Exclusions.UnattributedWithholding(ctx, asset, quote, from, to)
}
