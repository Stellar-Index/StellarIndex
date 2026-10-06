package spectra

import (
	"fmt"
	"slices"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Attribute keys a discovered PT or YT is persisted with, so a restart
// knows its role (and a YT its market) without re-reading the creation.
const (
	AttrRole     = "role"
	AttrMarketPT = "market_pt"
)

// Decoder is the dispatcher-facing Spectra decoder. Its state is the
// contract-identity gate (ADR-0035) and each gated contract's role:
//
//   - the factory is the trust root for `pt_deployed`, which admits the PT;
//   - `yt_deployed` from an admitted PT admits the YT, with the PT as its
//     market;
//   - the registry, router, order engines, wrapper IBTs and the current
//     PTs/YTs are hand-kept ([MainnetContracts], [MainnetInfrastructure]).
//
// An IBT named by `pt_deployed` is a market attribute only and is never
// admitted (earnXLM / earnUSDC are Upshift vaults). The registry's
// `*_change` events admit nothing.
//
// In a market's creation transaction the PT's `yt_deployed` precedes the
// factory's `pt_deployed`, so the first read of that window misses it. The
// projector re-reads a window whose gate widened, and the re-read claims it.
type Decoder struct {
	reg *contractid.Registry

	mu       sync.RWMutex
	roles    map[string]Role   // discovered PTs and YTs
	ytMarket map[string]string // discovered YT → PT
}

// NewDecoder constructs a Decoder with the hand-kept set installed first.
// Options layer the protocol_contracts warm (contractid.WithAttrSeed
// carries the role attributes) and the persistence hooks on top.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{
		contractid.WithFactories([]string{MainnetFactory}),
		contractid.WithSeed(MainnetGatedSet()),
		contractid.WithSeed(append([]string{MainnetRouter}, MainnetOrderEngines...)),
	}
	d := &Decoder{
		reg:      contractid.New(append(base, opts...)...),
		roles:    make(map[string]Role),
		ytMarket: make(map[string]string),
	}
	for id, a := range d.reg.AllAttrs() {
		switch Role(a[AttrRole]) {
		case RolePT:
			d.roles[id] = RolePT
		case RoleYT:
			if a[AttrMarketPT] != "" {
				d.roles[id], d.ytMarket[id] = RoleYT, a[AttrMarketPT]
			}
		}
	}
	return d
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns every contract whose events Matches can accept.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// role returns a contract's role, or "" when it is not gated or was
// admitted without one (a bare protocol_contracts row): such a contract
// fails closed into a visible recognition gap.
func (d *Decoder) role(id string) Role {
	if m, ok := MainnetContracts[id]; ok {
		return m.Role
	}
	switch {
	case d.reg.IsFactory(id):
		return RoleFactory
	case id == MainnetRouter:
		return RoleRouter
	case slices.Contains(MainnetOrderEngines, id):
		return RoleOrderEngine
	}
	if !d.reg.Has(id) {
		return ""
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.roles[id]
}

func (d *Decoder) marketOf(yt string) string {
	if m, ok := MainnetContracts[yt]; ok {
		return m.MarketPT
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ytMarket[yt]
}

// gate returns the event's kind and emitter role when the kind is claimed
// from that role. Contract identity first, topic second: every Spectra
// symbol is generic (`transfer`, `deposit`, `role_granted`...).
func (d *Decoder) gate(ev *events.Event) (string, Role, bool) {
	kind := classify(ev)
	if kind == "" {
		return "", "", false
	}
	role := d.role(ev.ContractID)
	return kind, role, role != "" && slices.Contains(kindRoles[kind], role)
}

// Matches implements [dispatcher.Decoder].
func (d *Decoder) Matches(ev events.Event) bool {
	_, _, ok := d.gate(&ev)
	return ok
}

// Decode implements [dispatcher.Decoder]. A row kind yields one [Event];
// a recognised zero-row kind yields none and no error (ADR-0033
// expected-zero). A malformed body is an error, so the ledger stays blind.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind, role, ok := d.gate(&ev)
	switch {
	case kind == "":
		return nil, ErrNotSpectraEvent
	case !ok:
		return nil, fmt.Errorf("%w: %s from %s (role %q)", ErrNotGated, kind, ev.ContractID, role)
	}
	if !isRowKind(kind, role) {
		return nil, checkRegistryChange(&ev, kind)
	}

	out, err := newEvent(&ev, kind, role)
	if err != nil {
		return nil, err
	}
	switch role {
	case RolePT:
		out.MarketPT = ev.ContractID
	case RoleYT:
		if out.MarketPT = d.marketOf(ev.ContractID); out.MarketPT == "" {
			return nil, fmt.Errorf("%w: YT %s has no known market", ErrNotGated, ev.ContractID)
		}
	}
	if out, err = decodeRow(&ev, out); err != nil {
		return nil, err
	}
	switch kind {
	case EventPTDeployed:
		if err := d.admitPT(out); err != nil {
			return nil, err
		}
	case EventYTDeployed:
		d.admitYT(out)
	}
	return []consumer.Event{out}, nil
}

// isRowKind reports whether kind from role becomes a spectra_events row.
// An IBT's own `transfer` is its share token moving, not a PT/YT one.
func isRowKind(kind string, role Role) bool {
	switch kind {
	case EventMint, EventBurn, EventApprove:
		return false
	case EventTransfer:
		return role == RolePT || role == RoleYT
	}
	return slices.Contains(rowKinds, kind)
}

var rowKinds = []string{
	EventPTDeployed, EventYTDeployed, EventPTAdded, EventPTMinted, EventRedeem,
	EventYieldUpdated, EventTransfer, EventWrap, EventUnwrap, EventDeposit,
	EventWithdraw, EventOrderRegistered, EventOrderFilled, EventOrderCancelled,
}

// admitPT admits a factory-announced PT. Matches proved the event came
// from the factory, so the PT is genuine Spectra code; its IBT is not
// admitted.
func (d *Decoder) admitPT(e Event) error {
	if r, ok := MainnetContracts[e.MarketPT]; ok && r.Role != RolePT {
		return fmt.Errorf("%w: pt_deployed names %s, hand-kept as %s", ErrMalformedPayload, e.MarketPT, r.Role)
	}
	d.mu.Lock()
	d.roles[e.MarketPT] = RolePT
	d.mu.Unlock()
	d.reg.SeedWithAttrs(e.MarketPT, e.ContractID, e.Ledger, contractid.Attrs{AttrRole: string(RolePT)})
	return nil
}

// admitYT admits the YT an admitted PT announces, with the PT as its
// market and provenance. A hand-kept YT keeps its curated market.
func (d *Decoder) admitYT(e Event) {
	d.mu.Lock()
	d.roles[e.YT], d.ytMarket[e.YT] = RoleYT, e.MarketPT
	d.mu.Unlock()
	d.reg.SeedWithAttrs(e.YT, e.MarketPT, e.Ledger,
		contractid.Attrs{AttrRole: string(RoleYT), AttrMarketPT: e.MarketPT})
}
