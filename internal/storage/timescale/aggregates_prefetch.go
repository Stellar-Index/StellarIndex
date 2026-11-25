package timescale

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"time"

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
	latestSeam   = readset.NewPairSeam[KeyResult[Vwap1mRow]]("latest_vwap_1m", cloneRowResult)
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
	r, found, _ := latestSeam.Read(ctx, memoPairKey(pair), func(ctx context.Context) (KeyResult[Vwap1mRow], bool, error) {
		row, err := p.Store.LatestClosedVWAP1mForPair(ctx, pair)
		return KeyResult[Vwap1mRow]{Value: row, Err: err}, true, nil
	})
	if !found { // a memoised definite miss
		return Vwap1mRow{}, sql.ErrNoRows
	}
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
		found := p.prefetchLatestChunk(ctx, chunk)
		mu.Lock()
		live = append(live, found...)
		mu.Unlock()
	})
	forChunks(live, func(chunk []canonical.Pair) {
		rows, err := p.RecentClosedVWAP1mCombinedSet(ctx, chunk, limit)
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

// prefetchLatestChunk fills ctx's memo with each pair's latest closed bucket and returns the pairs that have one.
func (p PrefetchStore) prefetchLatestChunk(ctx context.Context, chunk []canonical.Pair) []canonical.Pair {
	res, err := p.LatestClosedVWAP1mForPairSet(ctx, chunk)
	if err != nil {
		for range chunk {
			latestSeam.Failed()
		}
		return nil
	}
	var live []canonical.Pair
	for i, r := range res {
		if r.Err != nil && !errors.Is(r.Err, sql.ErrNoRows) {
			latestSeam.Failed()
			continue
		}
		latestSeam.Fill(ctx, memoPairKey(chunk[i]), r, r.Err == nil)
		if r.Err == nil {
			live = append(live, chunk[i])
		}
	}
	return live
}

// forChunks calls f on each prefetchChunk-sized slice of pairs, prefetchParallel at a time, and waits for all of them.
func forChunks(pairs []canonical.Pair, f func([]canonical.Pair)) {
	forJobs(chunkJobs(nil, pairs, f))
}

// chunkJobs appends to jobs one call of f per prefetchChunk-sized slice of pairs.
func chunkJobs(jobs []func(), pairs []canonical.Pair, f func([]canonical.Pair)) []func() {
	for lo := 0; lo < len(pairs); lo += prefetchChunk {
		chunk := pairs[lo:min(lo+prefetchChunk, len(pairs))]
		jobs = append(jobs, func() { f(chunk) })
	}
	return jobs
}

// forJobs runs jobs prefetchParallel at a time and waits for all of them.
func forJobs(jobs []func()) {
	sem := make(chan struct{}, prefetchParallel)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// A panic leaves its chunk unfilled, so those pairs are read as before.
			defer worker.Recover(nil, "timescale-price-prefetch")
			job()
		}()
	}
	wg.Wait()
}

// AnchorReads is the change_24h anchor reads [PrefetchAnchors] decorates; [*Store] has them.
type AnchorReads interface {
	ClosedVWAP1mAtOrBefore(ctx context.Context, p canonical.Pair, t time.Time) (Vwap1mRow, error)
	ClosedVWAP1mCombinedBefore(ctx context.Context, p canonical.Pair, before time.Time, limit int) ([]Vwap1mRow, error)
	ClosedVWAP1mAtOrBeforeSet(ctx context.Context, pairs []canonical.Pair, t time.Time) ([]KeyResult[Vwap1mRow], error)
	ClosedVWAP1mCombinedBeforeSet(ctx context.Context, pairs []canonical.Pair, before time.Time, limit int) ([][]Vwap1mRow, error)
}

// PrefetchAnchors is AnchorReads whose anchor and anchored-baseline reads answer a pair the request's batch prefetch
// ([PrefetchAnchors.PrefetchChange24hAnchors]) read, and every other read exactly as AnchorReads.
type PrefetchAnchors struct{ AnchorReads }

// anchorKey is an at-or-before read's memo key: the pair as asked and the minute of its instant, which is all the
// read's answer depends on, since prices_1m buckets are minute-aligned.
type anchorKey struct {
	readset.PairKey
	minute int64
}

// beforeKey is an anchored-baseline read's memo key: the pair as asked, its exact anchor and its row limit.
type beforeKey struct {
	readset.PairKey
	before int64
	limit  int
}

var (
	anchorSeam         = readset.NewSeam[anchorKey, KeyResult[Vwap1mRow]]("anchor_vwap_1m", cloneRowResult)
	anchorTrailingSeam = readset.NewSeam[beforeKey, []Vwap1mRow]("anchor_trailing_vwap_1m", cloneRows)
)

// anchorMinute is t's memo minute; ok is false within a microsecond of the next minute, where rounding t to the
// database's microseconds could make the read that minute's.
func anchorMinute(t time.Time) (minute int64, ok bool) {
	m := t.Truncate(time.Minute)
	return m.UnixNano(), t.Add(time.Microsecond).Truncate(time.Minute).Equal(m)
}

