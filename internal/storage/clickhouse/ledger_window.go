package clickhouse

// ledgerWindowHi returns the INCLUSIVE upper bound of the window at lo over [from, to]: the last window is clamped to `to`, and `to - lo
// >= window` is tested first so lo+window cannot overflow uint32. Callers guarantee 0 < from <= to, window > 0, lo in [from, to].
func ledgerWindowHi(lo, to, window uint32) uint32 {
	if rem := to - lo; rem >= window { // window fits without uint overflow
		return lo + window - 1
	}
	return to
}

// adaptiveLedgerWindow is a walk's width over ledger_entry_changes: narrows at once on a memory-limit error, widens only after widenAfter
// clean windows (re-widening into a just-failed range would oscillate). The memory ceiling is never raised.
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

// walkLedgerWindows scans [minLedger, maxLedger] in ascending windows sized by win. A memory-limit error retries the SAME start narrower;
// any other error, or one at the floor, is returned unchanged.
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
