package aquarius

import (
	"fmt"
	"time"

	"github.com/StellarIndex/stellar-index/internal/events"
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// decode_rewards.go decodes the twelve rewards-gauge event kinds
// (ROADMAP #89, 2026-07-10 topic census). AquaToken's soroban-amm
// contract source (github.com/AquaToken/soroban-amm) that previously
// documented this surface is no longer publicly reachable (its GitHub
// org shows zero public repositories as of this audit) — so unlike
// decode.go's `trade` decoder (cited against a cloned Rust source),
// every function below is reverse-engineered directly from real r1
// ClickHouse lake bytes (stellar.contract_events, captured 2026-07-10,
// contract-and-topic scoped queries). Wire TYPES, ARITY, and POSITIONS
// are exact — every field below was read off a real decoded ScVal.
// Business-meaning field NAMES beyond what the bytes themselves prove
// (e.g. "is this i32 a concentrated-liquidity tick, or a reward
// checkpoint index?") are marked BEST-EFFORT in the per-function
// comment; treat them as informative, not authoritative, until the
// Aquarius team confirms or the source becomes available again.
//
// Bodies here are Vec-shaped (Soroban's only wire representation for
// a Rust tuple), so — same as decodeTrade's tuple body — decode is
// positional-by-necessity, not decode-by-Map-field-name; arity is
// checked defensively before each positional read.

// decodeRewardsEvent dispatches on the already-classified event kind
// and returns the decoded RewardsEvent. Called from Decode() after
// Matches() has gated on contract identity.
func decodeRewardsEvent(e *events.Event, kind string, closedAt time.Time) (RewardsEvent, error) {
	switch kind {
	case EventPoolState:
		return decodePoolState(e, closedAt)
	case EventClaimReward:
		return decodeClaimReward(e, closedAt)
	case EventSetRewardsConfig:
		return decodeSetRewardsConfig(e, closedAt)
	case EventPositionUpdate:
		return decodePositionUpdate(e, closedAt)
	case EventGaugeDeposit:
		return decodeGaugeDeposit(e, closedAt)
	case EventClaimFees:
		return decodeClaimFees(e, closedAt)
	case EventRewardsGaugeClaim:
		return decodeRewardsGaugeClaim(e, closedAt)
	case EventGaugeClaim:
		return decodeGaugeClaim(e, closedAt)
	case EventRewardsGaugeScheduleReward:
		return decodeRewardsGaugeScheduleReward(e, closedAt)
	case EventSetRewardsState:
		return decodeSetRewardsState(e, closedAt)
	case EventRewardsGaugeAdd:
		return decodeRewardsGaugeAdd(e, closedAt)
	case EventConfigRewards:
		return decodeConfigRewards(e, closedAt)
	default:
		return RewardsEvent{}, fmt.Errorf("%w: unhandled rewards kind %q", ErrUnknownEvent, kind)
	}
}

func rewardsEnvelope(e *events.Event, kind RewardsAction, closedAt time.Time) RewardsEvent {
	return RewardsEvent{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // non-negative by Soroban spec.
		ObservedAt: closedAt,
		Kind:       kind,
		Attributes: map[string]any{},
	}
}

// decodePoolState decodes `pool_state`.
//
//	topics: [Symbol("pool_state")]                          (topic_count=1)
//	body:   Vec[U256, I32, I128]
//
// Verified against r1 lake bytes 2026-07-10 (pool
// CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT, ledger
// 62006854): body = [<u256 accumulator>, 88499, 2372478774429]. In the
// SAME tx a `position_update` on the same pool carries [86260 (lo),
// 90340 (hi), 2372478774429] — the i128 is IDENTICAL and 88499 sits
// strictly between 86260 and 90340. BEST-EFFORT: this suggests
// field[1] is a range/tick-style checkpoint index and field[2] mirrors
// the correlated position's amount, but this is an observed
// correlation, not a confirmed contract-source semantic — see the
// package-level doc comment. No user/actor topic; fires on every
// reward-affecting pool interaction (by far the densest rewards-gauge
// topic — 339,712 lifetime events).
func decodePoolState(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	elts, err := decodeRewardsBodyVec(e, 3)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("pool_state: %w", err)
	}
	accumulator, err := scval.AsAmountFromU256(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: pool_state accumulator: %w", ErrMalformedPayload, err)
	}
	checkpoint, err := scval.AsI32(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: pool_state checkpoint: %w", ErrMalformedPayload, err)
	}
	value, err := scval.AsAmountFromI128(elts[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: pool_state value: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsPoolState, closedAt)
	rv.Attributes["accumulator"] = accumulator.String()
	rv.Attributes["checkpoint"] = checkpoint
	rv.Attributes["value"] = value.String()
	return rv, nil
}

// decodeClaimReward decodes `claim_reward`.
//
//	topics: [Symbol("claim_reward"), Address(reward_token), Address(user)]  (topic_count=3)
//	body:   Vec[I128]  (length 1: the claimed amount)
//
// Verified against r1 lake bytes 2026-07-10: topic[2] is always a
// G-strkey (the claiming account); topic[1] is a C-strkey reward-token
// address, most commonly the pool's designated reward token. Matches
// docs.aqua.network's public description of `claim`: "claim accrued
// AQUA rewards" — this is the protocol-native reward-claim path.
func decodeClaimReward(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 3 {
		return RewardsEvent{}, fmt.Errorf("%w: claim_reward expected 3 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	rewardToken, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_reward reward_token: %w", ErrMalformedPayload, err)
	}
	user, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_reward user: %w", ErrMalformedPayload, err)
	}
	elts, err := decodeRewardsBodyVec(e, 1)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("claim_reward: %w", err)
	}
	amount, err := scval.AsAmountFromI128(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_reward amount: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsClaimReward, closedAt)
	rv.UserAddress = user
	rv.Amount = &amount
	rv.Attributes["reward_token"] = rewardToken
	return rv, nil
}

