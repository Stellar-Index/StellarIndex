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
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SourceExclusionReader is the store seam the gate reads; *timescale.Store satisfies it.
type SourceExclusionReader interface {
	UnliftedSourceExclusions(ctx context.Context) ([]timescale.SourceExclusion, error)
}

const (
	exclusionCacheTTL = 15 * time.Second
	exclusionMaxStale = time.Minute
)

// SourceExclusionGate answers whether a served value depends on an excluded source. A nil gate withholds nothing.
type SourceExclusionGate struct {
	r      SourceExclusionReader
	logger *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	rows   []timescale.SourceExclusion
	readAt time.Time // zero until the first successful read
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

func (g *SourceExclusionGate) snapshot(ctx context.Context) ([]timescale.SourceExclusion, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if !g.readAt.IsZero() && now.Sub(g.readAt) < exclusionCacheTTL {
		return g.rows, true
	}
	rows, err := g.r.UnliftedSourceExclusions(ctx)
	if err == nil {
		g.rows, g.readAt = rows, now
		return rows, true
	}
	if !g.readAt.IsZero() && now.Sub(g.readAt) < exclusionMaxStale {
		g.logger.Warn("source exclusions unreadable; serving under the last good read", "err", err, "read_at", g.readAt)
		return g.rows, true
	}
	g.logger.Error("source exclusions unreadable; withholding prices until they can be read", "err", err)
	return nil, false
}
