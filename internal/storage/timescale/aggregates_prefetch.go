package timescale

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// prefetchChunk bounds the pairs one set statement carries; prefetchParallel the statements one prefetch runs at once.
const (
	prefetchChunk    = 100
	prefetchParallel = 4
)

// trailingKey is a trailing-buckets read's memo key: the pair as asked and its row limit.
type trailingKey struct {
	readset.PairKey
	limit int
}

var (
	latestSeam   = readset.NewSeam[readset.PairKey, KeyResult[Vwap1mRow]]("latest_vwap_1m", cloneRowResult)
	trailingSeam = readset.NewSeam[trailingKey, []Vwap1mRow]("trailing_vwap_1m", cloneRows)
)

func cloneRowResult(r KeyResult[Vwap1mRow]) KeyResult[Vwap1mRow] {
	r.Value.Sources = slices.Clone(r.Value.Sources)
	return r
}

func cloneRows(rows []Vwap1mRow) []Vwap1mRow {
	if rows == nil {
		return nil
	}
	out := make([]Vwap1mRow, len(rows))
	for i, r := range rows {
		r.Sources = slices.Clone(r.Sources)
		out[i] = r
	}
	return out
}

func memoPairKey(p canonical.Pair) readset.PairKey { return readset.Pair(p.Base, p.Quote, 0) }

// PrefetchStore is a [Store] whose latest-bucket and trailing-bucket reads answer a pair the request's batch prefetch
// ([PrefetchStore.PrefetchLatest]) read from that read, and every other read exactly as Store.
type PrefetchStore struct{ *Store }

// LatestClosedVWAP1mForPair is [Store.LatestClosedVWAP1mForPair] through ctx's memo.
func (p PrefetchStore) LatestClosedVWAP1mForPair(ctx context.Context, pair canonical.Pair) (Vwap1mRow, error) {
	if readset.From(ctx) == nil {
		return p.Store.LatestClosedVWAP1mForPair(ctx, pair)
	}
	r, _, _ := latestSeam.Read(ctx, memoPairKey(pair), func(ctx context.Context) (KeyResult[Vwap1mRow], bool, error) {
		row, err := p.Store.LatestClosedVWAP1mForPair(ctx, pair)
		return KeyResult[Vwap1mRow]{Value: row, Err: err}, true, nil
	})
	return r.Value, r.Err
}

// RecentClosedVWAP1mCombined is [Store.RecentClosedVWAP1mCombined] through ctx's memo.
func (p PrefetchStore) RecentClosedVWAP1mCombined(ctx context.Context, pair canonical.Pair, limit int) ([]Vwap1mRow, error) {
	if readset.From(ctx) == nil {
		return p.Store.RecentClosedVWAP1mCombined(ctx, pair, limit)
	}
	rows, _, err := trailingSeam.Read(ctx, trailingKey{memoPairKey(pair), limit}, func(ctx context.Context) ([]Vwap1mRow, bool, error) {
		rows, err := p.Store.RecentClosedVWAP1mCombined(ctx, pair, limit)
		return rows, true, err
	})
	return rows, err
}

// PrefetchLatest fills ctx's memo with every pair's latest closed bucket, then with the trailing limit buckets of each
// pair that has one (the serving guard's read), in statements of at most prefetchChunk pairs. Only a definite answer
// is memoised: a failed statement leaves every pair it carried unfilled, so each is read as before.
func (p PrefetchStore) PrefetchLatest(ctx context.Context, pairs []canonical.Pair, limit int) {
	if readset.From(ctx) == nil || len(pairs) == 0 {
		return
	}
	var (
		mu   sync.Mutex
		live []canonical.Pair
	)
	forChunks(pairs, func(chunk []canonical.Pair) {
		res, err := p.Store.LatestClosedVWAP1mForPairSet(ctx, chunk)
		if err != nil {
			for range chunk {
				latestSeam.Failed()
			}
			return
		}
		for i, r := range res {
			if r.Err != nil && !errors.Is(r.Err, sql.ErrNoRows) {
				latestSeam.Failed()
				continue
			}
			latestSeam.Fill(ctx, memoPairKey(chunk[i]), r, true)
			if r.Err == nil {
				mu.Lock()
				live = append(live, chunk[i])
				mu.Unlock()
			}
		}
	})
	forChunks(live, func(chunk []canonical.Pair) {
		rows, err := p.Store.RecentClosedVWAP1mCombinedSet(ctx, chunk, limit)
		if err != nil {
			for range chunk {
				trailingSeam.Failed()
			}
			return
		}
		for i, r := range rows {
			trailingSeam.Fill(ctx, trailingKey{memoPairKey(chunk[i]), limit}, r, true)
		}
	})
}

// forChunks calls f on each prefetchChunk-sized slice of pairs, prefetchParallel at a time, and waits for all of them.
func forChunks(pairs []canonical.Pair, f func([]canonical.Pair)) {
	sem := make(chan struct{}, prefetchParallel)
	var wg sync.WaitGroup
	for lo := 0; lo < len(pairs); lo += prefetchChunk {
		chunk := pairs[lo:min(lo+prefetchChunk, len(pairs))]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// A panic leaves its chunk unfilled, so those pairs are read as before.
			defer worker.Recover(nil, "timescale-price-prefetch")
			f(chunk)
		}()
	}
	wg.Wait()
}
