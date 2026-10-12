package defindex

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder implements dispatcher.Decoder for vault wrappers (`("DeFindexVault","deposit"|"withdraw")`, end-user attribution) and Blend strategies (`("BlendStrategy",...)`, vault-to-strategy movement); both are gated on CONTRACT IDENTITY (ADR-0035/0040), since any pubnet contract can emit these topic strings.
type Decoder struct {
	// reg gates Matches() on contract identity: MainnetFactories gate factory events; vaults and
	// strategies come from the curated seed plus the protocol_contracts operator warm.
	//
	// There is deliberately NO live fan-out from factory `create` bodies: the factory is
	// permissionless, so the addresses a body names are attacker-controlled and seeding them would
	// let anyone register arbitrary contracts and contaminate flow/TVL. Unseeded vaults and
	// strategies fail-close into an ADR-0033 recognition gap until an operator verifies provenance
	// (docs/protocols/defindex.md).
	reg *contractid.Registry
}

// NewDecoder constructs a defindex Decoder; the curated mainnet set is always seeded as the trust root because create events omit the vault address and only name attacker-controlled strategies.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories(MainnetFactories),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (d *Decoder) Name() string { return SourceName }

// Matches implements [dispatcher.Decoder], gating on CONTRACT IDENTITY, not topic bytes (ADR-0035/0040): flow events match only from a registered child, factory events (`create` / `n_fee`) only from a MainnetFactories entry and are recognised but not decoded; an un-seeded real vault or strategy fail-closes into a visible recognition gap.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) != "" || classifyVault(&ev) != "" {
		return d.reg.Has(ev.ContractID)
	}
	return classifyFactory(&ev) != "" && d.reg.IsFactory(ev.ContractID)
}

// Decode implements [dispatcher.Decoder], emitting Event, VaultEvent, DFeesEvent (one per distributed_fees entry) or AdminEvent for modelled topics; other recognised topics drop with (nil, nil), and an error is reserved for malformed bodies of modelled topics.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if kind := classify(&ev); kind != "" {
		return d.decodeStrategy(&ev, kind)
	}
	if kind := classifyVault(&ev); kind != "" {
		return d.decodeVault(&ev, kind)
	}
	if classifyFactory(&ev) != "" {
		// create / n_fee are recognised so the drop counter doesn't file them as "unmatched topic".
		// SECURITY: a `create` body's strategy addresses are attacker-controlled (permissionless
		// factory) and nothing here corroborates them, so fail closed; strategies are recognised only
		// via MainnetStrategies or an operator-seeded protocol_contracts row.
		return nil, nil
	}
	// Defensive — Matches should have filtered.
	return nil, ErrUnknownEvent
}

// decodeStrategy handles a classified BlendStrategy event; deposit, withdraw and harvest each
// model a StrategyFlow. Dropping harvest would under-count vault NAV.
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

// decodeVault handles a classified DeFindexVault event. deposit/withdraw model a VaultFlow; dfees
// emits one DFeesEvent per distributed_fees entry so reconcile expected-counts equal served rows;
// the seven admin topics model one [VaultAdmin] each.
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
