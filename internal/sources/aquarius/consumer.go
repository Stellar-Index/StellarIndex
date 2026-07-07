package aquarius

import (
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/consumer"
)

// TradeEvent is the [consumer.Event] Aquarius's Decoder emits on
// a successful decode. The indexer's event sink type-switches on
// this at its output channel.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "aquarius.trade" }

// Source implements [consumer.Event].
func (TradeEvent) Source() string { return SourceName }

// LiquidityAction discriminates the two Aquarius liquidity-mutating
// events. String values are stamped onto aquarius_liquidity.action and
// match the migration-0089 CHECK constraint.
type LiquidityAction string

// Liquidity actions. `deposit` (deposit_liquidity) grows pool
// reserves; `withdraw` (withdraw_liquidity) shrinks them.
const (
	LiquidityDeposit  LiquidityAction = "deposit"
	LiquidityWithdraw LiquidityAction = "withdraw"
)

// IsValid reports whether a is one of the two known actions.
func (a LiquidityAction) IsValid() bool {
	switch a {
	case LiquidityDeposit, LiquidityWithdraw:
		return true
	}
	return false
}

// ReservesEvent is the [consumer.Event] Aquarius's Decoder emits on a
// successful `update_reserves` decode. It carries the pool's full
// POST-STATE reserve vector (one i128 per pool token, in the pool's
// canonical token order — 2 for a volatile pool, N for stableswap).
// The sink fans it out to one aquarius_reserves row per token
// position. This is the first real Aquarius TVL / liquidity-depth
// signal.
//
// update_reserves carries NO token address in its topics (topic[0] is
// the only topic); the reserve is identified only by position, which
// matches the pool's canonical token order (the order the pool's
// deposit/withdraw/trade topics list the tokens).
type ReservesEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the contract event's index within its operation —
	// an op can emit several update_reserves events (one per pool
	// touched), so this is the per-event discriminator in the
	// aquarius_reserves PK (same role as comet_liquidity.event_index).
	EventIndex uint32
	ObservedAt time.Time
	// Reserves is the post-state reserve for each pool token, ordered
	// by the pool's canonical token index. Length is the pool's token
	// count. i128 per ADR-0003 (never truncates).
	Reserves []canonical.Amount
}

// EventKind implements [consumer.Event].
func (ReservesEvent) EventKind() string { return "aquarius.reserves" }

// Source implements [consumer.Event].
func (ReservesEvent) Source() string { return SourceName }

// LiquidityEvent is the [consumer.Event] Aquarius's Decoder emits on a
// successful `deposit_liquidity` / `withdraw_liquidity` decode. One
// event carries the per-token amounts plus the LP shares minted
// (deposit) or burned (withdraw); the sink fans it out to one
// aquarius_liquidity row per token position, landing Shares on the
// token_index = 0 row only.
type LiquidityEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Action     LiquidityAction
	// Tokens is the pool's token address (strkey) at each position;
	// Amounts is the amount moved for that token. len(Tokens) ==
	// len(Amounts) == the pool's token count. i128 per ADR-0003.
	Tokens  []string
	Amounts []canonical.Amount
	// Shares is the LP-share amount minted (deposit) / burned
	// (withdraw) — a single per-event value, NOT per-token.
	Shares canonical.Amount
}

// EventKind implements [consumer.Event].
func (LiquidityEvent) EventKind() string { return "aquarius.liquidity" }

// Source implements [consumer.Event].
func (LiquidityEvent) Source() string { return SourceName }

// Compile-time checks that the emitted types satisfy consumer.Event.
var (
	_ consumer.Event = TradeEvent{}
	_ consumer.Event = ReservesEvent{}
	_ consumer.Event = LiquidityEvent{}
)
