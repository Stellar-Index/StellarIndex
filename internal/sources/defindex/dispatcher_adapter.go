package defindex

import (
	"github.com/StellarIndex/stellar-index/internal/consumer"
	"github.com/StellarIndex/stellar-index/internal/contractid"
	"github.com/StellarIndex/stellar-index/internal/events"
)

// Decoder implements dispatcher.Decoder (the event-based variant —
// not ContractCallDecoder). DeFindex contracts publish Soroban
// contract events on every capital flow at both layers:
//
//   - Vault wrappers: `("DeFindexVault","deposit"|"withdraw")` —
//     end-user (G-strkey) attribution.
//   - Blend strategies: `("BlendStrategy","deposit"|"withdraw"|…)` —
//     vault↔strategy capital movement (`from` = vault C-strkey).
//
// We match both, gated on CONTRACT IDENTITY (ADR-0035/0040, CS-026):
// the namespaced topic strings are still just strings any pubnet
// contract can emit, and the lake contains emitters that carry the
// topic shape but none of the four independent DeFindex-provenance
// proofs (see MainnetVaults). Dispatch is therefore topic shape AND
// registry membership — the curated evidence-verified set plus any
// operator-seeded protocol_contracts rows.
type Decoder struct {
	// reg gates Matches() on contract identity. The factory trust
	// roots (MainnetFactories) gate factory events; children come
	// from the curated in-code seed + the protocol_contracts warm.
	// NOTE: unlike blend, the factory `create` event does NOT carry
	// the new vault's address, so there is no live fan-out — a new
	// vault fail-closes into a recognition gap until it is verified
	// and seeded (docs/protocols/defindex.md).
	reg *contractid.Registry
}

// NewDecoder constructs a defindex Decoder. Contract-identity gating
// (ADR-0035/0040): the curated mainnet set (vault wrappers +
// strategies, docs/protocols/defindex.md) is ALWAYS seeded — the
// deploy-graph cannot be reconstructed from creation events (they
// omit the vault address), so the in-code evidence-verified seed is
// the trust root. Caller opts layer the protocol_contracts DB warm +
// live-upsert hook on top (the hook only fires if a future factory
// WASM ever announces children in a decodable way).
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories(MainnetFactories),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT
// IDENTITY, not topic bytes (ADR-0035/0040):
//
//   - vault / strategy flow events match ONLY when emitted by a
//     REGISTERED child (curated seed + protocol_contracts warm);
//   - factory events (`create` / `n_fee`) match ONLY when emitted
//     by one of the canonical MainnetFactories. Decode returns
//     ([], nil) for them — recognised for EVERY-event-policy
//     completeness, not decoded into a flow.
//
// COVERAGE NOTE (ADR-0035): an un-seeded real vault fail-closes into
// an ADR-0033 recognition gap — visible, never silently
// mis-attributed. Because the create event omits the child address,
// closing such a gap is an operator step (verify provenance, then
// seed protocol_contracts / extend the in-code set) rather than
// automatic fan-out.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) != "" || classifyVault(&ev) != "" {
		return d.reg.Has(ev.ContractID)
	}
	return classifyFactory(&ev) != "" && d.reg.IsFactory(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. Emits one Event per
// matched flow — Event (strategy layer) or VaultEvent (vault
// wrapper layer) — for the deposit/withdraw events we model. Every
// OTHER recognised topic (strategy harvest; vault rebalance + the
// eight admin events; factory create / n_fee) drops cleanly with
// (nil, nil): "match, nothing to emit". Returning an ERROR is a
// "skip + count-as-decode-error" signal, reserved for genuinely
// malformed deposit/withdraw bodies — NOT for topics we recognise
// but intentionally don't model yet (BACKLOG #58). Filing those as
// decode errors would inflate the source's decode-error counter for
// normal upstream traffic.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if kind := classify(&ev); kind != "" {
		return d.decodeStrategy(&ev, kind)
	}
	if kind := classifyVault(&ev); kind != "" {
		return d.decodeVault(&ev, kind)
	}
	if classifyFactory(&ev) != "" {
		// Factory create / n_fee — recognised so the dispatcher's
		// drop-counter doesn't file them as "unmatched topic", but
		// no consumer.Event yet (body decode is Phase C; the body
		// does NOT carry the new vault's address, so there is no
		// registry fan-out to do here either).
		return nil, nil
	}
	// Defensive — Matches should have filtered.
	return nil, ErrUnknownEvent
}

// decodeStrategy handles a classified BlendStrategy event. Only
// deposit/withdraw model a StrategyFlow; `harvest` is recognised
// (EVERY-event policy) but its body has never been observed on-chain
// and is NOT modelled — it drops cleanly rather than counting as a
// decode error (BACKLOG #58, blocked on real samples).
func (d *Decoder) decodeStrategy(ev *events.Event, kind string) ([]consumer.Event, error) {
	if kind != EventDeposit && kind != EventWithdraw {
		return nil, nil // harvest (or any future unmodelled strategy topic)
	}
	flow, err := decodeFlow(ev, kind)
	if err != nil {
		return nil, err
	}
	flow.EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
	return []consumer.Event{Event{Flow: flow}}, nil
}

// decodeVault handles a classified DeFindexVault event. Only
// deposit/withdraw model a VaultFlow; `rebalance` and the eight admin
// topics (rescue / paused / unpaused / nreceiver / nmanager /
// nemanager / rbmanager / dfees) are recognised but NOT modelled —
// their bodies (including the four-way rebalance_method payloads) have
// never been observed on-chain, so they drop cleanly. The rebalance
// discriminator scaffolding lives in [DecodeRebalanceMethod]; the
// per-method payload decode is blocked on real samples (BACKLOG #58).
func (d *Decoder) decodeVault(ev *events.Event, kind string) ([]consumer.Event, error) {
	if kind != EventDeposit && kind != EventWithdraw {
		return nil, nil // rebalance / admin (unmodelled)
	}
	flow, err := decodeVaultFlow(ev, kind)
	if err != nil {
		return nil, err
	}
	flow.EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
	return []consumer.Event{VaultEvent{Flow: flow}}, nil
}
