package phoenix

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// TradeEvent is the [consumer.Event] emitted on a completed 8-field swap assembly.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "phoenix.trade" }

// EventLedger reports the trade's own ledger, the correlation group's first-field ledger; for a sweep rescue this differs from the triggering event's ledger, so the completeness re-derive must count the trade here.
func (e TradeEvent) EventLedger() uint32 { return e.Trade.Ledger }

// Source implements [consumer.Event].
func (TradeEvent) Source() string { return SourceName }

// Compile-time check that TradeEvent satisfies consumer.Event.
var _ consumer.Event = TradeEvent{}

// LiquidityChange is one completed provide_liquidity (5-event) or withdraw_liquidity (4-event) reassembly (phoenix_liquidity row); withdraw events carry no token addresses so TokenA / TokenB stay empty there, and SharesAmount is LP shares burned (withdraw only).
type LiquidityChange struct {
	Action  string
	Pool    string
	Ledger  uint32
	TxHash  string
	OpIndex int
	// EventIndex is the first field-event's in-op index; it keeps two provides/withdraws in one op
	// from colliding on the PK.
	EventIndex   int
	ClosedAt     time.Time
	Sender       string
	TokenA       string
	AmountA      canonical.Amount
	TokenB       string
	AmountB      canonical.Amount
	SharesAmount canonical.Amount // withdraw-only; zero on provide
}

// LiquidityEvent is the [consumer.Event] wrapping a LiquidityChange, type-switched by the sink.
type LiquidityEvent struct {
	Change LiquidityChange
}

// EventKind implements [consumer.Event].
func (LiquidityEvent) EventKind() string { return "phoenix.liquidity" }

// EventLedger follows TradeEvent.EventLedger's sweep-rescue attribution contract.
func (e LiquidityEvent) EventLedger() uint32 { return e.Change.Ledger }

// Source implements [consumer.Event].
func (LiquidityEvent) Source() string { return SourceName }

// Compile-time check.
var _ consumer.Event = LiquidityEvent{}

// StakeChange is one completed bond / unbond (3-event reassembly, phoenix_stake_events row); each pool has its own stake contract accepting only its LP-share token, and Amount is positive for both directions (Action carries the direction).
type StakeChange struct {
	Action   string
	Contract string
	Ledger   uint32
	TxHash   string
	OpIndex  int
	// EventIndex is the first field-event's in-op index; it keeps two bonds/unbonds in one op from
	// colliding on the PK.
	EventIndex int
	ClosedAt   time.Time
	User       string
	LPToken    string
	Amount     canonical.Amount
}

// StakeEvent is the [consumer.Event] wrapping a StakeChange.
type StakeEvent struct {
	Change StakeChange
}

// EventKind implements [consumer.Event].
func (StakeEvent) EventKind() string { return "phoenix.stake" }

// EventLedger follows TradeEvent.EventLedger's sweep-rescue attribution contract.
func (e StakeEvent) EventLedger() uint32 { return e.Change.Ledger }

// Source implements [consumer.Event].
func (StakeEvent) Source() string { return SourceName }

// Compile-time check.
var _ consumer.Event = StakeEvent{}

// Every correlation-buffered kind must satisfy completeness.eventLedgerCarrier so a sweep-rescued
// output is counted at its own ledger, not the sweep trigger's.
var (
	_ interface{ EventLedger() uint32 } = TradeEvent{}
	_ interface{ EventLedger() uint32 } = LiquidityEvent{}
	_ interface{ EventLedger() uint32 } = StakeEvent{}
)

// InitializeEvent is a pool-deploy token announcement (one of the two "initialize" token_a / token_b events, each carrying one token address); self-contained, one event, one phoenix_initialize row.
type InitializeEvent struct {
	Pool       string // emitting pool contract C-strkey
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	TokenSlot  string // "a" | "b" (from topic[1])
	Token      string // announced token contract address (event body)
}

// EventKind implements [consumer.Event].
func (InitializeEvent) EventKind() string { return "phoenix.initialize" }

// Source implements [consumer.Event].
func (InitializeEvent) Source() string { return SourceName }

// Compile-time check.
var _ consumer.Event = InitializeEvent{}

