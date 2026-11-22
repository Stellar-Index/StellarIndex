package v1

import (
	"context"
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// PriceExclusionGate is the operator source-exclusion seam for values the server computes or reads itself.
type PriceExclusionGate interface {
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
