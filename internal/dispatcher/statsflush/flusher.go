// Package statsflush exports the dispatcher's cumulative per-source counters to
// decoder_stats_5m as per-interval deltas. It diffs against its own snapshot
// rather than clearing the counters, which would race concurrent decoder writes.
package statsflush

import (
	"context"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// shutdownDrainTimeout bounds the final flush so a wedged pool cannot hold shutdown open.
const shutdownDrainTimeout = 5 * time.Second

// StatsSource is the counter source; the Dispatcher satisfies it.
type StatsSource interface {
	Stats() dispatcher.Stats
}

// LedgerSource supplies last_ledger; nil leaves it NULL.
type LedgerSource interface {
	LatestLedger() uint32
}

// statsWriter is the subset of *timescale.Store the flusher uses; an interface
// so write-failure behaviour is testable without a reachable Postgres.
type statsWriter interface {
	InsertDecoderStats(ctx context.Context, rows []timescale.DecoderStatsBucket) error
}

// Flusher writes per-bucket counter deltas to decoder_stats_5m every interval.
type Flusher struct {
	source   StatsSource
	store    statsWriter
	logger   *slog.Logger
	interval time.Duration
	ledger   LedgerSource

	// Cumulative counters captured on the previous tick. Subtracting
	// from current values yields the delta for this bucket.
	last dispatcher.Stats

	// obsLast baselines the counters emitted straight to Prometheus. It advances on
	// every emission, unlike last, which holds back on a write failure; sharing
	// last would replay the same delta and WARN on every tick until a new event.
	obsLast dispatcherObsCounters
}

// dispatcherObsCounters is the baseline for the dispatcher-level
// counters promoted straight to Prometheus (see obsLast).
type dispatcherObsCounters struct {
	TxReadErrors          int
	TxEventReadErrors     int
	EntryMetaUnsupported  int
	EvictedKeysUnreadable int
	LedgerUpgradeEntries  int
	// UncorroboratedCalls is per-source, unlike its scalar siblings.
	UncorroboratedCalls map[string]int
}

// Options tunes a Flusher at construction time.
type Options struct {
	// Interval between snapshots; default 5 min, matching decoder_stats_5m.
	Interval time.Duration

	// LedgerSource is consulted on every flush so each row carries
	// the latest ingested ledger. nil leaves the column NULL.
	LedgerSource LedgerSource
}

// New constructs a Flusher; store is typically *timescale.Store.
func New(source StatsSource, store statsWriter, logger *slog.Logger, opts Options) *Flusher {
	if logger == nil {
		// Default the logger: flushAt derefs it on a background tick, so nil would panic
		// minutes after a clean start.
		logger = slog.Default()
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Flusher{
		source:   source,
		store:    store,
		logger:   logger,
		interval: interval,
		ledger:   opts.LedgerSource,
		last: dispatcher.Stats{
			EventsSeen:   map[string]int{},
			DecodeErrors: map[string]int{},
			OrphanEvents: map[string]int{},
		},
	}
}

// Run flushes every interval until ctx is cancelled; per-tick failures only log.
func (f *Flusher) Run(ctx context.Context) error {
	t := time.NewTicker(f.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush on a context detached from the cancelled ctx: database/sql would
			// reject the write otherwise and drop up to an interval of counters.
			drainCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), shutdownDrainTimeout)
			f.flush(drainCtx)
			cancel()
			return nil
		case now := <-t.C:
			f.flushAt(ctx, now)
		}
	}
}

// flush is flushAt at time.Now.
func (f *Flusher) flush(ctx context.Context) {
	f.flushAt(ctx, time.Now())
}

