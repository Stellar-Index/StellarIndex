package sorocredit

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of sorocredit (ADR-0035 contract-identity gating). The
// single trust root is the main contract ([MainnetContract]), which emits every business + config
// event and deploys the per-position `Collateral-<uuid>` children (via NewCollateralContract).
//
// The gate is a blend-style childgate: NewCollateralContract is honored only from the trust root
// and announces a child C-address that the decoder Seeds; every other event is honored from the
// trust root OR a registered child.
//
// In practice all eight event types come from the trust root and the children emit NOTHING, so the
// IsFactory check is what gates; the childgate is forward-compat defense for a contract version
// that routes events through children. For the same reason this source is NOT in
// pipeline.gatedSources: DB-warming ~139k never-emitting children is pure overhead, unlike blend
// whose seeded pools do emit. Live in-memory seeding suffices.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs a sorocredit Decoder. The trust-root set (the
// single main contract) is intrinsic to the protocol — hard-coded via
// WithFactories, always installed first; caller opts (WithSeed / WithHook,
// unused in current wiring) layer on top.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithFactories([]string{MainnetContract})}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the contracts Matches can accept: the trust root
// plus every child registered so far. The completeness re-derive scopes its
// lake read to it, so a child's events are counted exactly as live admits them.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY, not the topic symbol
// (ADR-0035): two other mainnet contracts emit the same eight symbols.
//
//   - NewCollateralContract matches ONLY from the trust root (IsFactory), so a child or look-alike
//     emitting this topic can't inject a contract into the registry.
//   - every other event matches from the trust root OR a registered child (Has); the child branch
//     is forward-compat (see the Decoder doc).
func (d *Decoder) Matches(ev events.Event) bool {
	kind := classify(&ev)
	if kind == "" {
		return false
	}
	if kind == TypeNewCollateralContract {
		return d.reg.IsFactory(ev.ContractID)
	}
	return d.reg.IsFactory(ev.ContractID) || d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder], returning one consumer.Event per recognised event. A
// NewCollateralContract also Seeds the announced child C-address so the childgate's Has() branch
// recognises it (Matches already guaranteed the trust root emitted it, so the child is genuine).
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	outs, err := project(&ev)
	if err != nil {
		return nil, err
	}
	for _, o := range outs {
		e, ok := o.(Event)
		if !ok {
			continue
		}
		if e.EventType == TypeNewCollateralContract && e.CollateralContract != "" {
			// factoryID = the trust root that announced the child
			// (ev.ContractID — guaranteed to be the trust root by Matches).
			d.reg.Seed(e.CollateralContract, ev.ContractID, ev.Ledger)
		}
	}
	return outs, nil
}
