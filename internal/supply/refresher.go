package supply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// LedgerLookup is the storage-side primitive the [Refresher] uses
// to resolve "what's the most recent known chain ledger." Production
// impl wraps timescale.Store.ListCursors and takes the max
// last_ledger across all sources; tests pass in-memory fakes.
type LedgerLookup interface {
	LatestKnownLedger(ctx context.Context) (uint32, time.Time, error)
}

// SnapshotComputer is the supply-package primitive — computes one
// [Supply] for the given ledger. Production impl is *XLMComputer;
// classic + SEP-41 computers can plug in once they ship.
type SnapshotComputer interface {
	Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error)
}

// SnapshotInserter writes one [Supply] into the persistence layer.
// Production impl is timescale.Store.InsertSupply; idempotent on
// (asset_key, ledger_sequence).
type SnapshotInserter interface {
	InsertSupply(ctx context.Context, snap Supply) error
}

// Outcome is what one [Refresher.Tick] produced. Drives the
// aggregator's Prometheus counters; OutcomeKind is a stable
// string suitable for a metric label.
type Outcome struct {
	Kind     OutcomeKind
	Snapshot Supply // populated on OutcomeKindOK only
	Err      error  // populated on every error outcome
}

// OutcomeKind identifies a refresh outcome. Values are stable
// metric-label strings.
type OutcomeKind string

const (
	OutcomeKindOK            OutcomeKind = "ok"
	OutcomeKindNoLedger      OutcomeKind = "no_ledger"      // LedgerLookup error
	OutcomeKindNoObservation OutcomeKind = "no_observation" // ChainReader fell through with no static fallback either
	OutcomeKindComputeError  OutcomeKind = "compute_error"  // computer failed for non-observation reasons
	OutcomeKindWriteError    OutcomeKind = "write_error"    // InsertSupply failed
)

// Refresher runs one supply-snapshot cycle per [Refresher.Tick]
// call. Composes ledger resolution + computer + inserter; the
// aggregator drives it via a ticker in its own goroutine,
// mirroring the baseline-refresher shape.
type Refresher struct {
	ledgers  LedgerLookup
	computer SnapshotComputer
	inserter SnapshotInserter
	logger   *slog.Logger
}

// NewRefresher constructs the Refresher.
func NewRefresher(ledgers LedgerLookup, computer SnapshotComputer, inserter SnapshotInserter, logger *slog.Logger) *Refresher {
	return &Refresher{ledgers: ledgers, computer: computer, inserter: inserter, logger: logger}
}

// Tick runs one refresh cycle:
//
//  1. Resolve the latest known chain ledger.
//  2. Compute the supply at that ledger.
//  3. Insert the snapshot (idempotent on conflict).
//
// Returns an [Outcome] for metric emission. Tick does NOT bubble
// errors — it logs them and returns the outcome so the
// surrounding goroutine never crashes the aggregator's whole
// loop on a transient supply-side issue.
func (r *Refresher) Tick(ctx context.Context) Outcome {
	ledger, observedAt, err := r.ledgers.LatestKnownLedger(ctx)
	if err != nil {
		r.logger.Warn("supply refresh: no ledger", "err", err)
		return Outcome{Kind: OutcomeKindNoLedger, Err: err}
	}

	snap, err := r.computer.Compute(ctx, ledger, observedAt)
	if err != nil {
		// Distinguish the "no observation" outcome (which the
		// ChainReader surfaces with ErrNoObservation when both live
		// AND static fall through) from generic compute errors so
		// operators can chart the bootstrap-progress signal.
		kind := OutcomeKindComputeError
		if errors.Is(err, ErrNoObservation) {
			kind = OutcomeKindNoObservation
		}
		r.logger.Warn("supply refresh: compute failed",
			"err", err, "ledger", ledger, "kind", string(kind))
		return Outcome{Kind: kind, Err: err}
	}

	if err := r.inserter.InsertSupply(ctx, snap); err != nil {
		r.logger.Error("supply refresh: insert failed",
			"err", err, "asset", snap.AssetKey, "ledger", snap.LedgerSequence)
		return Outcome{Kind: OutcomeKindWriteError, Err: err, Snapshot: snap}
	}

	r.logger.Debug("supply refresh ok",
		"asset", snap.AssetKey,
		"ledger", snap.LedgerSequence,
		"circulating", snap.CirculatingSupply.String())
	return Outcome{Kind: OutcomeKindOK, Snapshot: snap}
}

// String renders the outcome for log lines / test fixtures. Stable
// across versions.
func (o Outcome) String() string {
	if o.Err != nil {
		return fmt.Sprintf("%s: %v", o.Kind, o.Err)
	}
	return string(o.Kind)
}
