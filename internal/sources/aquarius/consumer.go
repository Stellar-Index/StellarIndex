package aquarius

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
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
// successful `update_reserves` decode. It carries the pool's full post-state reserve vector (one
// i128 per token, in canonical token order) and is the Aquarius TVL signal; the sink fans it out
// to one aquarius_reserves row per token position.
//
// update_reserves has no token address in its topics, so a reserve is identified only by position
// in the pool's canonical token order (the order its deposit/withdraw/trade topics list tokens).
type ReservesEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the event's index within its operation; an op can emit several
	// update_reserves events (one per pool touched), so it is part of the aquarius_reserves PK.
	EventIndex uint32
	ObservedAt time.Time
	// Kind is EventUpdateReserves (-> aquarius_reserves) or EventReservesSync (-> the distinct
	// aquarius_reserves_sync). Empty is treated as update_reserves.
	Kind string
	// Reserves is the per-pool-token vector in canonical token order: post-state reserves for
	// update_reserves, the synced value for reserves_sync. i128 per ADR-0003 (never truncates).
	Reserves []canonical.Amount
}

// EventKind implements [consumer.Event]. reserves_sync lands in its own table, so it carries its
// own kind; a by-EventKind reconcile can then tell the two apart.
func (e ReservesEvent) EventKind() string {
	if e.Kind == EventReservesSync {
		return "aquarius.reserves_sync"
	}
	return "aquarius.reserves"
}

// Source implements [consumer.Event].
func (ReservesEvent) Source() string { return SourceName }

// LiquidityEvent is the [consumer.Event] Aquarius's Decoder emits on a
// successful `deposit_liquidity` / `withdraw_liquidity` decode. It carries per-token amounts plus
// the LP shares minted or burned; the sink fans it out to one aquarius_liquidity row per token
// position, landing Shares on the token_index = 0 row only.
type LiquidityEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Action     LiquidityAction
	// Tokens is the token strkey at each position and Amounts the amount moved for it;
	// len(Tokens) == len(Amounts). i128 per ADR-0003.
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

// RewardsAction discriminates the twelve rewards-gauge event kinds
// (migration 0099). String values match the
// aquarius_rewards_events.event_kind CHECK constraint.
type RewardsAction string

// Rewards-gauge event kinds. See README.md's "Rewards-gauge subsystem" section and
// decode_rewards.go for the per-kind wire shapes.
const (
	RewardsPoolState           RewardsAction = RewardsAction(EventPoolState)
	RewardsClaimReward         RewardsAction = RewardsAction(EventClaimReward)
	RewardsSetRewardsConfig    RewardsAction = RewardsAction(EventSetRewardsConfig)
	RewardsPositionUpdate      RewardsAction = RewardsAction(EventPositionUpdate)
	RewardsDeposit             RewardsAction = RewardsAction(EventGaugeDeposit)
	RewardsClaimFees           RewardsAction = RewardsAction(EventClaimFees)
	RewardsGaugeClaim          RewardsAction = RewardsAction(EventRewardsGaugeClaim)
	RewardsClaim               RewardsAction = RewardsAction(EventGaugeClaim)
	RewardsGaugeScheduleReward RewardsAction = RewardsAction(EventRewardsGaugeScheduleReward)
	RewardsSetRewardsState     RewardsAction = RewardsAction(EventSetRewardsState)
	RewardsGaugeAdd            RewardsAction = RewardsAction(EventRewardsGaugeAdd)
	RewardsConfigRewards       RewardsAction = RewardsAction(EventConfigRewards)
)

// RewardsEvent is the [consumer.Event] Aquarius's Decoder emits on a
// successful decode of any of the twelve rewards-gauge event kinds. UserAddress / Amount are
// promoted fields, empty/nil when the kind lacks them; Attributes carries the kind-specific
// remainder, with i128/u128/u256 values as decimal strings (ADR-0003: jsonb numbers are lossy).
type RewardsEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Kind       RewardsAction
	// UserAddress is the acting/addressed account topic slot: an end-user LP, or a pool
	// admin/manager for config-toggle kinds. Empty when the kind carries none.
	UserAddress string
	// Amount is the single always-non-negative i128/u128 amount for kinds with exactly one. Nil
	// for kinds with zero, two or signed amounts, which land in Attributes (position_update's
	// delta can be negative, so it never populates this field).
	Amount     *canonical.Amount
	Attributes map[string]any
}

// EventKind implements [consumer.Event].
func (RewardsEvent) EventKind() string { return "aquarius.rewards" }

