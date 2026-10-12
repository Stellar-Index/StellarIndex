package projector

import (
	"context"
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// QuarantineAfterCycles is the retry budget for an UNCLASSIFIED sink failure when the cycle proved the sink healthy: after this many CONSECUTIVE failing cycles (about 100s at [Interval]) the row is quarantined (loud log + counter) and the cursor advances.
const QuarantineAfterCycles = 20

// QuarantineAfterCyclesNoProgress is the longer budget (about 1 hour, past the lag / sink_retry paging alert) when the cycle could NOT prove the sink healthy, so a GLOBAL fault (schema drift, permissions, wedged downstream) is a visible stall, not shed rows.
const QuarantineAfterCyclesNoProgress = 720

// PermanentSkipPerCycle is the most rows ONE cycle may shed on a [dispositionSkip] verdict (lowest ledger first, the rest HOLD the cursor), and only with sink-health proof as in [quarantineCandidate], because a migration can make every row look poison and shedding them all would be silent loss.
const PermanentSkipPerCycle = 1

// heldRowLogEvery throttles the per-row "holding cursor" warning to the first failing cycle and
// every Nth after, so a held row retrying every [Interval] does not bury the ERROR lines.
const heldRowLogEvery = 20

// sinkDisposition is the verdict for ONE sink write failure: may the cursor advance past this row? Three-valued, not a boolean: folding
// "known transient" and "unclassified" into one retry-forever bucket would let a missed deterministic failure hold a per-source cursor
// forever and halt a sole-writer domain such as sep41, visible only as lag.
type sinkDisposition int

const (
	// dispositionSkip is a row-local, deterministic data fault: the DB rejected the row's VALUES (SQLSTATE class 22/23) or it failed a
	// canonical value-shape check before SQL. It is skipped loudly once a cycle proved the sink healthy, at most [PermanentSkipPerCycle] per
	// cycle since those classes also arrive globally; otherwise it waits [QuarantineAfterCyclesNoProgress].
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

// String renders the disposition for logs, so a held-row warning distinguishes "database down" from "unknown fault, budget counting down".
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

// valueShapeSentinels are the canonical errors meaning "this row's VALUES are invalid" (the pre-SQL twin of SQLSTATE class 22/23).
// Deliberately EXCLUDED: canonical.ErrUnknownAsset (registry state a later cycle may have warmed) and canonical.ErrPairMismatch (a
// cross-row comparison); both fall to [dispositionUnclassified], where the retry budget bounds the damage.
var valueShapeSentinels = []error{
	canonical.ErrInvalidOracle,
	canonical.ErrInvalidTrade,
	canonical.ErrInvalidAsset,
	canonical.ErrInvalidStrkey,
	canonical.ErrInvalidAmount,
	canonical.ErrI128Overflow,
}

// classifySinkFault maps a sink write error onto its [sinkDisposition]; nil classifies as [dispositionRetry], the never-skip side.
// Positive identifications run first and unclassified is the residue, so a fault we understand is never absorbed by the budget arm.
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
// value: any production ErrI128Overflow means an int64 snuck onto an amount path (ADR-0003), so
// the drop is counted under its own `outcome` for the stellarindex_projector_i128_overflow alert.
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

// poisonTracker counts CONSECUTIVE failing cycles per row identity for ONE source; goroutine-local, no locking. It is bounded by the rows
// failing inside one [BatchLimit] window: [poisonTracker.retain] drops every identity that did not fail in the last cycle.
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
