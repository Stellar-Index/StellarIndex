package blnt

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder gates the BLNT V3 ABIs on the canonical contracts and on pools
// announced by the canonical factory. Generic symbols never establish identity.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs a decoder with the factory and current deployment as
// cold-start trust roots. Options layer the protocol_contracts warm and hook on
// top so newly deployed pools survive restarts.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories([]string{MainnetPoolFactory}),
		contractid.WithSeed(MainnetGatedSet()),
	}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements dispatcher.Decoder.
func (*Decoder) Name() string { return SourceName }

// GatedContractSet is the tight contract-id prefilter for projector lake reads.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

func (d *Decoder) roleOf(contractID string) Role {
	switch contractID {
	case MainnetPoolFactory:
		return RoleFactory
	case MainnetBackstop:
		return RoleBackstop
	case MainnetEmitter:
		return RoleEmitter
	case MainnetBackfill:
		return RoleBackfill
	case MainnetLockupBackfill:
		return RoleLockupBackfill
	default:
		if d.reg.Has(contractID) {
			return RolePool
		}
		return ""
	}
}

// Matches requires both a registered contract and a topic emitted by that
// contract's role-specific ABI.
func (d *Decoder) Matches(ev events.Event) bool {
	role := d.roleOf(ev.ContractID)
	return role != "" && classify(role, &ev) != ""
}

// Decode produces exactly one lossless semantic-envelope event. A canonical
// factory deploy also admits the announced child pool for later events.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	role := d.roleOf(ev.ContractID)
	kind := classify(role, &ev)
	if role == "" || kind == "" {
		return nil, fmt.Errorf("%w: contract=%s", ErrNotBLNTEvent, ev.ContractID)
	}
	out, err := decodeEvent(role, kind, &ev)
	if err != nil {
		return nil, err
	}
	if role == RoleFactory && kind == "deploy" {
		target, err := decodeDeployTarget(&ev)
		if err != nil {
			return nil, err
		}
		d.reg.Seed(target, MainnetPoolFactory, ev.Ledger)
	}
	return []consumer.Event{out}, nil
}
