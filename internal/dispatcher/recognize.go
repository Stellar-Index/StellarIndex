package dispatcher

import "github.com/Stellar-Index/StellarIndex/internal/events"

// Recognize reports whether any registered event decoder claims this event, returning its name.
// It runs the same Matches() predicates as the live dispatch walk, with no decode and no side
// effects (eventsSeen / unmatchedHits untouched). It is the oracle for ADR-0033 Claim 2a: a
// (contract_id, topic) shape in soroban_events that returns false is an on-chain event the system
// would silently drop. ContractCallDecoders are excluded; they emit no Soroban events.
//
// Matches() proves only the topic shape is owned. Calling Decode here would corrupt the
// correlation state (Soroswap swap+sync, Phoenix 8-field) of the live decoder instances, so a
// matched decoder may implement [Validator] for a side-effect-free check; stateful decoders don't.
func (d *Dispatcher) Recognize(ev events.Event) (name string, ok bool) {
	// Matches runs on adversary-influenced ledger data and can panic like Decode, and Recognize's
	// callers do not recover, so one malformed row would take down the whole verification run.
	// A panic resolves to NOT RECOGNISED (fail closed): the shape counts as an unrecognised event
	// and the recognition axis goes RED, rather than certifying a shape nobody can decode.
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
		if !dec.Matches(ev) {
			continue
		}
		if v, ok := dec.(Validator); ok {
			if err := v.Validate(ev); err != nil {
				// dispatchOne commits to the first matching decoder and never falls through on
				// decode failure, so Recognize reports a gap, not a later decoder's name.
				return "", false
			}
		}
		return dec.Name(), true
	}
	return "", false
}

// Validator is an optional [Decoder] interface letting Recognize verify that a matched sample
// would actually decode. Validate must be side-effect-free and independent of cross-event
// correlation state; decoders with such state (Soroswap, Phoenix) must not implement it, since
// Recognize runs against the live decoder instances.
type Validator interface {
	Validate(ev events.Event) error
}
