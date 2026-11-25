package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// The optional prefetch capabilities a batch's read seams may have; each fills ctx's memo for the reads given.
type (
	latestPricePrefetcher interface {
		PrefetchLatestPrices(context.Context, []readset.PairRead)
	}
	triangulatedPrefetcher interface {
		PrefetchTriangulatedVWAP(context.Context, []readset.PairRead)
	}
)

const (
	// batchPrefetchBudget bounds the wait for the fills: one slower than this leaves too little of [readset.MaxAge].
	batchPrefetchBudget = readset.MaxAge / 2
	// batchPrefetchMinIDs: below this a batch's per-row reads cost about what the prefetch round trips would.
	batchPrefetchMinIDs = 8
	// batchPrefetchMaxIDs: rows run 16 at a time, so later rows start after the 2 s expiry, and the memo caps at 8192 entries.
	batchPrefetchMaxIDs = 200
)

// prefetchBatch returns ctx carrying a memo the batch's first read stages are prefetched into, before its rows start:
// every alias spelling of the first batchPrefetchMaxIDs ids against quote, as the row's alias walk and Redis VWAP
// fallback ask. Every alias is read although the walk stops at its first fresh one: one more key in a set statement is
// cheap, and a missing one costs that row its own round trip. The freeze marker is not prefetched: a row's freeze verdict
// must be read after the price it judges.
//
// Invariant: each seam answers a key exactly as its single-key read would have at an instant no more than
// [readset.MaxAge] before the read; any key not read, or read longer ago, is read as before — the prefetch changes
// when reads happen, never what they return or how they are judged. A fill that fails, panics or outlasts the budget
// only leaves keys unfilled; it never fails the request.
func (s *Server) prefetchBatch(ctx context.Context, ids []string, quote canonical.Asset) context.Context {
	if len(ids) < batchPrefetchMinIDs {
		return ctx
	}
	fills := s.batchPrefetchFills()
	if len(fills) == 0 {
		return ctx
	}
	reads, ok := batchPrefetchReads(ids[:min(len(ids), batchPrefetchMaxIDs)], quote)
	if !ok {
		return ctx
	}
	m := readset.New()
	mctx := readset.With(ctx, m)
	done := make(chan struct{}, len(fills))
	for _, fill := range fills {
		go func() {
			defer func() { done <- struct{}{} }()
			defer worker.Recover(s.logger, "api-price-batch-prefetch")
			// The budget stops only the wait: cancelling a statement in flight makes pgx discard its connection.
			fctx, cancel := context.WithTimeout(context.WithoutCancel(mctx), readset.MaxAge)
			defer cancel()
			fill(fctx, reads)
		}()
	}
	budget := time.NewTimer(batchPrefetchBudget)
	defer budget.Stop()
	for range fills {
		select {
		case <-done:
		case <-budget.C: // a late fill only fills entries that read as expired
			return readset.With(ctx, m)
		case <-ctx.Done():
			return readset.With(ctx, m)
		}
	}
	return readset.With(ctx, m)
}

// batchPrefetchFills is every prefetch capability this server's seams have, each with the windowed reads it takes.
func (s *Server) batchPrefetchFills() []func(context.Context, []readset.PairRead) {
	var out []func(context.Context, []readset.PairRead)
	if p, ok := s.Prices.(latestPricePrefetcher); ok {
		out = append(out, func(ctx context.Context, reads []readset.PairRead) { p.PrefetchLatestPrices(ctx, unwindowed(reads)) })
	}
	if p, ok := s.Triangulated.(triangulatedPrefetcher); ok {
		out = append(out, p.PrefetchTriangulatedVWAP)
	}
	return out
}

// batchPrefetchReads is the (spelling, quote) reads a batch's rows begin with, windowed for the Redis VWAP fallback: per
// id, [assetAliases] in the order [Server.readPriceWithAliasesServedOnce] and [Server.tryRedisVWAPFallback] walk them.
// ok is false when an id would fail the batch, which then reads nothing.
func batchPrefetchReads(ids []string, quote canonical.Asset) (reads []readset.PairRead, ok bool) {
	seen := map[readset.PairKey]bool{}
	for _, raw := range ids {
		asset, err := parsePriceLeg(raw)
		if err != nil || asset.Equal(quote) {
			return nil, false
		}
		for _, a := range assetAliases(asset) {
			r := readset.PairRead{Base: a, Quote: quote, Window: triangulationLookupWindow}
			if a.Equal(quote) || seen[r.Key()] {
				continue
			}
			seen[r.Key()] = true
			reads = append(reads, r)
		}
	}
	return reads, true
}

func unwindowed(reads []readset.PairRead) []readset.PairRead {
	out := make([]readset.PairRead, len(reads))
	for i, r := range reads {
		out[i] = readset.PairRead{Base: r.Base, Quote: r.Quote}
	}
	return out
}

// memoCoversWalk reports whether ctx's memo answers every price read asset's alias walk against quote makes.
func memoCoversWalk(ctx context.Context, asset, quote canonical.Asset) bool {
	for _, a := range assetAliases(asset) {
		if !a.Equal(quote) && !readset.Covers(ctx, a, quote) {
			return false
		}
	}
	return true
}
