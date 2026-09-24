package clickhouse

// ledgerWindowHi returns the INCLUSIVE upper bound of the ledger window that
// starts at lo, for a walk over [from, to] in windows of `window` ledgers.
//
// It is the one definition of the walk step shared by every windowed,
// resumable lake job in this package — BackfillTxHashIndex,
// BackfillContractActiveLedgers, BackfillContractInstanceChanges and
// BackfillOperationParticipants. All four ran a byte-identical copy of this
// arithmetic inline; folding them onto one helper is what lets the step be
// tested once (ledger_window_test.go) instead of four times or, as was the
// case, never.
//
// The caller's loop is always:
//
//	for lo := from; ; {
//	    hi := ledgerWindowHi(lo, to, window)
//	    ... do the window ...
//	    if hi >= to { return }
//	    lo = hi + 1
//	}
//
// Two properties the arithmetic exists to hold, both of which a naive
// `hi := lo + window - 1` breaks:
//
//   - The FINAL window is never dropped and never over-runs `to`. When fewer
//     than `window` ledgers remain, hi is clamped to `to`, so a range that is
//     not a whole multiple of the window still ends on a short window rather
//     than reading past the tip (or, with a `hi > to` guard bolted on later,
//     silently skipping the remainder).
//   - No uint32 overflow. `lo + window - 1` wraps when the window runs off the
//     end of the ledger-sequence space; testing the REMAINING span
//     (`to - lo >= window`) first means the addition is only ever performed
//     when its result provably fits.
//
// Callers guarantee 0 < from <= to and window > 0 (each validates its own
// arguments and returns a usage error); lo is always within [from, to].
func ledgerWindowHi(lo, to, window uint32) uint32 {
	if rem := to - lo; rem >= window { // window fits without uint overflow
		return lo + window - 1
	}
	return to
}

// adaptiveLedgerWindow carries the ledger-window width for a windowed walk of
// stellar.ledger_entry_changes: narrow on a ClickHouse memory-limit error,
// widen again after sustained success. Every windowed append-log seed walks
// through it so no walk can regress to monotonic narrowing, where one dense
// range pins the window at its floor for the rest of the chain.
//
// The asymmetry is deliberate. Narrowing is immediate (one failure halves the
// width) because a failed window is a wasted scan; widening needs widenAfter
// consecutive clean windows because re-widening into a range that just failed
// would oscillate. The per-query memory ceiling is never raised — only the
// amount of chain per query changes.
type adaptiveLedgerWindow struct {
	initial    uint32
	floor      uint32
	widenAfter int

	width uint32
	clean int // consecutive scans with no memory-limit error
}

func newAdaptiveLedgerWindow(initial, floor uint32, widenAfter int) *adaptiveLedgerWindow {
	return &adaptiveLedgerWindow{initial: initial, floor: floor, widenAfter: widenAfter, width: initial}
}

func (w *adaptiveLedgerWindow) canNarrow() bool {
	return w.width > w.floor
}

func (w *adaptiveLedgerWindow) narrow() {
	w.width /= 2
	if w.width < w.floor {
		w.width = w.floor
	}
	w.clean = 0
}

func (w *adaptiveLedgerWindow) succeeded() {
	w.clean++
	if w.clean < w.widenAfter || w.width >= w.initial {
		return
	}
	w.width *= 2
	if w.width > w.initial {
		w.width = w.initial
	}
	w.clean = 0
}

// walkLedgerWindows calls scan over [minLedger, maxLedger] in ascending,
// non-overlapping windows sized by win. A memory-limit error retries the SAME
// start narrower until win's floor; any other error, or a memory-limit error at
// the floor, is returned unchanged.
func walkLedgerWindows(minLedger, maxLedger uint32, win *adaptiveLedgerWindow, scan func(from, to uint32) error) error {
	for start := minLedger; ; {
		end := ledgerWindowHi(start, maxLedger, win.width)
		switch err := scan(start, end); {
		case err == nil:
			win.succeeded()
		case isMemoryLimitExceeded(err) && win.canNarrow():
			win.narrow()
			continue
		default:
			return err
		}
		if end >= maxLedger {
			return nil
		}
		start = end + 1
	}
}
