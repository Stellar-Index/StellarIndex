package projector

import (
	"context"
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// QuarantineAfterCycles is the retry budget for an UNCLASSIFIED sink failure
// on a row the cycle proved the sink is otherwise healthy for (at least one
// other event durably committed in the same cycle). After this many
// CONSECUTIVE cycles failing on the identical row identity the projector
// declares the row deterministically un-processable, quarantines it (loud log
// + counter) and lets the cursor advance past it.
//
// 20 cycles ≈ 100s at [Interval] — long enough for every genuinely transient
// non-infra fault we have observed (deadlock 40P01, serialization 40001,
// lock_not_available 55P03, statement_timeout 57014) to clear on a retry,
// short enough that a poison row costs the source ~2 minutes rather than
// forever (COR-11 / COR-01, audit-2026-07-23).
const QuarantineAfterCycles = 20

// QuarantineAfterCyclesNoProgress is the (much longer) retry budget that
// applies when the cycle could NOT prove the sink is healthy — no other event
// committed, so "this row is poison" and "the whole sink is broken" look
// identical from here. A GLOBAL fault (schema drift after a bad deploy, a
// permissions change, a wedged downstream) must NOT be answered by shedding
// rows: the correct response is a visible stall while an operator fixes it.
// So the projector holds for ~1 hour of cycles first, which is far longer than
// the paging alert on projector lag / sink_retry takes to fire, and only then
// gives up one row per cycle. Sparse sources whose window contains a single
// (poison) event therefore still self-heal — just slowly, and loudly.
const QuarantineAfterCyclesNoProgress = 720

// PermanentSkipPerCycle is the most rows ONE cycle may shed on a
// [dispositionSkip] verdict — a SQLSTATE class 22/23 rejection or a canonical
// value-shape rejection raised before the statement ran.
//
// The verdict is positively identified and deterministic, so a shed row needs
// no retry budget: what it needs is a RATE limit. IsPermanentDataError is true
// for ANY class 22/23 error, and those classes are not always row-local — a
// migration that adds a NOT NULL or a CHECK the live rows violate makes every
// row of the window "poison" at once. Shedding them all on the first cycle
// (what this arm did before RLT-131) turns a global, fixable fault into an
// immediate unbounded loss: the cursor sails past the whole backlog, and the
// raw events stay in the lake but nothing in the served tier says which rows
// went missing.
//
// So the skip arm borrows BOTH halves of the quarantine arm's rail:
//
//   - the RATE limit — at most ONE row per cycle, lowest ledger first
//     ([shedCandidate]) — and every row it did not shed HOLDS the cursor
//     exactly like a retryable fault;
//   - the sink-health PROOF — [permanentSkipCandidate] takes the same
//     `madeProgress` argument [quarantineCandidate] does, so a verdict
//     returned by a cycle that committed nothing else waits out
//     [QuarantineAfterCyclesNoProgress] first.
//
// A genuine scattered poison row sits beside healthy rows, so the proof is
// there and it still costs one cycle. A global fault has no proof by
// construction, so it turns into a ~1 hour visible stall — lag climbing,
// runs_total{outcome="sink_retry"} ticking — and only then bleeds at 1 row per
// [Interval] with one loud ERROR per row, instead of a silent backlog-wide
// drop.
const PermanentSkipPerCycle = 1

// heldRowLogEvery throttles the per-row "holding cursor" warning to the
// first failing cycle and every Nth after, mirroring the sink's own
// infraRetryLogEvery: a held row is retried every [Interval] for as long as
// the fault lasts (forever, for an infra fault), and one line per row per
// cycle turned a sustained outage into thousands of identical warnings that
// buried the ERROR lines an operator actually needs. The cycle-level
// signals (runs_total{outcome="sink_retry"}, the per-cycle held-progress
// warning, lag) are unaffected and remain the paging surface.
const heldRowLogEvery = 20

// sinkDisposition is the projector's durability verdict for ONE sink write
// failure. It answers exactly one question: may the cursor advance past this
// row?
//
// The taxonomy is deliberately three-valued rather than the boolean
// permanent/transient split that wedged the sole-writer sources (COR-11 /
// COR-01, audit-2026-07-23). The boolean collapsed "positively known to be
// transient" and "we could not classify it" into the same retry-forever
// bucket, so any DETERMINISTIC failure the classifier did not recognise — a
// pre-SQL store validation error such as `InsertSEP41TransferBatch: row 0
// transfer negative Amount -1`, or an `OracleUpdate.Validate` rejection — held
// the per-source cursor forever. Under INV-4 (one writer per Soroban-derived
// domain) there is no second writer to make progress, so one hostile or
// malformed on-chain value halted the whole domain, silently, visible only as
// growing lag.
type sinkDisposition int

const (
	// dispositionSkip — a POSITIVELY-identified, row-local, deterministic data
	// fault: the database rejected the row's VALUES (SQLSTATE class 22/23) or
	// the row failed a canonical value-shape check before it ever reached SQL.
	// Retrying can never succeed, so the row is counted, logged loudly and
	// skipped — on the FIRST cycle when the cycle also PROVED the sink is
	// otherwise healthy, and at most [PermanentSkipPerCycle] rows of one
	// cycle, because the same SQLSTATE classes also arrive globally
	// (RLT-131). Without that proof the wait is
	// [QuarantineAfterCyclesNoProgress]. A row over the cap, or short of the
	// budget, holds the cursor and is shed by a later cycle.
	dispositionSkip sinkDisposition = iota

	// dispositionRetry — a POSITIVELY-identified infrastructure / shutdown
	// fault (Postgres unreachable, restarting, out of capacity; context
	// cancellation; the cycle deadline). The fault is global, not row-local,
	// and it clears on its own, so the cursor is held below the row and the
	// row is retried FOREVER. Never quarantined: dropping live rows because
	// the database is down is the C2-1 silent-loss bug, not a fix for it.
	dispositionRetry

	// dispositionUnclassified — everything else. Retried like
	// [dispositionRetry], but under a bounded budget
	// ([QuarantineAfterCycles] / [QuarantineAfterCyclesNoProgress]): a row
	// that re-fails identically for the whole budget is deterministic in
	// practice whatever its error type, and is quarantined so it cannot wedge
	// the source. This is the arm that makes the fix robust to error types
	// nobody has enumerated yet — a NEW deterministic failure mode lands here
	// and self-heals instead of stalling the domain indefinitely.
	dispositionUnclassified
)

// String renders the disposition for structured logs, so an operator reading
// a held-row warning can tell "the database is down" from "we don't know what
// this is and the budget is counting down".
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

// valueShapeSentinels are the canonical error sentinels that mean "this row's
// VALUES are invalid" — a verdict about the data itself, which no amount of
// retrying changes. They are the pre-SQL twin of SQLSTATE class 22/23: e.g.
// `Store.InsertOracleUpdate` returns `OracleUpdate.Validate`'s error verbatim,
// so a live oracle source publishing a self-priced or out-of-range update
// surfaces here rather than as a *pgconn.PgError.
//
// Deliberately EXCLUDED, though they live in the same package:
//
//   - canonical.ErrUnknownAsset — depends on registry/catalogue state a later
//     cycle may have warmed, so it is not row-deterministic.
//   - canonical.ErrPairMismatch — a cross-row comparison, not a row verdict.
//
// Both fall to [dispositionUnclassified], where the retry budget still bounds
// the damage without the classifier asserting more than it knows.
var valueShapeSentinels = []error{
	canonical.ErrInvalidOracle,
	canonical.ErrInvalidTrade,
	canonical.ErrInvalidAsset,
	canonical.ErrInvalidStrkey,
	canonical.ErrInvalidAmount,
	canonical.ErrI128Overflow,
}

// classifySinkFault maps a sink write error onto its [sinkDisposition].
// err must be non-nil (callers only classify an actual failure); a nil error
// classifies as [dispositionRetry], the never-skip side.
//
// Ordering matters: the positive identifications run first, and the
// unclassified bucket is the residue — so a fault we DO understand is never
// silently absorbed by the budget arm.
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
	// … or the sink itself panicked on this event. Deterministic for the
	// event, and the sink's own classifier already drops it; holding the
	// cursor would re-run the panicking decode every cycle for the whole
	// budget. Skipped under the same shed cap + health proof as the rest.
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

// isI128Overflow reports whether a permanently-dropped output was rejected
// because a value that must fit in 128 bits did not.
//
// It is one of [valueShapeSentinels] and is skipped like the rest, but it is
// NOT a verdict about an on-chain value: ADR-0003 (§Operational impact) reads
// "any observed errors.Is(err, canonical.ErrI128Overflow) in production fires
// a SEV-1 — it indicates an int64 sneaking in somewhere". The bug is ours, and
// an int64 on an amount path silently truncates every value it touches, not
// just the one that tripped the check. So the drop is counted under its own
// `outcome` rather than folded into the ordinary class-22/23 count, which is
// what gives that promise a rule to hang on
// (stellarindex_projector_i128_overflow, RLT-131).
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

// rowIdentity is the lake row's identity — the same tuple the per-source
// hypertables use as their conflict key. The retry budget is counted per
// identity (not per ledger) so a DIFFERENT row failing at the same ledger
// starts its own budget, and a healed row's count is dropped entirely.
type rowIdentity struct {
	ledger     uint32
	txHash     string
	opIndex    int
	eventIndex int
}

// poisonTracker counts CONSECUTIVE failing cycles per row identity for ONE
// source. Goroutine-local — one per source loop, same ownership model as the
// adaptive window in runOneSource — so it needs no locking.
//
// The map is bounded by the number of rows currently failing inside a single
// [BatchLimit] window: [poisonTracker.retain] drops every identity that did not
// fail in the cycle just finished, so a transient blip cannot accumulate.
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

// retain keeps only the identities that failed in the cycle just finished.
// A row that succeeded (or that the window no longer covers) therefore starts
// from zero if it ever fails again — the budget is CONSECUTIVE-cycle based,
// which is what makes it a poison detector rather than a lifetime error count.
func (t *poisonTracker) retain(seen map[rowIdentity]bool) {
	for id := range t.fails {
		if !seen[id] {
			delete(t.fails, id)
		}
	}
}