// AdminEvent is a pool admin-rotation step, factory config update or blend-pool setting change, landed in phoenix_admin_events; self-contained, one event, one row.
type AdminEvent struct {
	Pool        string // emitting pool contract C-strkey
	Ledger      uint32
	TxHash      string
	OpIndex     uint32
	EventIndex  uint32
	ObservedAt  time.Time
	AdminAction string           // AdminAction* slug (from topic[1])
	Admin       string           // admin address from the body; "" if absent
	Value       canonical.Amount // i128 body of blend_set_min_trading_a/_b; zero otherwise
}

// EventKind implements [consumer.Event].
func (AdminEvent) EventKind() string { return "phoenix.admin" }

// Source implements [consumer.Event].
func (AdminEvent) Source() string { return SourceName }

// Compile-time check.
var _ consumer.Event = AdminEvent{}

// ─── 8-field correlation buffer ─────────────────────────────────
// Phoenix emits one swap as 8 separate events (one per field). An entry waits until all 8 slots
// are populated; a missing field (pagination race, faulty or malformed pool) would otherwise leave
// it forever, so age-based eviction bounds memory.

// defaultOrphanMaxAge caps how long an incomplete entry waits; all 8 events land in one transaction,
// so 5 minutes is generous.
const defaultOrphanMaxAge = 5 * time.Minute

type buffer struct {
	m               map[groupKey]*RawSwap
	pl              map[groupKey]*RawProvideLiquidity
	wl              map[groupKey]*RawWithdrawLiquidity
	bond            map[groupKey]*RawStake
	unbond          map[groupKey]*RawStake
	withdrawRewards map[groupKey]*RawWithdrawRewards
	maxAge          time.Duration
	nowFn           func() time.Time
	// otherEvicted counts non-swap orphans swept during a swap absorb.
	otherEvicted int
}

func (b *buffer) takeOtherEvicted() int {
	n := b.otherEvicted
	b.otherEvicted = 0
	return n
}

func newBuffer() *buffer {
	return &buffer{
		m:               map[groupKey]*RawSwap{},
		pl:              map[groupKey]*RawProvideLiquidity{},
		wl:              map[groupKey]*RawWithdrawLiquidity{},
		bond:            map[groupKey]*RawStake{},
		unbond:          map[groupKey]*RawStake{},
		withdrawRewards: map[groupKey]*RawWithdrawRewards{},
		maxAge:          defaultOrphanMaxAge,
		nowFn:           time.Now,
	}
}

// newRawSwap seeds a RawSwap from the group's first field-event. Its in-op EventIndex is the
// FanoutOpIndex seed that keeps sibling swaps distinct on the trades PK.
func newRawSwap(e *events.Event, closedAt time.Time) *RawSwap {
	return &RawSwap{
		Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex),
		EventIndex: e.EventIndex,
		Pool:       e.ContractID, ClosedAt: closedAt,
	}
}

// absorb stores one field-event in the appropriate RawSwap slot. It returns the completed swap
// (non-nil once all 8 slots are populated), entries evicted for being older than maxAge, and
// ErrUnknownField / decode errors for the current event.
func (b *buffer) absorb(e *events.Event, fieldTopic string, closedAt time.Time) (completed *RawSwap, evicted []RawSwap, err error) {
	// Eviction ages against the event's ClosedAt, not wall-clock, so backfill compares against the
	// timeline being replayed. Liquidity/stake orphans are counted via takeOtherEvicted.
	evicted = b.sweepStale(closedAt)
	b.otherEvicted += b.sweepStaleAll(closedAt)

	k := keyOf(e)
	r, ok := b.m[k]
	if !ok {
		r = newRawSwap(e, closedAt)
		b.m[k] = r
	} else if prior := r.slot(fieldTopic); prior != nil && prior.EventIndex != e.EventIndex {
		// A different EventIndex in a populated slot is the first field of a SECOND swap through the
		// SAME pool in the SAME op (multi-hop or cyclic arbitrage). Without this guard it would
		// overwrite the first swap's slots and lose that trade with no orphan count. Rotate the
		// incomplete group out as a rescue candidate and start a fresh generation keyed on THIS
		// event's index so the trades fan to distinct op_index (FanoutOpIndex). The same EventIndex
		// is a lake redelivery and rewrites the identical value idempotently.
		evicted = append(evicted, *r)
		r = newRawSwap(e, closedAt)
		b.m[k] = r
	}
	if err := r.assign(e, fieldTopic); err != nil {
		return nil, evicted, err
	}
	if r.Complete() {
		delete(b.m, k)
		return r, evicted, nil
	}
	return nil, evicted, nil
}

