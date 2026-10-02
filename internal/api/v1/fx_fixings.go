package v1

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// FXFixingReader binds a closed bucket's FX leg to the vendor's time series
// (fx_fixings, ADR-0018). *timescale.Store satisfies it.
type FXFixingReader interface {
	FXFixingAtOrBefore(ctx context.Context, tickers []string, e time.Time, maxAge time.Duration) (map[string]timescale.FXFixingBinding, error)
	LoadFXFixingWindow(ctx context.Context, horizon time.Duration, ingestedAfter time.Time) ([]timescale.FXFixing, time.Time, error)
}

// errFXLegMissing is a lookback miss: no fixing binds the ticker at E.
var errFXLegMissing = errors.New("fx fixing: no rate binds at the bucket end")

// fxFixingStaleAfter is how far a bound hourly bar may trail E − lag before
// the derived price reports stale outside the weekend market close.
const fxFixingStaleAfter = 4 * time.Hour

// fxFixingCache holds the recent fixings window in memory so a closed cross
// costs no round trip. It answers E only when the answer equals the store's
// at the last load L; anything else reads the store.
type fxFixingCache struct {
	reader FXFixingReader
	logger *slog.Logger
	maxAge time.Duration

	mu       sync.Mutex
	rows     map[string][]timescale.FXFixing
	loadedAt time.Time // L: the last load's statement_timestamp
	floor    time.Time // every row with bar_end ≥ floor is held
	flight   chan struct{}
}

func newFXFixingCache(reader FXFixingReader, logger *slog.Logger, maxAge time.Duration) *fxFixingCache {
	if reader == nil {
		return nil
	}
	return &fxFixingCache{reader: reader, logger: logger, maxAge: maxAge}
}

func (c *fxFixingCache) horizon() time.Duration {
	return c.maxAge + timescale.FXFixingLag + 24*time.Hour
}

// bind returns every ticker's binding at bucket end e, or errFXLegMissing
// when any one has none.
func (c *fxFixingCache) bind(ctx context.Context, tickers []string, e time.Time) (map[string]timescale.FXFixingBinding, error) {
	if out, ok := c.fromMemory(tickers, e); ok { //nolint:contextcheck // the refresh it may start must outlive this request
		return out, nil
	}
	out, err := c.reader.FXFixingAtOrBefore(ctx, tickers, e, c.maxAge)
	if err != nil {
		return nil, err
	}
	for _, t := range tickers {
		if _, ok := out[t]; !ok {
			return nil, errFXLegMissing
		}
	}
	return out, nil
}

// fromMemory answers only when E ≤ L and E's whole lookback lies above the
// held floor; a ticker with no held row reads the store, which owns the
// daily arm and the era-gap decision.
func (c *fxFixingCache) fromMemory(tickers []string, e time.Time) (map[string]timescale.FXFixingBinding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loadedAt.IsZero() || e.After(c.loadedAt) {
		c.refreshLocked()
		return nil, false
	}
	cutoff := e.Add(-timescale.FXFixingLag)
	if cutoff.Add(-c.maxAge).Before(c.floor) {
		return nil, false
	}
	out := make(map[string]timescale.FXFixingBinding, len(tickers))
	for _, t := range tickers {
		f, ok := selectFixing(c.rows[t], cutoff, c.maxAge)
		if !ok {
			return nil, false
		}
		out[t] = timescale.FXFixingBinding{FXFixing: f, Resolution: fxResolutionOf(f.Grain)}
	}
	return out, true
}

// refreshLocked starts one detached delta load unless one is in flight.
func (c *fxFixingCache) refreshLocked() {
	if c.flight != nil {
		return
	}
	done := make(chan struct{})
	c.flight = done
	var after time.Time
	if !c.loadedAt.IsZero() {
		after = c.loadedAt.Add(-timescale.FXFixingIngestSlack)
	}
	type load struct {
		rows []timescale.FXFixing
		at   time.Time
	}
	upstream := func(ctx context.Context) (load, error) {
		rows, at, err := c.reader.LoadFXFixingWindow(ctx, c.horizon(), after)
		return load{rows: rows, at: at}, err
	}
	settle := func(l load, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.flight = nil
		if err != nil {
			c.logger.Warn("fx fixings cache load failed", "err", err)
			return
		}
		c.mergeLocked(l.rows, l.at)
	}
	go runDetachedFill(c.logger, "api-fx-fixings-refresh", cacheFillBudget, done, upstream, settle)
}

