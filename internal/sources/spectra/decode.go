package spectra

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// classify maps topic[0] to a kind, or "" if unrecognised. Routing only;
// the contract-identity gate is [Decoder.Matches] (ADR-0035).
func classify(e *events.Event) string {
	if len(e.Topic) == 0 {
		return ""
	}
	return topicKind[e.Topic[0]]
}

// reader decodes one event's topics and Map body by field NAME, never by
// position. The first failure sticks; later reads are no-ops.
type reader struct {
	e    *events.Event
	kind string
	body []scval.ScMapEntry
	err  error
}

func newReader(e *events.Event, kind string, topics int) *reader {
	r := &reader{e: e, kind: kind}
	if len(e.Topic) < topics {
		r.err = fmt.Errorf("%w: %s expects %d topics, got %d", ErrShortTopic, kind, topics, len(e.Topic))
	}
	return r
}

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: %s: %s", ErrMalformedPayload, r.kind, fmt.Sprintf(format, args...))
	}
}

func (r *reader) topicAddr(i int, name string) string {
	if r.err != nil {
		return ""
	}
	sv, err := scval.Parse(r.e.Topic[i])
	if err != nil {
		r.fail("topic[%d] (%s): %v", i, name, err)
		return ""
	}
	addr, err := scval.AsAddressStrkey(sv)
	if err != nil {
		r.fail("topic[%d] (%s): %v", i, name, err)
	}
	return addr
}

// topicOrderID reads a bytes32 order id as lowercase hex.
func (r *reader) topicOrderID(i int) string {
	if r.err != nil {
		return ""
	}
	sv, err := scval.Parse(r.e.Topic[i])
	if err != nil {
		r.fail("topic[%d] (order_id): %v", i, err)
		return ""
	}
	b, err := scval.AsBytes(sv)
	if err != nil || len(b) != 32 {
		r.fail("topic[%d] (order_id) is not bytes32 (%d bytes, %v)", i, len(b), err)
		return ""
	}
	return hex.EncodeToString(b)
}

// field returns a named body field, parsing the Map body on first use.
func (r *reader) field(name string) (scval.ScVal, bool) {
	if r.err != nil {
		return scval.ScVal{}, false
	}
	if r.body == nil {
		sv, err := scval.Parse(r.e.Value)
		if err != nil {
			r.fail("parse body: %v", err)
			return scval.ScVal{}, false
		}
		if r.body, err = scval.AsMap(sv); err != nil {
			r.fail("body is not a Map: %v", err)
			return scval.ScVal{}, false
		}
	}
	fv, err := scval.MustMapField(r.body, name)
	if err != nil {
		r.fail("%s: %v", name, err)
		return scval.ScVal{}, false
	}
	return fv, true
}

func (r *reader) addr(name string) string {
	fv, ok := r.field(name)
	if !ok {
		return ""
	}
	addr, err := scval.AsAddressStrkey(fv)
	if err != nil {
		r.fail("%s: %v", name, err)
	}
	return addr
}

func (r *reader) u64(name string) uint64 {
	fv, ok := r.field(name)
	if !ok {
		return 0
	}
	v, err := scval.AsU64(fv)
	if err != nil {
		r.fail("%s: %v", name, err)
	}
	return v
}

func (r *reader) hash(name string) string {
	fv, ok := r.field(name)
	if !ok {
		return ""
	}
	b, err := scval.AsBytes(fv)
	if err != nil {
		r.fail("%s: %v", name, err)
	}
	return hex.EncodeToString(b)
}

func (r *reader) signed(name string) canonical.Amount {
	fv, ok := r.field(name)
	if !ok {
		return canonical.Amount{}
	}
	amt, err := scval.AsAmountFromI128(fv)
	if err != nil {
		r.fail("%s: %v", name, err)
	}
	return amt
}

// amount reads an i128 the protocol only ever emits as a quantity, so a
// negative value is a schema break, not a row (the column is >= 0).
func (r *reader) amount(name string) canonical.Amount {
	amt := r.signed(name)
	if r.err == nil && amt.Sign() < 0 {
		r.fail("%s is negative (%s)", name, amt)
	}
	return amt
}

// mapBody requires a Map body (order_cancelled's `{}`).
func (r *reader) mapBody() {
	if r.err != nil || r.body != nil {
		return
	}
	sv, err := scval.Parse(r.e.Value)
	if err == nil {
		r.body, err = scval.AsMap(sv)
	}
	if err != nil {
		r.fail("body is not a Map: %v", err)
	}
}

// transferAmount reads a SEP-41 `transfer` body: a bare i128, or the
// CAP-67 Map carrying `amount` (and `to_muxed_id`). The type is tested
// before the value is read.
func (r *reader) transferAmount() canonical.Amount {
	if r.err != nil {
		return canonical.Amount{}
	}
	sv, err := scval.Parse(r.e.Value)
	if err != nil {
		r.fail("parse body: %v", err)
		return canonical.Amount{}
	}
	v, err := scval.SEP41BalanceAmount(sv)
	if err != nil {
		r.fail("body is neither an i128 nor a Map carrying amount: %v", err)
		return canonical.Amount{}
	}
	amt := canonical.NewAmount(v)
	if amt.Sign() < 0 {
		r.fail("amount is negative (%s)", amt)
	}
	return amt
}

