package upshift

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the Upshift vaults. Every decoded body is
// self-contained, so there is no correlation buffer and no orphan class.
//
// Its one piece of state is the contract-identity gate (ADR-0035). Neither vault has a creation
// event in the lake, so the trust root is the in-code [MainnetGatedSet] (the ADR-0040 curated-set
// mechanism, as in internal/sources/comet), with the `protocol_contracts` DB warm layered on top
// to admit a vault without a redeploy.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs an Upshift Decoder. The curated vault set is
// always installed first; opts layer the DB warm on top.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns every contract whose events Matches can accept: the tightest sound
// contract-id prefilter for a lake read (the projector's Source.ContractIDsFunc). `deposit`,
// `withdraw` and `transfer` are common pubnet symbols, so a topic prefilter would be useless;
// scoping to the vault contracts is cheaper and lossless.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY,
// never on topic bytes alone (ADR-0035).
//
// `deposit`, `withdraw` and `transfer` are generic Soroban symbols with no protocol namespace,
// so a topic-only decoder would record strangers' flows under this source and corrupt the
// ADR-0033 re-derive (served table and expectation would agree over polluted data).
//
// An un-admitted real vault's events are DROPPED, so [MainnetVaults] completeness is
// load-bearing. A missed vault fails CLOSED into a visible recognition gap (ADR-0033 Claim 2a),
// never a silent mis-attribution.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]: one consumer.Event for the four decoded kinds and
// ZERO events for the ten recognized-but-undecoded ones.
//
// Zero rows with NO error is the deliberate ADR-0033 signal: those ten are real, gated events
// with no serveable state, so the re-derive counts their ledgers as expected-zero. An error would
// mark the ledger blind and hold the completeness verdict open forever; errors are reserved for
// INDETERMINATE parse failures.
//
// Decode is shape-only: the identity gate is Matches, consulted first by the dispatcher.
// Direct callers (tests, fixture tooling) bypass the gate by design.
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