// decodeSetRewardsConfig decodes `set_rewards_config` — the POOL-side
// counterpart to the router's `config_rewards` (decodeConfigRewards).
//
//	topics: [Symbol("set_rewards_config")]  (topic_count=1)
//	body:   Vec[U64, U128]  = [expires_at (unix seconds), amount]
//
// Verified against r1 lake bytes 2026-07-10: the SAME tx's
// `config_rewards` router event carries an IDENTICAL amount +
// expires_at pair for the same pool (e.g. amount=7428124,
// expires_at=1758341413 on both), confirming the field identities —
// this is the strongest-confidence field mapping in this file. No
// user/actor topic (admin/router-triggered, not user-triggered).
func decodeSetRewardsConfig(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	elts, err := decodeRewardsBodyVec(e, 2)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("set_rewards_config: %w", err)
	}
	expiresAt, err := scval.AsU64(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: set_rewards_config expires_at: %w", ErrMalformedPayload, err)
	}
	amount, err := scval.AsAmountFromU128(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: set_rewards_config amount: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsSetRewardsConfig, closedAt)
	rv.Amount = &amount
	rv.Attributes["expires_at"] = expiresAt
	return rv, nil
}

// decodePositionUpdate decodes `position_update`.
//
//	topics: [Symbol("position_update"), Address(user)]  (topic_count=2)
//	body:   Vec[I32, I32, I128] = [range_from, range_to, delta]
//
// Verified against r1 lake bytes 2026-07-10: `delta` is SIGNED and
// observed negative on a withdrawal (the same [range_from, range_to]
// pair reappearing with the i128 negated on a later ledger — pool
// CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT, ledgers
// 62006854 → 62007126: +2372478774429 then -2372478774429). Because
// delta can be negative it is NOT promoted to the universal
// (always >= 0) Amount field — it lands in Attributes as a signed
// decimal string. BEST-EFFORT field names (range_from/range_to): see
// decodePoolState's doc comment for the correlated-checkpoint evidence.
func decodePositionUpdate(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 2 {
		return RewardsEvent{}, fmt.Errorf("%w: position_update expected 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: position_update user: %w", ErrMalformedPayload, err)
	}
	elts, err := decodeRewardsBodyVec(e, 3)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("position_update: %w", err)
	}
	rangeFrom, err := scval.AsI32(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: position_update range_from: %w", ErrMalformedPayload, err)
	}
	rangeTo, err := scval.AsI32(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: position_update range_to: %w", ErrMalformedPayload, err)
	}
	delta, err := scval.AsAmountFromI128(elts[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: position_update delta: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsPositionUpdate, closedAt)
	rv.UserAddress = user
	rv.Attributes["range_from"] = rangeFrom
	rv.Attributes["range_to"] = rangeTo
	rv.Attributes["delta"] = delta.String()
	return rv, nil
}