// sweepStale removes and returns entries older than maxAge relative to `ref`; a zero `ref` falls
// back to nowFn() for drain-at-shutdown calls.
func (b *buffer) sweepStale(ref time.Time) []RawSwap {
	if b.maxAge <= 0 {
		return nil
	}
	if ref.IsZero() {
		ref = b.nowFn()
	}
	cutoff := ref.Add(-b.maxAge)
	var evicted []RawSwap
	for k, r := range b.m {
		if r.ClosedAt.Before(cutoff) {
			evicted = append(evicted, *r)
			delete(b.m, k)
		}
	}
	return evicted
}

// orphans returns the incomplete swap entries without mutating the buffer.
func (b *buffer) orphans() []RawSwap {
	out := make([]RawSwap, 0, len(b.m))
	for _, r := range b.m {
		out = append(out, *r)
	}
	return out
}

// drain empties every per-action buffer. Swap groups are returned for rescue (a 7-field swap never
// Complete()s); every other open group is incomplete by construction and only counted.
func (b *buffer) drain() (swaps []RawSwap, otherOrphans int) {
	swaps = b.orphans()
	clear(b.m)
	otherOrphans = len(b.pl) + len(b.wl) + len(b.bond) + len(b.unbond) + len(b.withdrawRewards)
	clear(b.pl)
	clear(b.wl)
	clear(b.bond)
	clear(b.unbond)
	clear(b.withdrawRewards)
	return swaps, otherOrphans
}

// rotateOpen drops the open group at k when e would land in an already filled slot as a DIFFERENT
// event (the first field of a second action through the same contract in the same op), since
// overwriting would splice two actions into one row. The same EventIndex is a redelivery and
// overwrites idempotently. Returns the number dropped.
func rotateOpen[R interface{ slot(string) *events.Event }](m map[groupKey]R, k groupKey, e *events.Event, fieldTopic string) int {
	r, ok := m[k]
	if !ok {
		return 0
	}
	if prior := r.slot(fieldTopic); prior == nil || prior.EventIndex == e.EventIndex {
		return 0
	}
	delete(m, k)
	return 1
}

// size returns the in-flight swap-entry count. Used by tests.
func (b *buffer) size() int { return len(b.m) }

// ─── provide_liquidity / withdraw_liquidity / bond / unbond absorb ──
//
// Same shape as the swap path: group by (ledger, tx, op, contract) and complete once the action's
// field count is met. Stale peers are swept from EVERY buffer using the current event's ClosedAt,
// so an old swap from another contract still ages out. Eviction counts are advisory (the
// dispatcher's evictedOrphans tally). Per-action maps beat one sum-type buffer because the field
// sets are heterogeneous (5 / 4 / 3 fields).

func (b *buffer) absorbProvideLiquidity(e *events.Event, fieldTopic string, closedAt time.Time) (*RawProvideLiquidity, int, error) {
	evicted := b.sweepStaleAll(closedAt)
	k := keyOf(e)
	evicted += rotateOpen(b.pl, k, e, fieldTopic)
	r, ok := b.pl[k]
	if !ok {
		r = &RawProvideLiquidity{
			Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex),
			EventIndex: e.EventIndex,
			Pool:       e.ContractID, ClosedAt: closedAt,
		}
		b.pl[k] = r
	}
	if err := r.assign(e, fieldTopic); err != nil {
		return nil, evicted, err
	}
	if r.Complete() {
		delete(b.pl, k)
		return r, evicted, nil
	}
	return nil, evicted, nil
}

