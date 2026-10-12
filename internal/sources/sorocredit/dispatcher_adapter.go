package sorocredit

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing sorocredit view, gated on contract identity (ADR-0035): the single trust root is [MainnetContract], which emits all events and deploys `Collateral-<uuid>` children via NewCollateralContract (honored only from the root, which Seeds the child). All eight events in practice come from the root; the child gate is forward-compat, and not in pipeline.gatedSources to avoid DB-warming ~139k silent children.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs a sorocredit Decoder with the main contract installed first as the trust root (WithFactories); caller opts layer on top.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithFactories([]string{MainnetContract})}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Compile-time check that *Decoder satisfies dispatcher.Decoder.
var _ dispatcher.Decoder = (*Decoder)(nil)

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns the trust root plus every registered child, the contracts Matches can accept; the completeness re-derive scopes its lake read to it.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder], gating on CONTRACT IDENTITY, not the topic symbol (ADR-0035; two other mainnet contracts emit the same eight symbols). NewCollateralContract matches only from the trust root (IsFactory); every other event from the root or a registered child (Has).
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

// Decode implements [dispatcher.Decoder], returning one consumer.Event per recognised event; a NewCollateralContract also Seeds the announced child, genuine because Matches proved the root emitted it.
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
