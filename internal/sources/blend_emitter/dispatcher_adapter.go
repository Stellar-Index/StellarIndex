package blend_emitter

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the Blend Emitter. Routing is by topic bytes, but
// ATTRIBUTION is gated on contract identity (ADR-0035/0040): the `distribute` topic collides with
// blend_backstop's own `distribute` (different body), so a topic-only match would mis-decode a
// backstop event as an Emitter one.
//
// The Emitter has NO factory namespace, so the gate is the curated set (ADR-0040 §1 mechanism 3, as
// in comet.Decoder): the in-code MainnetGatedSet is the trust root and caller opts (the
// protocol_contracts warm) add operator-admitted instances.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs an Emitter Decoder with MainnetGatedSet installed first and caller opts
// layered on top. With no on-chain creation event there is no factory, so live fan-out never fires.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. It gates on CONTRACT IDENTITY, not topic bytes
// (`distribute` is shared with blend_backstop): an event matches only from a contract in the
// curated registry. A look-alike from an unregistered contract is left for the recognition audit
// (ADR-0033 Claim 2a), visible and never silently attributed.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder], returning one consumer.Event: DistributeEvent,
// DropEvent (all recipients of the one Soroban event) or SwapConfigEvent for `q_swap` / `swap`.
// A decode error is non-fatal: counted and skipped.
//
// Decode is shape-only; the identity gate lives in Matches, so direct callers (tests, fixture
// tooling) bypass it by design.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	if kind == "" {
		return nil, ErrNotEmitterEvent
	}

	closedAt, err := ev.EventClosedAt()
	if err != nil {
		// Fail closed like the comet/blend/phoenix/defindex siblings
		// rather than substituting time.Now() — during a backfill
		// replay that would stamp every event with the wall-clock of
		// the replay run, not the historical ledger.
		return nil, err
	}

	opIndex := uint32(ev.OperationIndex) //nolint:gosec // OperationIndex is non-negative by Soroban spec.
	eventIndex := uint32(ev.EventIndex)  //nolint:gosec // EventIndex is non-negative by Soroban spec.

	switch kind {
	case EventDistribute:
		f, derr := decodeDistribute(&ev)
		if derr != nil {
			return nil, derr
		}
		return []consumer.Event{DistributeEvent{
			ContractID: ev.ContractID,
			Ledger:     ev.Ledger,
			TxHash:     ev.TxHash,
			OpIndex:    opIndex,
			EventIndex: eventIndex,
			ObservedAt: closedAt,
			BackstopID: f.BackstopID,
			Amount:     f.Amount,
		}}, nil

	case EventDrop:
		recipients, derr := decodeDrop(&ev)
		if derr != nil {
			return nil, derr
		}
		out := make([]Recipient, len(recipients))
		for i, r := range recipients {
			out[i] = Recipient{Address: r.Recipient, Amount: r.Amount}
		}
		return []consumer.Event{DropEvent{
			ContractID: ev.ContractID,
			Ledger:     ev.Ledger,
			TxHash:     ev.TxHash,
			OpIndex:    opIndex,
			EventIndex: eventIndex,
			ObservedAt: closedAt,
			Recipients: out,
		}}, nil

	case EventQSwap, EventSwap:
		f, derr := decodeSwapConfig(&ev)
		if derr != nil {
			return nil, derr
		}
		k := SwapConfigQueued
		if kind == EventSwap {
			k = SwapConfigExecuted
		}
		return []consumer.Event{SwapConfigEvent{
			ContractID:       ev.ContractID,
			Ledger:           ev.Ledger,
			TxHash:           ev.TxHash,
			OpIndex:          opIndex,
			EventIndex:       eventIndex,
			ObservedAt:       closedAt,
			Kind:             k,
			NewBackstop:      f.NewBackstop,
			NewBackstopToken: f.NewBackstopToken,
			UnlockTime:       f.UnlockTime,
		}}, nil
	}

	// Unreachable while classify and this switch stay in lockstep.
	return nil, ErrNotEmitterEvent
}
