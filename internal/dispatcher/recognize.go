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
//
// Matches() alone proves the topic *shape* is owned, not that this
// specific sample would decode — a decoder can match on
// (contract_id, topic[0]) and still fail deeper SCVal parsing (RLT-137
// / #608 "recognition proves less than every event shape"). Recognize
// cannot close that gap by calling Decode directly: d.decoders are the
// SAME instances the live pipeline runs, and decoders with correlation
// state (Soroswap swap+sync, Phoenix 8-field) would have that state
// corrupted by an out-of-band dry-run Decode. Instead a matched
// decoder may optionally implement [Validator] to opt into a
// side-effect-free check of this exact sample; stateful decoders
// simply don't implement it and keep today's shape-only behavior.
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
		if !dec.Matches(ev) {
			continue
		}
		if v, ok := dec.(Validator); ok {
			if err := v.Validate(ev); err != nil {
				continue
			}
		}
		return dec.Name(), true
	}
	return "", false
}

// Validator is an OPTIONAL interface a [Decoder] implements to let
// Recognize verify that a specific matched sample would actually
// decode, not merely that its topic shape matches. Validate MUST be
// side-effect-free and independent of any correlation state Decode
// carries across events — a decoder with such state (Soroswap
// swap+sync, Phoenix 8-field) must not implement this, since Recognize
// runs against the SAME decoder instances the live pipeline uses.
type Validator interface {
	Validate(ev events.Event) error
}
