package comet

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// TradeEvent is the [consumer.Event] Comet's Decoder emits on a
// successful (POOL, swap) decode. The indexer's event sink type-
// switches on this and calls store.InsertTrade — same shape as
// soroswap.TradeEvent / aquarius.TradeEvent / phoenix.TradeEvent.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "comet.trade" }

// Source implements [consumer.Event].
func (TradeEvent) Source() string { return SourceName }

// LiquidityEvent is the [consumer.Event] emitted for a (POOL, join_pool | exit_pool | deposit |
// withdraw) decode. A multi-token join_pool / exit_pool yields one LiquidityEvent per token.
//
// The indexer's sink type-switches on this and calls store.InsertCometLiquidity
// (`comet_liquidity` hypertable). PoolAmountIn is populated only for withdraw (BPT-share count
// burned); the writer translates a zero or unset Amount to SQL NULL on the other kinds.
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
