// Source-exclusion gate: the serving side of `stellarindex-ops source-exclude`. A prices_1m bucket
// pools every source and cannot be re-folded without one, so a served value that an un-lifted
// exclusion covers is withheld rather than served as clean.
//
// Fail-CLOSED, unlike the scam and substance gates: this is a kill switch, so an unreadable table
// must not read as "nothing excluded". The last good read is reused for exclusionMaxStale so a
// brief store error does not blank every price.
package pricingguard

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// SourceExclusionReader is the store seam the gate reads; *timescale.Store satisfies it.
type SourceExclusionReader interface {
	UnliftedSourceExclusions(ctx context.Context) ([]timescale.SourceExclusion, error)
}

const (
	exclusionCacheTTL = 15 * time.Second
	exclusionMaxStale = time.Minute
	// exclusionRetryInterval spaces read attempts after a failure; exclusionReadTimeout bounds one attempt.
	exclusionRetryInterval = 5 * time.Second
	exclusionReadTimeout   = 2 * time.Second
)

// SourceExclusionGate answers whether a served value depends on an excluded source. A nil gate withholds nothing.
type SourceExclusionGate struct {
	r      SourceExclusionReader
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	rows     []timescale.SourceExclusion
	readAt   time.Time     // zero until the first successful read
	triedAt  time.Time     // last read attempt, successful or not
	inflight chan struct{} // non-nil while a read runs; closed when it ends
}

// SourceExclusionGateOptions configures [NewSourceExclusionGate].
type SourceExclusionGateOptions struct {
	Logger *slog.Logger // nil → discard
}

// NewSourceExclusionGate builds a gate over r.
func NewSourceExclusionGate(r SourceExclusionReader, opts SourceExclusionGateOptions) *SourceExclusionGate {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SourceExclusionGate{r: r, logger: logger, now: time.Now}
}

// Withholding reports [WithheldSourceExcluded] when an un-lifted exclusion covers any of sources for base/quote over
// [from, to), [WithheldExclusionsUnread] when the exclusions cannot be read, else [NotWithheld].
func (g *SourceExclusionGate) Withholding(ctx context.Context, base, quote canonical.Asset, sources []string, from, to time.Time) Withholding {
	if g == nil {
		return NotWithheld
	}
	rows, ok := g.snapshot(ctx)
	if !ok {
		return WithheldExclusionsUnread
	}
	b, q := base.String(), quote.String()
	for _, e := range rows {
		for _, src := range sources {
			if e.CoversSpan(src, b, q, from, to) {
				return WithheldSourceExcluded
			}
		}
	}
	return NotWithheld
}

// UnattributedWithholding is [SourceExclusionGate.Withholding] for a value that carries no source list (a cached or
// triangulated VWAP): any un-lifted exclusion overlapping [from, to) that is source-wide, covers the pair, or names the base
// on either side of its own pair withholds it, since the value's legs and sources cannot be checked.
func (g *SourceExclusionGate) UnattributedWithholding(ctx context.Context, base, quote canonical.Asset, from, to time.Time) Withholding {
	if g == nil {
		return NotWithheld
	}
	rows, ok := g.snapshot(ctx)
	if !ok {
		return WithheldExclusionsUnread
	}
	b, q := base.String(), quote.String()
	for _, e := range rows {
		if !inForceOver(e, from, to) {
			continue
		}
		// CoversPair(b, e.Quote) holds when e.Base is b's family, CoversPair(e.Base, b) when e.Quote is.
		if e.CoversPair(b, q) || e.CoversPair(b, e.Quote) || e.CoversPair(e.Base, b) {
			return WithheldSourceExcluded
		}
	}
	return NotWithheld
}

// MultiHopWithholding judges a value composed through pivot legs it does not record (a triangulated cache value): any un-lifted
// exclusion overlapping [from, to) may sit on one of those legs, so any withholds it.
func (g *SourceExclusionGate) MultiHopWithholding(ctx context.Context, from, to time.Time) Withholding {
	if g == nil {
		return NotWithheld
	}
	rows, ok := g.snapshot(ctx)
	if !ok {
		return WithheldExclusionsUnread
	}
	for _, e := range rows {
		if inForceOver(e, from, to) {
			return WithheldSourceExcluded
		}
	}
	return NotWithheld
}