// decodeGaugeDeposit decodes the bare `deposit` event — distinct from
// `deposit_liquidity` (decode.go / migration 0089): docs.aqua.network
// lists `deposit` as its own top-level pool write function alongside
// `swap`/`withdraw`/`claim`.
//
//	topics: [Symbol("deposit"), Address(ref), Address(user)]  (topic_count=3)
//	body:   Vec[I128, I128] = [amount_0, amount_1]
//
// Verified against r1 lake bytes 2026-07-10 (pool
// CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7): topic[1]
// is a CONSTANT C-strkey across every observed deposit on this pool
// (BEST-EFFORT: a gauge/position-manager reference, not necessarily a
// token — unconfirmed); topic[2] varies per call and is always a
// G-strkey (the depositing user). Two i128 amounts; which of the
// pool's tokens each corresponds to is not distinguishable from the
// wire alone (no per-token address in the body), so both land in
// Attributes rather than a single promoted Amount.
func decodeGaugeDeposit(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 3 {
		return RewardsEvent{}, fmt.Errorf("%w: deposit expected 3 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	ref, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: deposit ref: %w", ErrMalformedPayload, err)
	}
	user, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: deposit user: %w", ErrMalformedPayload, err)
	}
	elts, err := decodeRewardsBodyVec(e, 2)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("deposit: %w", err)
	}
	amount0, err := scval.AsAmountFromI128(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: deposit amount_0: %w", ErrMalformedPayload, err)
	}
	amount1, err := scval.AsAmountFromI128(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: deposit amount_1: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsDeposit, closedAt)
	rv.UserAddress = user
	rv.Attributes["ref_address"] = ref
	rv.Attributes["amount_0"] = amount0.String()
	rv.Attributes["amount_1"] = amount1.String()
	return rv, nil
}

// decodeClaimFees decodes `claim_fees`.
//
//	topics: [Symbol("claim_fees"), Address(user), Address(token_a), Address(token_b)]  (topic_count=4)
//	body:   Vec[I128, I128] = [amount_a, amount_b]
//
// Verified against r1 lake bytes 2026-07-10: topic[1] is a G-strkey
// (the claiming user — NOTE the position differs from every other
// rewards event: user is FIRST here, not last) and topic[2]/topic[3]
// are C-strkeys (the pool's two fee-bearing tokens). Either amount can
// legitimately be 0 (a one-sided fee accrual).
func decodeClaimFees(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 4 {
		return RewardsEvent{}, fmt.Errorf("%w: claim_fees expected 4 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_fees user: %w", ErrMalformedPayload, err)
	}
	tokenA, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_fees token_a: %w", ErrMalformedPayload, err)
	}
	tokenB, err := decodeAddressTopic(e.Topic[3])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_fees token_b: %w", ErrMalformedPayload, err)
	}
	elts, err := decodeRewardsBodyVec(e, 2)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("claim_fees: %w", err)
	}
	amountA, err := scval.AsAmountFromI128(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_fees amount_a: %w", ErrMalformedPayload, err)
	}
	amountB, err := scval.AsAmountFromI128(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim_fees amount_b: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsClaimFees, closedAt)
	rv.UserAddress = user
	rv.Attributes["token_a"] = tokenA
	rv.Attributes["token_b"] = tokenB
	rv.Attributes["amount_a"] = amountA.String()
	rv.Attributes["amount_b"] = amountB.String()
	return rv, nil
}

// decodeRewardsGaugeClaim decodes `rewards_gauge_claim` — same shape
// as claim_reward but the amount is U128 (unsigned) on the wire, not
// I128, and the reward token is not always the same as claim_reward's
// (observed paying out the XLM SAC in some samples, suggesting a more
// general third-party-incentive gauge path distinct from the
// protocol-native AQUA claim_reward path).
//
//	topics: [Symbol("rewards_gauge_claim"), Address(reward_token), Address(user)]  (topic_count=3)
//	body:   Vec[U128]  (length 1: the claimed amount)
func decodeRewardsGaugeClaim(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 3 {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_claim expected 3 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	rewardToken, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_claim reward_token: %w", ErrMalformedPayload, err)
	}
	user, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_claim user: %w", ErrMalformedPayload, err)
	}
	elts, err := decodeRewardsBodyVec(e, 1)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("rewards_gauge_claim: %w", err)
	}
	amount, err := scval.AsAmountFromU128(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_claim amount: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsGaugeClaim, closedAt)
	rv.UserAddress = user
	rv.Amount = &amount
	rv.Attributes["reward_token"] = rewardToken
	return rv, nil
}

