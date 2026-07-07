package sorocredit

import (
	"github.com/StellarIndex/stellar-index/internal/consumer"
	"github.com/StellarIndex/stellar-index/internal/contractid"
	"github.com/StellarIndex/stellar-index/internal/dispatcher"
	"github.com/StellarIndex/stellar-index/internal/events"
)

// Decoder is the dispatcher-facing view of the sorocredit protocol
// (ADR-0035 contract-identity gating). The protocol has a single trust
// root — the main contract ([MainnetContract]) — which emits every
// business + config event AND deploys the per-position `Collateral-<uuid>`
// child contracts (via NewCollateralContract).
//
// The gate is a childgate (blend-style): NewCollateralContract is honored
// only from the trust root, and each one announces a child C-address that
// the decoder Seeds into the registry; every other event is honored from
// the trust root OR a registered child.
//
// COVERAGE NOTE: in practice ALL seven event types are emitted by the
// trust root and the child contracts emit NOTHING (verified against the
// r1 lake 2026-07-07). So the child branch (Has) never fires today and
// the trust-root check (IsFactory) is what actually gates. The childgate
// is forward-compat defense-in-depth for a future contract version that
// might route events through the per-position children — which is also
// why this source is NOT registered in pipeline.gatedSources
// (protocol_contracts DB-warm/persist): DB-warming ~139k+ never-emitting
// children would be pure overhead with zero coverage benefit, unlike
// blend where the seeded pools DO emit. Live in-memory seeding suffices.
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

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY,
// not the topic symbol (ADR-0035): the seven symbols are distinctive but
// two other mainnet contracts emit them, so a non-trust-root emitter must
// NOT be attributed to this source.
//
//   - NewCollateralContract matches ONLY from the trust root (IsFactory):
//     only the protocol root announces a new position, so a child or a
//     look-alike emitting this topic can't inject a contract into the
//     registry.
//   - every other event matches from the trust root OR a registered
//     child (Has) — the child branch is forward-compat (see the Decoder
//     doc: children emit nothing today).
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

// Decode implements [dispatcher.Decoder]. Returns exactly one
// consumer.Event per recognised event. On a NewCollateralContract it also
// Seeds the announced child collateral C-address into the registry so the
// childgate's Has() branch would recognise it (Matches already guaranteed
// the deploy came from the trust root, so the child is genuine).
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