// ClosedVWAP1mAtOrBefore is [Store.ClosedVWAP1mAtOrBefore] through ctx's memo.
func (p PrefetchAnchors) ClosedVWAP1mAtOrBefore(ctx context.Context, pair canonical.Pair, t time.Time) (Vwap1mRow, error) {
	minute, ok := anchorMinute(t)
	if !ok || readset.From(ctx) == nil {
		return p.AnchorReads.ClosedVWAP1mAtOrBefore(ctx, pair, t)
	}
	r, found, _ := anchorSeam.Read(ctx, anchorKey{memoPairKey(pair), minute}, func(ctx context.Context) (KeyResult[Vwap1mRow], bool, error) {
		row, err := p.AnchorReads.ClosedVWAP1mAtOrBefore(ctx, pair, t)
		return KeyResult[Vwap1mRow]{Value: row, Err: err}, true, nil
	})
	if !found { // a memoised definite miss
		return Vwap1mRow{}, sql.ErrNoRows
	}
	return r.Value, r.Err
}

// ClosedVWAP1mCombinedBefore is [Store.ClosedVWAP1mCombinedBefore] through ctx's memo.
func (p PrefetchAnchors) ClosedVWAP1mCombinedBefore(ctx context.Context, pair canonical.Pair, before time.Time, limit int) ([]Vwap1mRow, error) {
	if readset.From(ctx) == nil {
		return p.AnchorReads.ClosedVWAP1mCombinedBefore(ctx, pair, before, limit)
	}
	k := beforeKey{memoPairKey(pair), before.UnixNano(), limit}
	rows, _, err := anchorTrailingSeam.Read(ctx, k, func(ctx context.Context) ([]Vwap1mRow, bool, error) {
		rows, err := p.AnchorReads.ClosedVWAP1mCombinedBefore(ctx, pair, before, limit)
		return rows, true, err
	})
	return rows, err
}

// PrefetchChange24hAnchors fills ctx's memo with each pair's newest closed bucket at or before at, then with the limit
// buckets before each such bucket not older than notBefore (the serving guard's baseline), in statements of at most
// prefetchChunk pairs. A failed statement leaves every pair it carried unfilled, so each is read as before.
func (p PrefetchAnchors) PrefetchChange24hAnchors(ctx context.Context, pairs []canonical.Pair, at, notBefore time.Time, limit int) {
	minute, ok := anchorMinute(at)
	if !ok || readset.From(ctx) == nil || len(pairs) == 0 {
		return
	}
	var (
		mu       sync.Mutex
		byBucket = map[time.Time][]canonical.Pair{}
	)
	forChunks(pairs, func(chunk []canonical.Pair) {
		for _, pair := range p.prefetchAnchorChunk(ctx, chunk, at, minute) {
			if !pair.bucket.Before(notBefore) {
				mu.Lock()
				byBucket[pair.bucket] = append(byBucket[pair.bucket], pair.pair)
				mu.Unlock()
			}
		}
	})
	var jobs []func()
	for bucket, group := range byBucket {
		jobs = chunkJobs(jobs, group, func(chunk []canonical.Pair) { p.prefetchBaselineChunk(ctx, chunk, bucket, limit) })
	}
	forJobs(jobs)
}

type anchoredPair struct {
	pair   canonical.Pair
	bucket time.Time
}

// prefetchAnchorChunk fills ctx's memo with each pair's anchor at at and returns the pairs that have one.
func (p PrefetchAnchors) prefetchAnchorChunk(ctx context.Context, chunk []canonical.Pair, at time.Time, minute int64) []anchoredPair {
	res, err := p.ClosedVWAP1mAtOrBeforeSet(ctx, chunk, at)
	if err != nil {
		for range chunk {
			anchorSeam.Failed()
		}
		return nil
	}
	var found []anchoredPair
	for i, r := range res {
		if r.Err != nil && !errors.Is(r.Err, sql.ErrNoRows) {
			anchorSeam.Failed()
			continue
		}
		anchorSeam.Fill(ctx, anchorKey{memoPairKey(chunk[i]), minute}, r, r.Err == nil)
		if r.Err == nil {
			found = append(found, anchoredPair{chunk[i], r.Value.Bucket})
		}
	}
	return found
}

// prefetchBaselineChunk fills ctx's memo with each pair's limit buckets before bucket.
func (p PrefetchAnchors) prefetchBaselineChunk(ctx context.Context, chunk []canonical.Pair, bucket time.Time, limit int) {
	rows, err := p.ClosedVWAP1mCombinedBeforeSet(ctx, chunk, bucket, limit)
	if err != nil {
		for range chunk {
			anchorTrailingSeam.Failed()
		}
		return
	}
	for i, r := range rows {
		anchorTrailingSeam.Fill(ctx, beforeKey{memoPairKey(chunk[i]), bucket.UnixNano(), limit}, r, true)
	}
}
