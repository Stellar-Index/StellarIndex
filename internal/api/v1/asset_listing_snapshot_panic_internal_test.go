// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// panicOnceListings panics on its first read and answers every later one.
type panicOnceListings struct{ reads int }

func (p *panicOnceListings) ListingDirectoryByAddress(context.Context) (
	map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error,
) {
	p.reads++
	if p.reads == 1 {
		panic("listing read blew up")
	}
	return map[string]timescale.ListingEntry{}, timescale.ListingDirectoryCensus{}, nil
}
