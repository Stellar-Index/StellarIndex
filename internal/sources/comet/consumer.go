package comet

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// TradeEvent is the [consumer.Event] Comet's Decoder emits on a successful (POOL, swap) decode; the sink type-switches on it and calls store.InsertTrade.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "comet.trade" }

// Source implements [consumer.Event].
func (TradeEvent) Source() string { return SourceName }

// LiquidityEvent is the [consumer.Event] for a (POOL, join_pool | exit_pool | deposit | withdraw) decode, one per token for a multi-token join/exit; stored in `comet_liquidity`. PoolAmountIn is set only for withdraw (BPT burned); the writer maps a zero/unset Amount to SQL NULL on other kinds.
type LiquidityEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the contract event's index within its operation, the per-event discriminator
	// in the comet_liquidity PK. The swap path fans op_index via canonical.FanoutOpIndex; the
	// liquidity path keys on event_index so two same-(kind,token) events in one op don't collide.
	EventIndex   uint32
	ObservedAt   time.Time
	Kind         LiquidityKind
	Caller       string
	Token        string
	Amount       canonical.Amount
	PoolAmountIn canonical.Amount // withdraw-only; zero on join/exit/deposit
}

// EventKind implements [consumer.Event].
func (LiquidityEvent) EventKind() string { return "comet.liquidity" }

// Source implements [consumer.Event].
func (LiquidityEvent) Source() string { return SourceName }

// Compile-time checks.
var (
	_ consumer.Event = TradeEvent{}
	_ consumer.Event = LiquidityEvent{}
)