func inForceOver(e timescale.SourceExclusion, from, to time.Time) bool {
	return !e.Lifted() && e.From.Before(to) && (e.Until.IsZero() || e.Until.After(from))
}

func inForceCount(rows []timescale.SourceExclusion, at time.Time) int {
	n := 0
	for _, e := range rows {
		if inForceOver(e, at, at.Add(time.Nanosecond)) {
			n++
		}
	}
	return n
}

// BucketWithholding is [SourceExclusionGate.Withholding] for one closed 1m bucket and the sources it pooled.
func (g *SourceExclusionGate) BucketWithholding(ctx context.Context, base, quote canonical.Asset, row timescale.Vwap1mRow) Withholding {
	return g.Withholding(ctx, base, quote, row.Sources, row.Bucket, row.Bucket.Add(time.Minute))
}

// InstantWithholding is [SourceExclusionGate.Withholding] for one source's observation at ts.
func (g *SourceExclusionGate) InstantWithholding(ctx context.Context, base, quote canonical.Asset, source string, ts time.Time) Withholding {
	return g.Withholding(ctx, base, quote, []string{source}, ts, ts.Add(time.Nanosecond))
}

// CleanBuckets drops every bucket an exclusion covers; a non-NotWithheld verdict means none could be judged.
func (g *SourceExclusionGate) CleanBuckets(ctx context.Context, base, quote canonical.Asset, rows []timescale.Vwap1mRow) ([]timescale.Vwap1mRow, Withholding) {
	if g == nil {
		return rows, NotWithheld
	}
	out := rows[:0:0]
	for _, row := range rows {
		switch w := g.BucketWithholding(ctx, base, quote, row); w {
		case NotWithheld:
			out = append(out, row)
		case WithheldSourceExcluded:
		default:
			return nil, w
		}
	}
	return out, NotWithheld
}

// snapshot holds g.mu only to read or replace state. One refresh runs at a time: its caller waits for it, as does any caller
// with no usable last read; everyone else serves the last good read. Attempts are spaced by exclusionRetryInterval, so a
// down store costs one query (and one log line) per interval rather than one per request.
func (g *SourceExclusionGate) snapshot(ctx context.Context) ([]timescale.SourceExclusion, bool) {
	g.mu.Lock()
	now := g.now()
	if !g.readAt.IsZero() && now.Sub(g.readAt) < exclusionCacheTTL {
		rows := g.rows
		g.mu.Unlock()
		return rows, true
	}
	done := g.inflight
	lead := done == nil && (g.triedAt.IsZero() || now.Sub(g.triedAt) >= exclusionRetryInterval)
	if lead {
		done = make(chan struct{})
		g.inflight, g.triedAt = done, now
		go g.refresh(context.WithoutCancel(ctx), done)
	}
	usable := !g.readAt.IsZero() && now.Sub(g.readAt) < exclusionMaxStale
	g.mu.Unlock()
	if done != nil && (lead || !usable) {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.readAt.IsZero() && g.now().Sub(g.readAt) < exclusionMaxStale {
		return g.rows, true
	}
	return nil, false
}

// refresh runs one read on a context detached from the request that started it, so that request leaving does not fail
// the read every other caller is waiting on.
func (g *SourceExclusionGate) refresh(ctx context.Context, done chan struct{}) {
	var (
		rows []timescale.SourceExclusion
		err  error
	)
	// A panic must still release the flight, or every later caller would wait on it.
	defer func() {
		if r := recover(); r != nil {
			worker.Report(g.logger, "source-exclusion-refresh", r)
			err = fmt.Errorf("source exclusions read panicked: %v", r)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		now := g.now()
		g.inflight = nil
		close(done)
		switch {
		case err == nil:
			g.rows, g.readAt = rows, now
			obs.PriceSourceExclusionsActive.Set(float64(inForceCount(rows, now)))
		case !g.readAt.IsZero() && now.Sub(g.readAt) < exclusionMaxStale:
			g.logger.Warn("source exclusions unreadable; serving under the last good read", "err", err, "read_at", g.readAt)
		default:
			g.logger.Error("source exclusions unreadable; withholding prices until they can be read", "err", err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, exclusionReadTimeout)
	defer cancel()
	rows, err = g.r.UnliftedSourceExclusions(ctx)
}
