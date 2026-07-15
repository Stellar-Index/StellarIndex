package comet

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of Comet. Single instance
// per indexer — Comet uses a shared ("POOL", <event_name>) topic
// namespace across every pool contract, so event ROUTING is by topic
// bytes, but ATTRIBUTION is gated on contract identity (ADR-0035/
// 0040, CS-026): any pubnet contract deployed from (or mimicking)
// the Balancer-v1 WASM emits the identical topic shape, and without
// the gate a look-alike could inject fabricated trades under
// `source = "comet"`.
//
// Comet has NO factory namespace — no creation event announces new
// pools — so the gate is the curated-set mechanism (ADR-0040 §1
// mechanism 2/3): the in-code MainnetGatedSet (today exactly one
// pool, Blend's backstop) is the trust root; caller opts layer the
// protocol_contracts DB warm on top (the operator seam for admitting
// a future pool without a redeploy). The WASM-hash sweep is the
// registered upkeep loop for discovering new byte-identical pools.
//
// No goroutines, no polling. Claims any of the five Soroban-emitted
// POOL events from a REGISTERED pool: swap (→ TradeEvent), join_pool
// / exit_pool / deposit / withdraw (→ LiquidityEvent). Admin
// functions (set_controller, gulp, init) exist but do NOT publish
// events in the Soroban port; BPT transfers go through the SEP-41
// standard token-event surface (handled by sep41_supply when the
// pool is in scope), not the POOL namespace.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs a Comet Decoder. The in-code curated set
// (MainnetGatedSet) is always installed first; caller opts (WithSeed
// from the protocol_contracts warm) layer discovered pools on top.
// There are no factories — comet pools have no on-chain creation
// event to self-register from, so live fan-out never fires.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT
// IDENTITY, not topic bytes (ADR-0035/0040, CS-026): the bare
// ("POOL", <event>) tuple is the Balancer-v1 event family shared by
// EVERY deployment of that code — forgeable by construction. An
// event matches ONLY when emitted by a pool in the curated registry
// (MainnetGatedSet + protocol_contracts warm). A comet-shaped event
// from an unregistered contract is left for the recognition audit to
// surface (ADR-0033 Claim 2a) — visible, never silently attributed.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. Returns exactly one
// consumer.Event on success — TradeEvent for swap, LiquidityEvent
// for the other four kinds. A decode error is non-fatal per the
// dispatcher contract — counted by the source's orphan/malformed
// metrics and skipped.
//
// Decode itself stays shape-only (no registry lookup): the identity
// gate lives in Matches, which the dispatcher consults first. Direct
// Decode callers (tests, fixture tooling) bypass the gate by design.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	if kind == "" {
		return nil, ErrNotCometEvent
	}

	closedAt, err := ev.EventClosedAt()
	if err != nil {
		// Comet events use ledger close time for the event timestamp
		// (unlike oracles, there's no contract-declared timestamp in
		// the body). Fail closed like the blend/phoenix/defindex
		// siblings rather than substituting time.Now() — during a
		// backfill replay that would stamp every event with the
		// wall-clock of the replay run, not the historical ledger.
		return nil, err
	}

	if kind == EventSwap {
		trade, err := decodeSwap(&ev, closedAt)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{TradeEvent{Trade: trade}}, nil
	}

	// All other recognised kinds are liquidity events.
	liq, err := decodeLiquidityEvent(&ev, closedAt)
	if err != nil {
		return nil, err
	}
	return []consumer.Event{liq}, nil
}
