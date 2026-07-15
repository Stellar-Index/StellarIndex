package blend_emitter

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the Blend Emitter. Single
// instance per indexer — the Emitter has a single canonical mainnet
// contract spanning Blend V1→V2, so event ROUTING is by topic bytes,
// but ATTRIBUTION is gated on contract identity (ADR-0035/0040): the
// `distribute` topic COLLIDES with internal/sources/blend_backstop's
// own `distribute` event (different body shape, same topic symbol —
// see events.go's package doc), so a topic-only match would
// misattribute or mis-decode a backstop event as an Emitter one.
//
// The Emitter has NO factory namespace — no creation event announces
// a new instance — so the gate is the curated-set mechanism
// (ADR-0040 §1 mechanism 3, same as comet.Decoder): the in-code seed
// (MainnetGatedSet: exactly the one known mainnet Emitter) is the
// trust root; caller opts (the protocol_contracts warm) layer any
// operator-admitted future instance on top.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs an Emitter Decoder. The in-code curated set
// (MainnetGatedSet) is always installed first; caller opts (WithSeed
// from the protocol_contracts warm) layer any operator-admitted
// instance on top. There is no factory — the Emitter has no on-chain
// creation event to self-register additional instances from, so live
// fan-out never fires.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT
// IDENTITY, not topic bytes: the bare `distribute` / `drop` /
// `q_swap` / `swap` topics are recognisable but NOT sufficient —
// `distribute` in particular is shared with blend_backstop's event
// surface. An event matches ONLY when emitted by a contract in the
// curated registry (MainnetGatedSet + protocol_contracts warm). A
// look-alike topic from an unregistered contract is left for the
// recognition audit to surface (ADR-0033 Claim 2a) — visible, never
// silently attributed.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. Returns exactly one
// consumer.Event on success: DistributeEvent for `distribute`,
// DropEvent for `drop` (carrying every recipient from the one
// underlying Soroban event — the storage layer fans it out),
// SwapConfigEvent for `q_swap` / `swap`. A decode error is non-fatal
// per the dispatcher contract — counted by the source's orphan/
// malformed metrics and skipped.
//
// Decode itself stays shape-only (no registry lookup): the identity
// gate lives in Matches, which the dispatcher consults first. Direct
// Decode callers (tests, fixture tooling) bypass the gate by design.
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