// Source implements [consumer.Event].
func (RewardsEvent) Source() string { return SourceName }

// AdminAction discriminates the eight governance/upgrade admin event
// kinds (migration 0100). String values match the
// aquarius_admin.event_kind CHECK constraint.
type AdminAction string

// Governance / upgrade admin event kinds; decode_admin.go has the per-kind wire shapes.
const (
	AdminApplyUpgrade            AdminAction = AdminAction(EventApplyUpgrade)
	AdminCommitUpgrade           AdminAction = AdminAction(EventCommitUpgrade)
	AdminSetPrivilegedAddrs      AdminAction = AdminAction(EventSetPrivilegedAddrs)
	AdminApplyTransferOwnership  AdminAction = AdminAction(EventApplyTransferOwnership)
	AdminCommitTransferOwnership AdminAction = AdminAction(EventCommitTransferOwnership)
	AdminEnableEmergencyMode     AdminAction = AdminAction(EventEnableEmergencyMode)
	AdminDisableEmergencyMode    AdminAction = AdminAction(EventDisableEmergencyMode)
	AdminPoolGaugeSwitchToken    AdminAction = AdminAction(EventPoolGaugeSwitchToken)
)

// AdminEvent is the [consumer.Event] Aquarius's Decoder emits on a
// successful decode of any of the eight governance/upgrade event kinds. Admin / Target are
// promoted fields, empty when the kind lacks them; Attributes carries the remainder.
type AdminEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Kind       AdminAction
	// Admin is the initiating-actor topic slot. Empty for every observed kind (none carries an
	// admin topic distinct from the addressed entity); kept for schema symmetry with
	// aquarius_admin.admin.
	Admin string
	// Target is the addressed entity: the new Wasm hash (hex) for *_upgrade, the new admin for
	// *_transfer_ownership, the new reward token for pool_gauge_switch_token. Empty when a kind
	// has no single target.
	Target     string
	Attributes map[string]any
}

// EventKind implements [consumer.Event].
func (AdminEvent) EventKind() string { return "aquarius.admin" }

// Source implements [consumer.Event].
func (AdminEvent) Source() string { return SourceName }

// FeeEvent is a protocol-fee treasury event: set_protocol_fee (per-token old to new fee) or
// claim_protocol_fee (accrued fee swept to a recipient), discriminated by Kind. The sink lands it
// in aquarius_protocol_fee. The four Fee* fields are set on set_protocol_fee, Recipient + Amount
// on claim_protocol_fee.
type FeeEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Kind       string // EventSetProtocolFee | EventClaimProtocolFee
	// set_protocol_fee: per-token old to new protocol fee (u32). The Map wire form carries all
	// four values (HasOldFee = true); the Vec form carries one pool-wide new fraction and no old
	// value (HasOldFee = false), so the sink lands the *_old columns NULL, not a fabricated 0.
	Fee0New   uint32
	Fee0Old   uint32
	Fee1New   uint32
	Fee1Old   uint32
	HasOldFee bool
	// claim_protocol_fee: the sweep destination and amount (i128, ADR-0003), one event per token.
	// Token is the claimed token's contract address from topic[1]; the body has none. Amounts
	// are meaningless without it, since same-tx claims of different tokens differ in scale.
	Recipient string
	Token     string
	Amount    canonical.Amount
}

// EventKind implements [consumer.Event].
func (FeeEvent) EventKind() string { return "aquarius.fee" }

// Source implements [consumer.Event].
func (FeeEvent) Source() string { return SourceName }

// KillEvent is a pool circuit-breaker toggle: one of the eight kill_/unkill_ admin actions. It has
// no body and a single topic, so (contract, action, event identity) fully identifies it. The sink
// lands it in aquarius_kill_switches.
type KillEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Action     string // one of the eight EventKill*/EventUnkill* constants
}

// EventKind implements [consumer.Event].
func (KillEvent) EventKind() string { return "aquarius.kill" }

// Source implements [consumer.Event].
func (KillEvent) Source() string { return SourceName }

// Compile-time checks that the emitted types satisfy consumer.Event.
var (
	_ consumer.Event = TradeEvent{}
	_ consumer.Event = ReservesEvent{}
	_ consumer.Event = LiquidityEvent{}
	_ consumer.Event = RewardsEvent{}
	_ consumer.Event = AdminEvent{}
	_ consumer.Event = FeeEvent{}
	_ consumer.Event = KillEvent{}
)