// decodeGaugeClaim decodes the bare `claim` event — docs.aqua.network:
// "claim accrued AQUA rewards". Unlike every other rewards kind, the
// body is a BARE I128, not a Vec (verified against r1 lake bytes
// 2026-07-10 — the parsed ScVal type is ScvI128 directly).
//
//	topics: [Symbol("claim"), Address(user)]  (topic_count=2)
//	body:   I128  (the claimed amount, NOT wrapped in a Vec)
func decodeGaugeClaim(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 2 {
		return RewardsEvent{}, fmt.Errorf("%w: claim expected 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim user: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim body: %w", ErrMalformedPayload, err)
	}
	amount, err := scval.AsAmountFromI128(body)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: claim amount: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsClaim, closedAt)
	rv.UserAddress = user
	rv.Amount = &amount
	return rv, nil
}

// decodeRewardsGaugeScheduleReward decodes `rewards_gauge_schedule_reward`.
//
//	topics: [Symbol("rewards_gauge_schedule_reward"), Address(reward_token)]  (topic_count=2)
//	body:   Vec[U64, U64, U128] = [starts_at, ends_at, amount]
//
// Verified against r1 lake bytes 2026-07-10: starts_at < ends_at in
// every sample (e.g. 1769788800 < 1770393600), consistent with
// scheduling a future reward-distribution window. No user/actor
// topic (an admin/operator action, not user-triggered).
func decodeRewardsGaugeScheduleReward(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 2 {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_schedule_reward expected 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	rewardToken, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_schedule_reward reward_token: %w", ErrMalformedPayload, err)
	}
	elts, err := decodeRewardsBodyVec(e, 3)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("rewards_gauge_schedule_reward: %w", err)
	}
	startsAt, err := scval.AsU64(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_schedule_reward starts_at: %w", ErrMalformedPayload, err)
	}
	endsAt, err := scval.AsU64(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_schedule_reward ends_at: %w", ErrMalformedPayload, err)
	}
	amount, err := scval.AsAmountFromU128(elts[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_schedule_reward amount: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsGaugeScheduleReward, closedAt)
	rv.Amount = &amount
	rv.Attributes["reward_token"] = rewardToken
	rv.Attributes["starts_at"] = startsAt
	rv.Attributes["ends_at"] = endsAt
	return rv, nil
}

// decodeSetRewardsState decodes `set_rewards_state`.
//
//	topics: [Symbol("set_rewards_state"), Address(admin)]  (topic_count=2)
//	body:   Vec[Bool]  (length 1: the new enabled state)
//
// Verified against r1 lake bytes 2026-07-10 (both `true` and `false`
// observed). topic[1] is populated in UserAddress for schema symmetry
// with the other kinds, though it is a pool admin/manager address
// here, not an end-user LP.
func decodeSetRewardsState(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 2 {
		return RewardsEvent{}, fmt.Errorf("%w: set_rewards_state expected 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	admin, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: set_rewards_state admin: %w", ErrMalformedPayload, err)
	}
	elts, err := decodeRewardsBodyVec(e, 1)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("set_rewards_state: %w", err)
	}
	enabled, err := scval.AsBool(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: set_rewards_state enabled: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsSetRewardsState, closedAt)
	rv.UserAddress = admin
	rv.Attributes["enabled"] = enabled
	return rv, nil
}

