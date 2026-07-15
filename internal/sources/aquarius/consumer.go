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

// RewardsAction discriminates the twelve rewards-gauge event kinds
// (migration 0099, ROADMAP #89). String values match the
// aquarius_rewards_events.event_kind CHECK constraint.
type RewardsAction string

// Rewards-gauge event kinds. See README.md's "Rewards-gauge subsystem"
// section for the per-kind lifetime counts + real-lake-bytes wire-shape
// citations (decode_rewards.go has the per-kind decode + Rust-shape
// doc comment).
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
// successful decode of any of the twelve rewards-gauge event kinds.
// UserAddress / Amount are universal promoted fields, empty/nil when
// the kind doesn't carry them (see decode_rewards.go per-kind doc);
// Attributes carries the kind-specific remainder — i128/u128/u256
// values inside it are decimal strings (ADR-0003: NUMERIC-inside-jsonb
// is lossy, a decimal string round-trips exactly).
type RewardsEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Kind       RewardsAction
	// UserAddress is the acting/addressed account topic slot when the
	// event carries one (may be an end-user LP or a pool admin/manager
	// for config-toggle kinds like set_rewards_state — see the per-kind
	// doc comment in decode_rewards.go). Empty when the kind carries
	// none (e.g. rewards_gauge_add, pool_state).
	UserAddress string
	// Amount is the single dominant, always-non-negative i128/u128
	// amount for kinds that have exactly one (claim_reward,
	// rewards_gauge_claim, claim, set_rewards_config, config_rewards).
	// Nil for kinds with zero, two, or signed amounts — those land
	// in Attributes instead (e.g. position_update's delta CAN be
	// negative, so it never populates this field).
	Amount     *canonical.Amount
	Attributes map[string]any
}

// EventKind implements [consumer.Event].
func (RewardsEvent) EventKind() string { return "aquarius.rewards" }

// Source implements [consumer.Event].
func (RewardsEvent) Source() string { return SourceName }

// AdminAction discriminates the eight governance/upgrade admin event
// kinds (migration 0100, ROADMAP #89). String values match the
// aquarius_admin.event_kind CHECK constraint.
type AdminAction string

// Governance / upgrade admin event kinds. See README.md's
// "Governance / admin" section for lifetime counts; decode_admin.go
// has the per-kind decode + wire-shape citation.
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
// successful decode of any of the eight governance/upgrade event
// kinds. Admin / Target are universal promoted fields, empty when the
// kind doesn't carry them; Attributes carries the kind-specific
// remainder.
type AdminEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	ObservedAt time.Time
	Kind       AdminAction
	// Admin is the initiating-actor topic slot when the event carries
	// one. Empty for every kind observed so far (none of the eight
	// carry a distinct "admin" topic separate from the addressed
	// entity — see decode_admin.go); kept for schema symmetry with
	// aquarius_admin.admin / blend_admin's pattern and any future kind
	// that does carry one.
	Admin string
	// Target is the "addressed entity" of the event — the new Wasm
	// hash (hex) for *_upgrade, the new admin address for
	// *_transfer_ownership, the new reward token for
	// pool_gauge_switch_token. Empty for kinds with no single target
	// (set_privileged_addrs; enable/disable_emergency_mode).
	Target     string
	Attributes map[string]any
}

// EventKind implements [consumer.Event].
func (AdminEvent) EventKind() string { return "aquarius.admin" }

// Source implements [consumer.Event].
func (AdminEvent) Source() string { return SourceName }

// Compile-time checks that the emitted types satisfy consumer.Event.
var (
	_ consumer.Event = TradeEvent{}
	_ consumer.Event = ReservesEvent{}
	_ consumer.Event = LiquidityEvent{}
	_ consumer.Event = RewardsEvent{}
	_ consumer.Event = AdminEvent{}
)