// flushAt writes per-source deltas since the last snapshot. The bucket is floored
// to the interval so two flushers during a leader handover write the same row.
func (f *Flusher) flushAt(ctx context.Context, now time.Time) {
	bucket := now.UTC().Truncate(f.interval)
	current := f.source.Stats()

	rows := make([]timescale.DecoderStatsBucket, 0, len(current.DecodeErrors)+len(current.OrphanEvents))
	sources := allSources(current, f.last)

	var lastLedger uint32
	if f.ledger != nil {
		lastLedger = f.ledger.LatestLedger()
	}

	for source := range sources {
		delta := timescale.DecoderStatsBucket{
			Bucket:       bucket,
			Source:       source,
			EventsSeen:   int64(current.EventsSeen[source] - f.last.EventsSeen[source]),
			DecodeErrors: int64(current.DecodeErrors[source] - f.last.DecodeErrors[source]),
			OrphanEvents: int64(current.OrphanEvents[source] - f.last.OrphanEvents[source]),
			LastLedger:   lastLedger,
		}
		// Skip all-zero rows with no ledger context.
		if delta.EventsSeen == 0 && delta.DecodeErrors == 0 && delta.OrphanEvents == 0 && delta.LastLedger == 0 {
			continue
		}
		rows = append(rows, delta)
	}

	// Tx-read errors are not attributable to a source, so they go to Prometheus
	// and a WARN, not to the per-source rows.
	if delta := current.TxReadErrors - f.obsLast.TxReadErrors; delta > 0 {
		f.logger.Warn("dispatcher: tx-read errors during this flush window",
			"delta", delta,
			"total", current.TxReadErrors,
			"window", f.interval.String(),
		)
		obs.DispatcherTxReadErrorsTotal.Add(float64(delta))
	}
	f.obsLast.TxReadErrors = current.TxReadErrors

	// Tx-event read errors drop every Soroban event in the tx, which would
	// otherwise look like a clean empty ledger that completeness still passes.
	if delta := current.TxEventReadErrors - f.obsLast.TxEventReadErrors; delta > 0 {
		f.logger.Warn("dispatcher: tx-event read errors during this flush window — Soroban events being dropped",
			"delta", delta,
			"total", current.TxEventReadErrors,
			"window", f.interval.String(),
		)
		obs.DispatcherTxEventReadErrorsTotal.Add(float64(delta))
	}
	f.obsLast.TxEventReadErrors = current.TxEventReadErrors

	// An unhandled TransactionMeta version hides every classic state change in the
	// tx, indistinguishable from an empty ledger; same WARN as above.
	if delta := current.EntryMetaUnsupported - f.obsLast.EntryMetaUnsupported; delta > 0 {
		f.logger.Warn("dispatcher: unsupported TransactionMeta version during this flush window — apply-phase entry changes being skipped",
			"delta", delta,
			"total", current.EntryMetaUnsupported,
			"window", f.interval.String(),
		)
		obs.DispatcherEntryMetaUnsupportedTotal.Add(float64(delta))
	}
	f.obsLast.EntryMetaUnsupported = current.EntryMetaUnsupported

	// An unreadable evicted-key list drops a whole ledger's state-archival
	// evictions, leaving each evicted balance served as live. The
	// dispatcher logs the ledger itself; this is the alertable series.
	if delta := current.EvictedKeysUnreadable - f.obsLast.EvictedKeysUnreadable; delta > 0 {
		f.logger.Warn("dispatcher: evicted ledger keys unreadable during this flush window — state-archival evictions being skipped",
			"delta", delta,
			"total", current.EvictedKeysUnreadable,
			"window", f.interval.String(),
		)
		obs.DispatcherEvictedKeysUnreadableTotal.Add(float64(delta))
	}
	f.obsLast.EvictedKeysUnreadable = current.EvictedKeysUnreadable

	if delta := current.LedgerUpgradeEntries - f.obsLast.LedgerUpgradeEntries; delta > 0 {
		obs.DispatcherLedgerUpgradeEntriesTotal.Add(float64(delta))
	}
	f.obsLast.LedgerUpgradeEntries = current.LedgerUpgradeEntries

	// A refused oracle corroboration is a security signal (forgery or routing
	// change), so it WARNs per source. It never touches f.last.
	for source, n := range current.UncorroboratedCalls {
		delta := n - f.obsLast.UncorroboratedCalls[source]
		if delta <= 0 {
			continue
		}
		f.logger.Warn("dispatcher: uncorroborated oracle calls during this flush window — rejected forged call or a routing-shape change",
			"source", source,
			"delta", delta,
			"total", n,
			"window", f.interval.String(),
		)
		obs.SourceUncorroboratedCallsTotal.WithLabelValues(source).Add(float64(delta))
	}
	f.obsLast.UncorroboratedCalls = copyIntMap(current.UncorroboratedCalls)

	if len(rows) > 0 {
		if err := f.store.InsertDecoderStats(ctx, rows); err != nil {
			// Keep f.last on a write failure so the next successful flush folds this
			// window in; advancing it would drop these counts forever.
			f.logger.Warn("decoder-stats flush failed — retaining last snapshot so the next successful flush recovers this window's counts",
				"rows", len(rows), "err", err)
			return
		}
	}

	// Copy the maps so concurrent dispatcher writes cannot mutate the snapshot.
	// The Prometheus-only counters stay on obsLast, decoupled from write success.
	f.last = dispatcher.Stats{
		EventsSeen:    copyIntMap(current.EventsSeen),
		DecodeErrors:  copyIntMap(current.DecodeErrors),
		OrphanEvents:  copyIntMap(current.OrphanEvents),
		UnmatchedHits: current.UnmatchedHits,
	}
}

// allSources unions current and last keys so a source whose counter returned
// to zero still gets a row.
func allSources(current, last dispatcher.Stats) map[string]struct{} {
	out := make(map[string]struct{}, len(current.EventsSeen)+len(current.DecodeErrors)+len(current.OrphanEvents))
	for k := range current.EventsSeen {
		out[k] = struct{}{}
	}
	for k := range current.DecodeErrors {
		out[k] = struct{}{}
	}
	for k := range current.OrphanEvents {
		out[k] = struct{}{}
	}
	for k := range last.EventsSeen {
		out[k] = struct{}{}
	}
	for k := range last.DecodeErrors {
		out[k] = struct{}{}
	}
	for k := range last.OrphanEvents {
		out[k] = struct{}{}
	}
	return out
}

func copyIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