func (b *buffer) absorbWithdrawLiquidity(e *events.Event, fieldTopic string, closedAt time.Time) (*RawWithdrawLiquidity, int, error) {
	evicted := b.sweepStaleAll(closedAt)
	if fieldTopic == TopicSymbolWLAutoUnbonded {
		// Optional and never stored; a group for it would age out as a false orphan.
		return nil, evicted, nil
	}
	k := keyOf(e)
	evicted += rotateOpen(b.wl, k, e, fieldTopic)
	r, ok := b.wl[k]
	if !ok {
		r = &RawWithdrawLiquidity{
			Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex),
			EventIndex: e.EventIndex,
			Pool:       e.ContractID, ClosedAt: closedAt,
		}
		b.wl[k] = r
	}
	if err := r.assign(e, fieldTopic); err != nil {
		return nil, evicted, err
	}
	if r.Complete() {
		delete(b.wl, k)
		return r, evicted, nil
	}
	return nil, evicted, nil
}

func (b *buffer) absorbStake(e *events.Event, fieldTopic string, closedAt time.Time, isBond bool) (*RawStake, int, error) {
	evicted := b.sweepStaleAll(closedAt)
	k := keyOf(e)
	target := b.unbond
	if isBond && !b.continuesEarlyUnbond(k, fieldTopic) {
		target = b.bond
	}
	evicted += rotateOpen(target, k, e, fieldTopic)
	r, ok := target[k]
	if !ok {
		r = &RawStake{
			Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex),
			EventIndex: e.EventIndex,
			Contract:   e.ContractID, ClosedAt: closedAt, IsBond: isBond,
		}
		target[k] = r
	}
	if err := r.assign(e, fieldTopic); err != nil {
		return nil, evicted, err
	}
	if r.Complete() {
		delete(target, k)
		return r, evicted, nil
	}
	return nil, evicted, nil
}

// continuesEarlyUnbond reports a "bond"-topic token/amount field belonging to an open unbond: the
// earliest stake WASMs published unbond as ("unbond","user"), ("bond","token"), ("bond","amount").
func (b *buffer) continuesEarlyUnbond(k groupKey, fieldTopic string) bool {
	if _, open := b.bond[k]; open {
		return false
	}
	u, open := b.unbond[k]
	if !open || u.User == nil {
		return false
	}
	switch fieldTopic {
	case TopicSymbolStakeToken:
		return u.Token == nil
	case TopicSymbolStakeAmount:
		return u.Amount == nil
	}
	return false
}

func (b *buffer) absorbWithdrawRewards(e *events.Event, fieldTopic string, closedAt time.Time) (*RawWithdrawRewards, int, error) {
	evicted := b.sweepStaleAll(closedAt)
	k := keyOf(e)
	evicted += rotateOpen(b.withdrawRewards, k, e, fieldTopic)
	r, ok := b.withdrawRewards[k]
	if !ok {
		r = &RawWithdrawRewards{
			Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex),
			EventIndex: e.EventIndex,
			Contract:   e.ContractID, ClosedAt: closedAt,
		}
		b.withdrawRewards[k] = r
	}
	if err := r.assign(e, fieldTopic); err != nil {
		return nil, evicted, err
	}
	if r.Complete() {
		delete(b.withdrawRewards, k)
		return r, evicted, nil
	}
	return nil, evicted, nil
}

// sweepStaleAll ages out every per-action map and returns the TOTAL evicted, for the dispatcher's
// orphan tally.
func (b *buffer) sweepStaleAll(ref time.Time) int {
	if b.maxAge <= 0 {
		return 0
	}
	if ref.IsZero() {
		ref = b.nowFn()
	}
	cutoff := ref.Add(-b.maxAge)

	n := 0
	for k, r := range b.pl {
		if r.ClosedAt.Before(cutoff) {
			n++
			delete(b.pl, k)
		}
	}
	for k, r := range b.wl {
		if r.ClosedAt.Before(cutoff) {
			n++
			delete(b.wl, k)
		}
	}
	for k, r := range b.bond {
		if r.ClosedAt.Before(cutoff) {
			n++
			delete(b.bond, k)
		}
	}
	for k, r := range b.unbond {
		if r.ClosedAt.Before(cutoff) {
			n++
			delete(b.unbond, k)
		}
	}
	for k, r := range b.withdrawRewards {
		if r.ClosedAt.Before(cutoff) {
			n++
			delete(b.withdrawRewards, k)
		}
	}
	return n
}
