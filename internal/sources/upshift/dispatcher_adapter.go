package upshift

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the Upshift vaults; bodies are self-contained (no correlation buffer, no orphans). Its one state is the contract-identity gate (ADR-0035): neither vault has a creation event, so the trust root is the in-code [MainnetGatedSet] (ADR-0040), with the `protocol_contracts` DB warm on top.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs an Upshift Decoder with the curated vault set installed first; opts layer the DB warm on top.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns every contract Matches can accept, the tightest sound contract-id prefilter for a lake read; `deposit`/`withdraw`/`transfer` are common symbols, so a topic prefilter would be useless.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder], gating on CONTRACT IDENTITY, never topic alone (ADR-0035): `deposit`/`withdraw`/`transfer` are generic symbols, so a topic-only decoder would record strangers' flows and corrupt the ADR-0033 re-derive. An un-admitted real vault is DROPPED, so [MainnetVaults] completeness is load-bearing; it fails closed into a visible recognition gap.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]: one consumer.Event for the four decoded kinds, ZERO events (no error) for the ten recognized-but-undecoded ones, the ADR-0033 expected-zero signal; errors are reserved for INDETERMINATE parse failures, which mark the ledger blind. Shape-only: the identity gate is Matches, so direct callers bypass it by design.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	switch kind {
	case "":
		return nil, ErrNotUpshiftEvent
	case EventDeposit, EventWithdraw:
		out, err := decodeFlow(&ev, kind)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventTransfer:
		out, err := decodeTransfer(&ev)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventDeployedAssetsChanged:
		out, err := decodeDeployedAssets(&ev)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	default:
		// Recognized custody / governance / allowance event. Gated,
		// classified, projected as zero rows.
		return nil, nil
	}
}
