package defindex

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder implements dispatcher.Decoder (the event-based variant). DeFindex contracts publish
// events on every capital flow at both layers: vault wrappers
// `("DeFindexVault","deposit"|"withdraw")` carry end-user attribution, and Blend strategies
// `("BlendStrategy","deposit"|"withdraw"|...)` carry vault-to-strategy movement.
//
// Both match gated on CONTRACT IDENTITY (ADR-0035/0040): the topic strings are just strings
// any pubnet contract can emit, and the lake holds emitters with the topic shape but none of
// the DeFindex provenance proofs (see MainnetVaults). Dispatch is topic shape AND registry
// membership.
type Decoder struct {
	// reg gates Matches() on contract identity. Factory trust roots (MainnetFactories) gate
	// factory events; vaults and strategies come from the curated in-code seed plus the
	// protocol_contracts operator warm.
	//
	// There is deliberately NO live fan-out from factory `create` bodies. The factory is
	// permissionless, so the strategy addresses a create body names are attacker-controlled: a
	// canonical-factory emitter proves only that the factory announced SOME vault. Seeding the
	// named addresses would let anyone register arbitrary contracts as strategies and
	// contaminate flow/TVL stats. Neither a vault nor a strategy self-registers; each
	// fail-closes into an ADR-0033 recognition gap until an operator verifies provenance and
	// seeds it (docs/protocols/defindex.md).
	reg *contractid.Registry
}

// NewDecoder constructs a defindex Decoder. The curated mainnet set (vaults and strategies) is
// always seeded as the trust root for both layers, because identity cannot be reconstructed
// from the permissionless factory's create events (they omit the vault address and only name
// attacker-controlled strategies). Caller opts layer the protocol_contracts warm and
// live-upsert hook on top; that operator seam is how a future deployment is admitted.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories(MainnetFactories),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY, not topic bytes
// (ADR-0035/0040): vault and strategy flow events match only from a registered child, factory
// events (`create` / `n_fee`) only from a canonical MainnetFactories entry. Decode returns
// ([], nil) for factory events: recognised for EVERY-event completeness, not decoded.
//
// An un-seeded real vault or strategy fail-closes into a visible ADR-0033 recognition gap,
// never silent mis-attribution; closing it is an operator step. The factory `create` body
// cannot vouch for children (permissionless factory, attacker-controlled names).
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) != "" || classifyVault(&ev) != "" {
		return d.reg.Has(ev.ContractID)
	}
	return classifyFactory(&ev) != "" && d.reg.IsFactory(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. It emits Event (strategy), VaultEvent (vault),
// DFeesEvent (one per distributed_fees entry) or AdminEvent for modelled topics. Every other
// recognised topic (vault rebalance / n_wasm; factory `create` / `n_fee`) drops with (nil, nil).
// An error means "skip and count as decode error", reserved for malformed bodies of modelled
// topics; using it for recognised-but-unmodelled topics or untrusted factory bodies would
// inflate the decode-error counter with normal upstream traffic.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if kind := classify(&ev); kind != "" {
		return d.decodeStrategy(&ev, kind)
	}
	if kind := classifyVault(&ev); kind != "" {
		return d.decodeVault(&ev, kind)
	}
	if classifyFactory(&ev) != "" {
		// create / n_fee are recognised so the drop counter doesn't file them as
		// "unmatched topic"; neither is a consumer.Event and neither body is decoded.
		//
		// SECURITY: the factory is permissionless, so a `create` body's strategy addresses are
		// attacker-controlled; a real-factory emitter does not make them genuine strategies.
		// This decode path has no execution-corroboration signal, so fail closed: strategies
		// are recognised only via MainnetStrategies or an operator-seeded protocol_contracts
		// row, like vaults. Unverified ones surface as an ADR-0033 recognition gap.
		return nil, nil
	}
	// Defensive — Matches should have filtered.
	return nil, ErrUnknownEvent
}

// decodeStrategy handles a classified BlendStrategy event; deposit, withdraw and harvest each
// model a StrategyFlow. Harvest bodies carry the {from, amount} shape decodeFlow reads by name
// (plus an unread price_per_share), and dropping them would under-count vault NAV.
func (d *Decoder) decodeStrategy(ev *events.Event, kind string) ([]consumer.Event, error) {
	if kind != EventDeposit && kind != EventWithdraw && kind != EventHarvest {
		return nil, nil // any future unmodelled strategy topic
	}
	flow, err := decodeFlow(ev, kind)
	if err != nil {
		return nil, err
	}
	flow.EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
	return []consumer.Event{Event{Flow: flow}}, nil
}

// decodeVault handles a classified DeFindexVault event. deposit/withdraw model a VaultFlow;
// dfees emits one DFeesEvent per distributed_fees entry so reconcile expected-counts equal
// served rows; the seven admin topics model one [VaultAdmin] each. `rebalance` and n_wasm are
// recognised but not modelled, and drop cleanly.
func (d *Decoder) decodeVault(ev *events.Event, kind string) ([]consumer.Event, error) {
	if _, ok := vaultAdminFields[kind]; ok {
		admin, err := decodeVaultAdmin(ev, kind)
		if err != nil {
			return nil, err
		}
		admin.EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
		return []consumer.Event{AdminEvent{Admin: admin}}, nil
	}
	if kind == EventDFees {
		fees, err := decodeDFees(ev)
		if err != nil {
			return nil, err
		}
		if len(fees) == 0 {
			// Empty distributed_fees Vec — a real observed shape (a
			// distribution ran with nothing to distribute): recognised,
			// zero rows, NOT an error and NOT ErrMalformedPayload.
			return nil, nil
		}
		out := make([]consumer.Event, 0, len(fees))
		for i := range fees {
			fees[i].EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
			out = append(out, DFeesEvent{Fee: fees[i]})
		}
		return out, nil
	}
	if kind != EventDeposit && kind != EventWithdraw {
		return nil, nil // rebalance / n_wasm (unmodelled)
	}
	flow, err := decodeVaultFlow(ev, kind)
	if err != nil {
		return nil, err
	}
	flow.EventIndex = uint32(ev.EventIndex) //nolint:gosec // event index is small, non-negative
	return []consumer.Event{VaultEvent{Flow: flow}}, nil
}
