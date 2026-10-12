package dispatcher

import "github.com/Stellar-Index/StellarIndex/internal/events"

// Recognize reports whether any registered event decoder claims ev and returns its name; it runs only Matches (no Decode, no side effects) so live correlation state is never corrupted, and a false result means an on-chain event the system would silently drop (ADR-0033 Claim 2a).
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

// Validator is an optional [Decoder] interface letting Recognize verify a matched sample would decode; Validate MUST be side-effect-free and independent of cross-event state, so stateful decoders must not implement it.
type Validator interface {
	Validate(ev events.Event) error
}