// decodeRow decodes a row kind into out, whose identity, Kind, Role and
// MarketPT the caller has set.
func decodeRow(e *events.Event, out Event) (Event, error) {
	var r *reader
	switch out.Kind {
	case EventPTDeployed:
		r = newReader(e, out.Kind, 1)
		out.Caller = r.addr("deployer")
		out.DurationSeconds = r.u64("duration")
		out.IBT = r.addr("ibt")
		out.MarketPT = r.addr("pt")
	case EventYTDeployed:
		r = newReader(e, out.Kind, 1)
		out.YT = r.addr("address")
	case EventPTAdded:
		r = newReader(e, out.Kind, 1)
		out.MarketPT = r.addr("pt")
	case EventPTMinted:
		r = newReader(e, out.Kind, 1)
		out.Caller, out.Receiver = r.addr("caller"), r.addr("receiver")
		out.Shares = r.amount("shares")
	case EventRedeem:
		r = newReader(e, out.Kind, 1)
		out.Owner, out.Receiver = r.addr("owner"), r.addr("receiver")
		out.Shares = r.amount("shares")
	case EventYieldUpdated:
		r = newReader(e, out.Kind, 1)
		out.Owner = r.addr("user")
		out.YieldInIBT = r.signed("yield_in_ibt")
	case EventTransfer:
		r = newReader(e, out.Kind, 3)
		out.Caller, out.Receiver = r.topicAddr(1, "from"), r.topicAddr(2, "to")
		out.Amount = r.transferAmount()
	case EventWrap:
		r = newReader(e, out.Kind, 3)
		out.Caller, out.Receiver = r.topicAddr(1, "caller"), r.topicAddr(2, "receiver")
		out.Shares, out.VaultShares = r.amount("shares"), r.amount("vault_shares")
	case EventUnwrap:
		r = newReader(e, out.Kind, 4)
		out.Caller, out.Receiver = r.topicAddr(1, "caller"), r.topicAddr(2, "receiver")
		out.Owner = r.topicAddr(3, "owner")
		out.Shares, out.VaultShares = r.amount("shares"), r.amount("vault_shares")
	case EventDeposit, EventWithdraw:
		r = newReader(e, out.Kind, 4)
		out.Caller, out.Receiver = r.topicAddr(1, "caller"), r.topicAddr(2, "receiver")
		out.Owner = r.topicAddr(3, "owner")
		out.Assets, out.Shares = r.amount("assets"), r.amount("shares")
	case EventOrderRegistered:
		r = newReader(e, out.Kind, 3)
		out.Maker, out.OrderID = r.topicAddr(1, "maker"), r.topicOrderID(2)
		out.Amount = r.amount("making_amount")
	case EventOrderFilled:
		r = newReader(e, out.Kind, 2)
		out.OrderID = r.topicOrderID(1)
		out.Amount = r.amount("actual_making")
	case EventOrderCancelled:
		r = newReader(e, out.Kind, 3)
		out.Maker, out.OrderID = r.topicAddr(1, "maker"), r.topicOrderID(2)
		r.mapBody()
	default:
		return Event{}, fmt.Errorf("%w: %s is not a row kind", ErrNotSpectraEvent, out.Kind)
	}
	if r.err != nil {
		return Event{}, r.err
	}
	return out, nil
}

// checkRegistryChange refuses a registry `*_change` whose new value is
// outside the hand-kept set: the change admits nothing, so an unlisted id
// would leave the gate silently incomplete.
func checkRegistryChange(e *events.Event, kind string) error {
	r := newReader(e, kind, 1)
	var (
		got   string
		known []string
	)
	switch kind {
	case EventFactoryChange:
		got, known = r.addr("new"), []string{MainnetFactory}
	case EventRouterChange:
		got, known = r.addr("new"), []string{MainnetRouter}
	case EventOrderEngineChange:
		got, known = r.addr("new"), MainnetOrderEngines
	case EventPTWasmHashChange:
		got, known = r.hash("new"), []string{PTWasmHash}
	case EventYTWasmHashChange:
		got, known = r.hash("new"), []string{YTWasmHash}
	default:
		return nil
	}
	if r.err != nil {
		return r.err
	}
	if !slices.Contains(known, got) {
		return fmt.Errorf("%w: %s new = %s at ledger %d", ErrUnlistedInfrastructure, kind, got, e.Ledger)
	}
	return nil
}

func newEvent(e *events.Event, kind string, role Role) (Event, error) {
	closedAt, err := e.EventClosedAt()
	if err != nil {
		return Event{}, errors.Join(ErrMalformedPayload, err)
	}
	return Event{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		ObservedAt: closedAt,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // non-negative by Soroban spec.
		Kind:       kind,
		Role:       role,
	}, nil
}
