package v1

import (
	"context"

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
	frozenPrefetcher interface {
		PrefetchFrozen(context.Context, []readset.PairRead)
	}
)

// batchPrefetchBudget bounds the planner: a prefetch slower than this leaves too little of [readset.MaxAge] to be read.
const batchPrefetchBudget = readset.MaxAge / 2

// prefetchBatch returns ctx carrying a memo the batch's first read stages are prefetched into, before its rows start:
// every alias spelling of every id against quote, as the row's alias walk, Redis VWAP fallback and freeze check ask.
//
// Invariant: each seam answers a key exactly as its single-key read would have at an instant no more than
// [readset.MaxAge] before the read; any key not read, or read longer ago, is read as before — the prefetch changes
// when reads happen, never what they return or how they are judged. A prefetch that fails, panics or overruns its
// budget only leaves keys unfilled; it never fails the request.
func (s *Server) prefetchBatch(ctx context.Context, ids []string, quote canonical.Asset) context.Context {
	fills := s.batchPrefetchFills()
	if len(fills) == 0 {
		return ctx
	}
	reads, ok := batchPrefetchReads(ids, quote)
	if !ok {
		return ctx
	}
	m := readset.New()
	pctx, cancel := context.WithTimeout(readset.With(ctx, m), batchPrefetchBudget)
	defer cancel()
	done := make(chan struct{}, len(fills))
	for _, fill := range fills {
		go func() {
			defer func() { done <- struct{}{} }()
			defer worker.Recover(s.logger, "api-price-batch-prefetch")
			fill(pctx, reads)
		}()
	}
	for range fills {
		select {
		case <-done:
		case <-pctx.Done(): // a fill still running past the budget only fills entries that will read as expired
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
	if p, ok := s.Freeze.(frozenPrefetcher); ok {
		out = append(out, func(ctx context.Context, reads []readset.PairRead) { p.PrefetchFrozen(ctx, unwindowed(reads)) })
	}
	return out
}

// batchPrefetchReads is the (spelling, quote) reads a batch's rows begin with, windowed for the Redis VWAP fallback: per id,
// [assetAliases] in the order [Server.readPriceWithAliasesServedOnce], [Server.tryRedisVWAPFallback] and
// [Server.frozenAnyAlias] walk them. ok is false when an id would fail the batch, which then reads nothing.
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
