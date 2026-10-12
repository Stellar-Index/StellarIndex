package reflector

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of one Reflector variant (DEX / CEX / FX), scoped to that variant's mainnet contract; stateless. Per-entry decode errors (unknown ticker, malformed payload) are non-fatal and counted by the dispatcher.
type Decoder struct {
	variant    Variant
	contractID string
	decimals   uint8
	observer   string
}

// NewDecoder constructs a Decoder bound to one variant; contractID scopes the matcher so a DEX Decoder won't decode a CEX event (fiat vs crypto assets) despite the shared REFLECTOR:update topic.
func NewDecoder(variant Variant, contractID string, opts ...DecoderOption) *Decoder {
	d := &Decoder{
		variant:    variant,
		contractID: contractID,
		decimals:   DefaultDecimals,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// DecoderOption configures a Decoder at construction.
type DecoderOption func(*Decoder)

// WithDecoderObserver stamps an observer strkey (typically the relayer tx source) on every emitted OracleUpdate; blank leaves the Observer column empty, which is valid.
func WithDecoderObserver(obs string) DecoderOption {
	return func(r *Decoder) { r.observer = obs }
}

// WithDecoderDecimals sets the power-of-10 scale stamped on every emitted OracleUpdate; 0 keeps [DefaultDecimals].
func WithDecoderDecimals(dec uint8) DecoderOption {
	return func(r *Decoder) {
		if dec != 0 {
			r.decimals = dec
		}
	}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return d.variant.SourceName() }

// Matches implements [dispatcher.Decoder]: events from this Decoder's contract whose topic[0..1] are Symbol("REFLECTOR"), Symbol("update"); classify() checks arity (topic[2]).
func (d *Decoder) Matches(ev events.Event) bool {
	if ev.ContractID != d.contractID {
		return false
	}
	return classify(&ev)
}

// Decode implements [dispatcher.Decoder], returning one UpdateEvent per (asset, price) entry in update_data after zero-price and unknown-symbol entries are skipped (see decodeUpdate).
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		// Fail closed rather than substituting time.Now(): closedAt is decodeUpdate's fallback
		// when the topic[2] timestamp is missing or out of window, and wall-clock time would
		// mis-timestamp the row during a backfill replay.
		return nil, err
	}
	updates, err := decodeUpdate(&ev, d.variant, d.decimals, d.observer, closedAt)
	if err != nil {
		return nil, err
	}
	out := make([]consumer.Event, 0, len(updates))
	for _, u := range updates {
		out = append(out, UpdateEvent{Update: u})
	}
	return out, nil
}
