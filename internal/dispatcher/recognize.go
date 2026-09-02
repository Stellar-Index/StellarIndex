package dispatcher

import "github.com/Stellar-Index/StellarIndex/internal/events"

// Recognize reports whether any registered event decoder claims this
// event, returning the matching decoder's name. It runs the SAME
// Matches() predicates the live dispatch walk uses, but with NO decode
// and NO side effects (it does not touch eventsSeen / unmatchedHits).
//
// This is the oracle for ADR-0033 Claim 2a (recognition): feed it the
// distinct (contract_id, topic) shapes actually present in
// soroban_events and any shape it returns false for is an on-chain
// event the system would silently drop — a recognition gap. Because it
// uses the real Matches() logic rather than a hand-maintained topic
// list, it cannot drift from what the decoders actually handle.
//
// ContractCallDecoders are intentionally excluded: they bind to
// InvokeContract op args, emit no Soroban events, and so never produce
// soroban_events rows to recognize.
func (d *Dispatcher) Recognize(ev events.Event) (name string, ok bool) {
	// Decoder-panic guard (#371 F1, ops path). Matches is arbitrary source
	// code running on adversary-influenced ledger data — it type-asserts
	// topic vectors and reads body fields — so it panics as readily as
	// Decode. Recognize is called from the completeness recogniser and two
	// ops subcommands, none of which recovered, so one malformed row took
	// the whole verification run down.
	//
	// A panic here resolves to NOT RECOGNISED, which is the fail-closed
	// direction: the shape is then counted as an unrecognised event on an
	// unowned contract and the recognition axis goes RED. Reporting the
	// decoder as the owner would have been the dangerous answer — it would
	// certify a shape nobody can actually decode.
	var current string
	defer func() {
		if r := recover(); r != nil {
			_ = d.recordDecoderPanic(current, false, r,
				panicSite{Ledger: ev.Ledger, TxHash: ev.TxHash, OpIndex: ev.OperationIndex})
			name, ok = "", false
		}
	}()
	for _, dec := range d.decoders {
		current = dec.Name()
		if dec.Matches(ev) {
			return dec.Name(), true
		}
	}
	return "", false
}
