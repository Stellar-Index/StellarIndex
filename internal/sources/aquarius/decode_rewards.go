package aquarius

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// decode_rewards.go decodes the twelve rewards-gauge event kinds. The contract source is not
// public, so unlike decode.go's `trade` decoder every function here is reverse-engineered from
// real lake bytes. Wire types, arity and positions are exact; business-meaning field NAMES beyond
// what the bytes prove are marked BEST-EFFORT and are informative, not authoritative.
//
// Bodies are Vec-shaped (Soroban's only wire form for a Rust tuple), so decoding is positional
// by necessity, and arity is checked before each positional read.

// decodeRewardsEvent dispatches on the classified kind; Decode() calls it after Matches() has gated on contract identity.
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

// decodePoolState decodes `pool_state`: topics [Symbol(kind)], body Vec[U256, I32, I128]. BEST-EFFORT: field[1] looks like a
// range/tick checkpoint and field[2] mirrors the same-tx position_update amount; observed, not confirmed. No user topic.
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

// decodeClaimReward decodes `claim_reward`: topics [Symbol(kind), Address(reward_token), Address(user)], body Vec[I128] (len 1).
// topic[2] is the claimer's G-strkey; topic[1] is a C-strkey reward token.
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
	if amount.Sign() < 0 {
		return RewardsEvent{}, fmt.Errorf("%w: claim_reward amount negative: %s", ErrMalformedPayload, amount)
	}
	rv := rewardsEnvelope(e, RewardsClaimReward, closedAt)
	rv.UserAddress = user
	rv.Amount = &amount
	rv.Attributes["reward_token"] = rewardToken
	return rv, nil
}

// decodeSetRewardsConfig decodes the pool-side `set_rewards_config`: topics [Symbol(kind)], body Vec[U64, U128] = [expires_at, amount].
// The same-tx router `config_rewards` carries identical values, confirming the mapping. No user topic.
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

// decodePositionUpdate decodes `position_update`: topics [Symbol(kind), Address(user)], body Vec[I32, I32, I128]
// = [range_from, range_to, delta].
// delta is SIGNED (withdrawal negates it), so it lands in Attributes as a decimal string, not the always-nonnegative Amount.
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

// decodeGaugeDeposit decodes the bare `deposit` (not deposit_liquidity): topics [Symbol(kind), Address(ref), Address(user)],
// body Vec[I128, I128]. The wire does not say which pool token each i128 is, so both land in Attributes, not Amount.
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

// decodeClaimFees decodes `claim_fees`: topics [Symbol(kind), Address(user), Address(token_a), Address(token_b)], body Vec[I128, I128].
// The user is FIRST here, unlike other rewards events; either amount may be 0 (one-sided accrual).
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

// decodeRewardsGaugeClaim decodes `rewards_gauge_claim`: shaped like claim_reward (topics [kind, reward_token, user]) but body
// Vec[U128] (len 1) and the reward token may differ (third-party incentive path).
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

// decodeGaugeClaim decodes the bare `claim` (AQUA rewards): topics [Symbol(kind), Address(user)]; body is a BARE I128, not a Vec.
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
	if amount.Sign() < 0 {
		return RewardsEvent{}, fmt.Errorf("%w: claim amount negative: %s", ErrMalformedPayload, amount)
	}
	rv := rewardsEnvelope(e, RewardsClaim, closedAt)
	rv.UserAddress = user
	rv.Amount = &amount
	return rv, nil
}

// decodeRewardsGaugeScheduleReward decodes `rewards_gauge_schedule_reward`: topics [Symbol(kind), Address(reward_token)],
// body Vec[U64, U64, U128] = [starts_at, ends_at, amount]. No user topic (admin action).
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

// decodeSetRewardsState decodes `set_rewards_state`: topics [Symbol(kind), Address(admin)], body Vec[Bool] (len 1, new enabled state).
// topic[1] fills UserAddress for schema symmetry but is a pool admin, not an LP.
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

// decodeRewardsGaugeAdd decodes `rewards_gauge_add` (no user topic): topics [Symbol(kind)], body Vec[Address, Address] (len 2).
// BEST-EFFORT: body[0] is a shared manager/factory reference; body[1] is the newly registered gauge or reward token.
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

// decodeConfigRewards decodes the ROUTER-side `config_rewards` (repeats the pool's set_rewards_config amount/expires_at): topics
// [Symbol(kind), Vec[Address, Address]] (stored whole in Attributes), body Vec[Address(pool), U128(amount), U64(expires_at)].
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

// decodeRewardsBodyVec asserts e.Value is a Vec of exactly n elements (every rewards kind except the bare `claim`).
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