func (c *fxFixingCache) mergeLocked(rows []timescale.FXFixing, at time.Time) {
	if c.rows == nil {
		c.rows = make(map[string][]timescale.FXFixing)
	}
	floor := at.Add(-c.horizon())
	for _, f := range rows {
		held := c.rows[f.Ticker]
		dup := false
		for _, h := range held {
			if h.Grain == f.Grain && h.BarStart.Equal(f.BarStart) && h.Generation == f.Generation {
				dup = true
				break
			}
		}
		if !dup {
			c.rows[f.Ticker] = append(held, f)
		}
	}
	for t, held := range c.rows {
		kept := held[:0]
		for _, h := range held {
			if !h.BarEnd.Before(floor) {
				kept = append(kept, h)
			}
		}
		c.rows[t] = kept
	}
	c.loadedAt, c.floor = at, floor
}

// selectFixing is the store's binding rule (timescale.FXFixingAtOrBefore)
// over held rows: the greatest bar_end in (cutoff − maxAge, cutoff], then
// grain '1h' over '1d', then the highest generation.
func selectFixing(rows []timescale.FXFixing, cutoff time.Time, maxAge time.Duration) (timescale.FXFixing, bool) {
	var best timescale.FXFixing
	found := false
	lower := cutoff.Add(-maxAge)
	for _, r := range rows {
		if r.BarEnd.After(cutoff) || !r.BarEnd.After(lower) {
			continue
		}
		if !found || fixingOutranks(r, best) {
			best, found = r, true
		}
	}
	return best, found
}

func fixingOutranks(a, b timescale.FXFixing) bool {
	if !a.BarEnd.Equal(b.BarEnd) {
		return a.BarEnd.After(b.BarEnd)
	}
	if a.Grain != b.Grain {
		return a.Grain > b.Grain
	}
	return a.Generation > b.Generation
}

func fxResolutionOf(grain string) string {
	if grain == timescale.FXGrainDay {
		return timescale.FXResolutionDaily
	}
	return timescale.FXResolutionHourly
}

// bindFXFixings binds tickers at closed bucket end e. Absent a reader the
// closed surfaces have no FX leg: they never fall back to the live snapshot.
func (s *Server) bindFXFixings(ctx context.Context, tickers []string, e time.Time) (map[string]timescale.FXFixingBinding, error) {
	if s.fxFixings == nil {
		return nil, errFXLegMissing
	}
	return s.fxFixings.bind(ctx, tickers, e)
}

// fixingRate parses a bound fixing's NUMERIC text exactly.
func fixingRate(b timescale.FXFixingBinding) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(b.RateUSD)
	if !ok || r.Sign() <= 0 {
		return nil, false
	}
	return r, true
}

// fxFixingStale reports whether an hourly fixing bound at e trails e − lag
// by more than fxFixingStaleAfter outside the weekend close
// [Fri 22:00Z, Mon 02:00Z). A pure function of e, so every region agrees.
// Daily fixings carry no FX staleness of their own.
func fxFixingStale(e time.Time, b timescale.FXFixingBinding) bool {
	if b.Resolution != timescale.FXResolutionHourly {
		return false
	}
	cutoff := e.Add(-timescale.FXFixingLag).UTC()
	if cutoff.Sub(b.BarEnd) <= fxFixingStaleAfter {
		return false
	}
	return !fxWeekendClose(cutoff)
}

func fxWeekendClose(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return true
	case time.Friday:
		return t.Hour() >= 22
	case time.Monday:
		return t.Hour() < 2
	case time.Tuesday, time.Wednesday, time.Thursday:
		return false
	}
	return false
}
