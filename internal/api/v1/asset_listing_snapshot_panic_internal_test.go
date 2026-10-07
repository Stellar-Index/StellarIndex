// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"

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

// A read that panics must neither wedge the snapshot mutex nor leave a
// freshly stamped TTL over the entry it never filled: the next caller
// re-reads instead of being served the pre-panic snapshot for a full TTL.
func TestAssetListingSnapshot_PanickingReadIsNotCached(t *testing.T) {
	rd := &panicOnceListings{}
	s := &Server{Options: Options{Listings: rd}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the first read did not panic — the fixture no longer exercises the path")
			}
		}()
		s.assetListingSnapshot(context.Background())
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.assetListingSnapshot(context.Background())
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("the snapshot mutex stayed held after a panicking read")
	}
	if rd.reads != 2 {
		t.Fatalf("reads = %d, want 2 — the panicked read's TTL stamp was served as a cached answer", rd.reads)
	}
}
