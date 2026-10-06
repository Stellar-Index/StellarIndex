package spectra

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing Spectra decoder. Its one piece of state
// is the contract-identity gate (ADR-0035): the curated [MainnetContracts]
// set, with caller options layering the protocol_contracts DB warm on top.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs a Decoder with the curated set installed first.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns every contract whose events Matches can accept.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. Contract identity first, topic
// second: `wrap`, `approve` and friends are generic symbols any contract
// can emit (ADR-0035).
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. wrap and unwrap yield one event;
// the recognised `approve` yields zero rows and no error (ADR-0033
// expected-zero). Malformed bodies are errors so the ledger stays blind.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	switch kind := classify(&ev); kind {
	case "":
		return nil, ErrNotSpectraEvent
	case EventWrap, EventUnwrap:
		out, err := decodeWrapper(&ev, kind)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	default:
		return nil, nil
	}
}
