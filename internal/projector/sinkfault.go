package projector

import (
	"context"
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// QuarantineAfterCycles is the retry budget for an UNCLASSIFIED sink failure when the cycle proved
// the sink healthy (another event committed). After this many CONSECUTIVE failing cycles on one
// row identity the row is quarantined (loud log + counter) and the cursor advances past it.
//
// 20 cycles is about 100s at [Interval]: long enough for transient faults (deadlock, serialization,
// lock_not_available, statement_timeout) to clear, short enough that a poison row is not forever.
const QuarantineAfterCycles = 20

// QuarantineAfterCyclesNoProgress is the longer retry budget used when the cycle could NOT prove
// the sink healthy, so "this row is poison" and "the whole sink is broken" look identical. A
// GLOBAL fault (schema drift, permissions, wedged downstream) must be a visible stall, not shed
// rows, so the projector holds about 1 hour, past the lag / sink_retry paging alert.
const QuarantineAfterCyclesNoProgress = 720

// PermanentSkipPerCycle is the most rows ONE cycle may shed on a [dispositionSkip] verdict.
// Class 22/23 errors are not always row-local: a migration adding a NOT NULL or CHECK the live
// rows violate makes every row "poison" at once, and shedding them all would be silent loss.
//
// So the skip arm borrows both halves of the quarantine rail:
//
//   - the RATE limit: at most ONE row per cycle, lowest ledger first ([shedCandidate]); every row
//     not shed HOLDS the cursor like a retryable fault;
//   - the sink-health PROOF: [permanentSkipCandidate] takes the same `madeProgress` argument as
//     [quarantineCandidate], so a cycle that committed nothing else first waits out
//     [QuarantineAfterCyclesNoProgress].
//
// A global fault has no such proof: it becomes a visible stall, then bleeds 1 row per [Interval].
const PermanentSkipPerCycle = 1

// heldRowLogEvery throttles the per-row "holding cursor" warning to the first failing cycle and
// every Nth after, so a held row retrying every [Interval] does not bury the ERROR lines.
const heldRowLogEvery = 20

// sinkDisposition is the projector's verdict for ONE sink write failure: may the cursor advance
// past this row?
//
// It is three-valued, not a boolean: a boolean would fold "known transient" and "unclassified"
// into one retry-forever bucket, so a deterministic failure the classifier missed would hold the
// per-source cursor forever. A sole-writer domain such as sep41 would halt silently, visible only
// as growing lag.
type sinkDisposition int

const (
	// dispositionSkip is a row-local, deterministic data fault: the DB rejected the row's VALUES
	// (SQLSTATE class 22/23) or it failed a canonical value-shape check before SQL. Retrying
	// cannot succeed, so the row is skipped loudly once a cycle also proved the sink healthy, at
	// most [PermanentSkipPerCycle] per cycle because those classes also arrive globally. Without
	// that proof the wait is [QuarantineAfterCyclesNoProgress]; a row over the cap holds the cursor.
	dispositionSkip sinkDisposition = iota

	// dispositionRetry is an infrastructure or shutdown fault (Postgres down or at capacity,
	// context cancellation, cycle deadline). It is global and self-clearing, so the row is
	// retried FOREVER. Never quarantined: dropping live rows because the database is down would
	// be silent loss.
	dispositionRetry

	// dispositionUnclassified is everything else. It retries under a bounded budget
	// ([QuarantineAfterCycles] / [QuarantineAfterCyclesNoProgress]); a row that re-fails
	// identically for the whole budget is deterministic in practice and is quarantined so it
	// cannot wedge the source.
	dispositionUnclassified
)

// String renders the disposition for logs, so a held-row warning distinguishes "database down"
// from "unknown fault, budget counting down".
func (d sinkDisposition) String() string {
	switch d {
	case dispositionSkip:
		return "permanent"
	case dispositionRetry:
		return "transient"
	case dispositionUnclassified:
		return "unclassified"
	default:
		return "unknown"
	}
}

// valueShapeSentinels are the canonical errors meaning "this row's VALUES are invalid", which
// retrying cannot change. They are the pre-SQL twin of SQLSTATE class 22/23:
// `Store.InsertOracleUpdate` returns `OracleUpdate.Validate`'s error verbatim.
//
// Deliberately EXCLUDED:
//
//   - canonical.ErrUnknownAsset depends on registry state a later cycle may have warmed.
//   - canonical.ErrPairMismatch is a cross-row comparison, not a row verdict.
//
// Both fall to [dispositionUnclassified], where the retry budget bounds the damage.
var valueShapeSentinels = []error{
	canonical.ErrInvalidOracle,
	canonical.ErrInvalidTrade,
	canonical.ErrInvalidAsset,
	canonical.ErrInvalidStrkey,
	canonical.ErrInvalidAmount,
	canonical.ErrI128Overflow,
}

// classifySinkFault maps a sink write error onto its [sinkDisposition]. A nil error classifies as
// [dispositionRetry], the never-skip side.
//
// Ordering matters: positive identifications run first and unclassified is the residue, so a
// fault we understand is never absorbed by the budget arm.
func classifySinkFault(err error) sinkDisposition {
	if err == nil {
		return dispositionRetry
	}
	// Row-local + deterministic: the DB rejected these values (class 22/23) …
	if timescale.IsPermanentDataError(err) {
		return dispositionSkip
	}
	// … or our own canonical validation did, before the statement ran.
	if isValueShapeError(err) {
		return dispositionSkip
	}
	// … or the sink panicked on this event: deterministic for the event, so holding the cursor
	// would only re-run the panic. Skipped under the same shed cap + health proof.
	if errors.Is(err, pipeline.ErrSinkPanic) {
		return dispositionSkip
	}
	// Global + self-clearing: DB down/restarting/at capacity, or we are
	// shutting down / the cycle deadline fired.
	if timescale.IsInfraError(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return dispositionRetry
	}
	return dispositionUnclassified
}

// isI128Overflow reports whether a permanently dropped output was rejected because a value that
// must fit in 128 bits did not.
//
// It is skipped like the other [valueShapeSentinels], but it is NOT a verdict about an on-chain
// value: any production ErrI128Overflow means an int64 snuck onto an amount path and truncates
// every value it touches (ADR-0003). So the drop is counted under its own `outcome`, giving the
// alert (stellarindex_projector_i128_overflow) a rule to hang on.
func isI128Overflow(err error) bool {
	return errors.Is(err, canonical.ErrI128Overflow)
}

// isValueShapeError reports whether err wraps one of [valueShapeSentinels].
func isValueShapeError(err error) bool {
	for _, sentinel := range valueShapeSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// rowIdentity is the lake row's identity, the same tuple the per-source hypertables use as their
// conflict key. The retry budget is per identity (not per ledger) so a different row failing at
// the same ledger starts its own budget and a healed row's count is dropped.
type rowIdentity struct {
	ledger     uint32
	txHash     string
	opIndex    int
	eventIndex int
}

// poisonTracker counts CONSECUTIVE failing cycles per row identity for ONE source. It is
// goroutine-local (one per source loop), so it needs no locking.
//
// The map is bounded by the rows failing inside one [BatchLimit] window: [poisonTracker.retain]
// drops every identity that did not fail in the last cycle.
type poisonTracker struct {
	fails map[rowIdentity]int
}

// fail records one more consecutive failing cycle for id and returns the new
// count (1 on the first failure).
func (t *poisonTracker) fail(id rowIdentity) int {
	if t.fails == nil {
		t.fails = make(map[rowIdentity]int)
	}
	t.fails[id]++
	return t.fails[id]
}

// forget drops id's history — used when a row is quarantined, so the cursor
// moving past it cannot leave a stale count behind.
func (t *poisonTracker) forget(id rowIdentity) {
	delete(t.fails, id)
}

// retain keeps only the identities that failed in the cycle just finished. A row that succeeded
// (or left the window) restarts from zero, so the budget is a CONSECUTIVE-cycle poison detector,
// not a lifetime error count.
func (t *poisonTracker) retain(seen map[rowIdentity]bool) {
	for id := range t.fails {
		if !seen[id] {
			delete(t.fails, id)
		}
	}
}
