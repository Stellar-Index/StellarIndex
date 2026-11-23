// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A SEP-40 answer whose own fiat:USD book is refused as thin may be priced through a USD-peg book (X/USDC), on lastprice and
// prices() alike, only when that book passed the reader's own gates (substance, flagged issuer, exclusions, read) and every USD
// peg is observed inside its depeg band over the span the answer was priced at. Any other refusal stands.

// errPegBandUnread: a peg market's band check could not be read, so whether a peg book may stand in is unknown.
var errPegBandUnread = errors.New("usd peg band check unread")

// thinOnly reports a refusal that is a substance verdict and nothing else: a flagged issuer, an exclusion (read or not) and an
// unattributed refusal are not thin.
func thinOnly(err error) bool {
	return errors.Is(err, ErrPriceWithheld) && priceWithheldReason(err) == PriceWithheldSubstance
}

// pegHeldOver reports whether every USD peg was observed inside its band over [from, to] ([Server.pegRangeDeviation]). With no
// point-in-time reader the peg is unchecked, never held; a read failure with no deviation seen is [errPegBandUnread].
func (s *Server) pegHeldOver(ctx context.Context, from, to time.Time) (bool, error) {
	if s.PriceAt == nil {
		return false, nil
	}
	deviated, unchecked, failed := s.pegRangeDeviation(ctx, defaultPriceQuote, from, to, nil)
	switch {
	case deviated:
		return false, nil
	case failed:
		if cerr := ctx.Err(); cerr != nil {
			return false, fmt.Errorf("%w: %w", errPegBandUnread, cerr)
		}
		return false, errPegBandUnread
	}
	return !unchecked, nil
}

// thinDirectPegRefusal is the error prices() answers instead of a peg series standing in for a direct series refused as thin:
// the direct refusal when the series carries a withheld or unread bucket or a peg left (or was not seen inside) its band over
// the series' span, a read error when the band check was unread, nil when the series qualifies or the direct read was not thin.
func (s *Server) thinDirectPegRefusal(ctx context.Context, directErr error, series []PriceSnapshot) error {
	if !thinOnly(directErr) {
		return nil
	}
	if slices.ContainsFunc(series, func(p PriceSnapshot) bool { return p.FillsWithheld || p.FillsUnread }) {
		return directErr
	}
	from, to := series[0].ObservedAt.Time(), series[0].ObservedAt.Time()
	for _, p := range series[1:] {
		at := p.ObservedAt.Time()
		if at.Before(from) {
			from = at
		}
		if at.After(to) {
			to = at
		}
	}
	held, err := s.pegHeldOver(ctx, from, to)
	if err != nil {
		return err
	}
	if !held {
		return directErr
	}
	return nil
}

// thinDirectPegAnswer is lastprice's answer for a direct fiat:USD read refused as thin: the first spelling's USD-peg book the
// gated reader serves ([Server.walkUSDPegs]), when every peg held its band at that observation. ok=false keeps the refusal; a
// USD stablecoin never rides another peg's book.
func (s *Server) thinDirectPegAnswer(ctx context.Context, asset canonical.Asset, directErr error) (fallbackResult, error) {
	if !thinOnly(directErr) || s.isUSDStablecoin(asset) || s.Prices == nil {
		return fallbackResult{}, nil
	}
	for _, a := range assetAliases(asset) {
		snap, srcs, ok, _, err := s.walkUSDPegs(ctx, a, defaultPriceQuote)
		if err != nil {
			return fallbackResult{}, err
		}
		if !ok {
			continue
		}
		at := snap.ObservedAt.Time()
		held, err := s.pegHeldOver(ctx, at, at)
		if err != nil || !held {
			return fallbackResult{}, err
		}
		return fallbackResult{snap: snap, sources: srcs, triangulated: true, ok: true, stale: true}, nil
	}
	return fallbackResult{}, nil
}

// writeLastPriceThinDirect answers a withheld direct lastprice read: ok with the qualifying peg answer, else it writes the
// direct refusal (404 price-withheld) or, for an unread layer, the retryable 503.
func (s *Server) writeLastPriceThinDirect(
	ctx context.Context, w http.ResponseWriter, r *http.Request, asset canonical.Asset, directErr error,
) (fallbackResult, bool) {
	fb, err := s.thinDirectPegAnswer(ctx, asset, directErr)
	switch {
	case err != nil:
		s.writeFallbackMiss(w, r, asset, defaultPriceQuote, fallbackResult{err: err}, nil)
		return fallbackResult{}, false
	case !fb.ok:
		writePriceWithheldProblem(w, r, asset, defaultPriceQuote, priceWithheldReason(directErr))
		return fallbackResult{}, false
	}
	return fb, true
}
