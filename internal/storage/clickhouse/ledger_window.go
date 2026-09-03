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