// decodeRewardsGaugeAdd decodes `rewards_gauge_add` — gauge
// registration. No user/actor topic (a factory/admin-level action).
//
//	topics: [Symbol("rewards_gauge_add")]  (topic_count=1)
//	body:   Vec[Address, Address]  (length 2)
//
// Verified against r1 lake bytes 2026-07-10: body[0] recurs across
// many samples (BEST-EFFORT: a shared gauge-manager/factory reference,
// unconfirmed); body[1] varies per pool (BEST-EFFORT: the newly
// registered gauge or reward-token contract for this pool).
func decodeRewardsGaugeAdd(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	elts, err := decodeRewardsBodyVec(e, 2)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("rewards_gauge_add: %w", err)
	}
	addr0, err := scval.AsAddressStrkey(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_add address_0: %w", ErrMalformedPayload, err)
	}
	addr1, err := scval.AsAddressStrkey(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: rewards_gauge_add address_1: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsGaugeAdd, closedAt)
	rv.Attributes["address_0"] = addr0
	rv.Attributes["address_1"] = addr1
	return rv, nil
}

// decodeConfigRewards decodes the ROUTER-side `config_rewards` — the
// companion to the pool-side `set_rewards_config` (decodeSetRewardsConfig).
// NOT one of the original 19 README-census topics (that census scanned
// pool-only events); folded in here as the rewards family's 12th kind
// because it was equally undecoded and directly duplicates
// set_rewards_config's (amount, expires_at) pair per-pool — closing
// the docs/protocols/aquarius.md line 23 gap in the same pass rather
// than leaving a second, separately-tracked one.
//
//	topics: [Symbol("config_rewards"), Vec[Address, Address]]  (topic_count=2)
//	body:   Vec[Address(pool), U128(amount), U64(expires_at)]
//
// Verified against r1 lake bytes 2026-07-10 (router
// CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK — the
// canonical trust root; this topic is 100% router-scoped, confirmed
// via a full-history contract-scoped count): body's amount + expires_at
// EXACTLY match the correlated pool's set_rewards_config event in the
// same tx (e.g. amount=7428124, expires_at=1758341413 on both, pool
// CAQODUH4XNX2NTFVACRMO4UR7MA5RLSZA5ZQTHILQYGYYCFQ3LUATIGM). topic[1]
// is itself a nested Vec of two addresses (BEST-EFFORT: an
// initiator/reference pair, unconfirmed) — stored whole in Attributes.
func decodeConfigRewards(e *events.Event, closedAt time.Time) (RewardsEvent, error) {
	if len(e.Topic) != 2 {
		return RewardsEvent{}, fmt.Errorf("%w: config_rewards expected 2 topics, got %d", ErrMalformedPayload, len(e.Topic))
	}
	refsSv, err := scval.Parse(e.Topic[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: config_rewards refs topic: %w", ErrMalformedPayload, err)
	}
	refElts, err := scval.AsVec(refsSv)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: config_rewards refs not a vec: %w", ErrMalformedPayload, err)
	}
	refs := make([]string, 0, len(refElts))
	for i, el := range refElts {
		addr, err := scval.AsAddressStrkey(el)
		if err != nil {
			return RewardsEvent{}, fmt.Errorf("%w: config_rewards refs[%d]: %w", ErrMalformedPayload, i, err)
		}
		refs = append(refs, addr)
	}

	elts, err := decodeRewardsBodyVec(e, 3)
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("config_rewards: %w", err)
	}
	pool, err := scval.AsAddressStrkey(elts[0])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: config_rewards pool: %w", ErrMalformedPayload, err)
	}
	amount, err := scval.AsAmountFromU128(elts[1])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: config_rewards amount: %w", ErrMalformedPayload, err)
	}
	expiresAt, err := scval.AsU64(elts[2])
	if err != nil {
		return RewardsEvent{}, fmt.Errorf("%w: config_rewards expires_at: %w", ErrMalformedPayload, err)
	}
	rv := rewardsEnvelope(e, RewardsConfigRewards, closedAt)
	rv.Amount = &amount
	rv.Attributes["pool"] = pool
	rv.Attributes["expires_at"] = expiresAt
	rv.Attributes["refs"] = refs
	return rv, nil
}

// decodeRewardsBodyVec parses e.Value and asserts it is a Vec of
// exactly n elements — the common shape every rewards-gauge kind
// except the bare `claim` uses.
func decodeRewardsBodyVec(e *events.Event, n int) ([]scval.ScVal, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return nil, fmt.Errorf("%w: body not a vec: %w", ErrMalformedPayload, err)
	}
	if len(elts) != n {
		return nil, fmt.Errorf("%w: body length %d != %d", ErrMalformedPayload, len(elts), n)
	}
	return elts, nil
}
