package phoenix

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Single-event shapes: each event is one complete row, so none of these
// touch the correlation buffer.

var stakeMigrationSlugByTopic = map[string]string{
	TopicMigrationStarted:   StakeActionMigrationStarted,
	TopicMigrationQueried:   StakeActionMigrationQueried,
	TopicMigrationCompleted: StakeActionMigrationCompleted,
}

var blendAdminSlugByTopic = map[string]string{
	TopicBlendSetDelegate:    AdminActionBlendSetDelegate,
	TopicBlendSetMinTradingA: AdminActionBlendSetMinTradingA,
	TopicBlendSetMinTradingB: AdminActionBlendSetMinTradingB,
}

// Map keys of the Map-body liquidity events.
const (
	mapFieldSender    = "sender"
	mapFieldTokenA    = "token_a"
	mapFieldTokenB    = "token_b"
	mapFieldReceivedA = "actual_received_a"
	mapFieldReceivedB = "actual_received_b"
	mapFieldShares    = "shares_amount"
	mapFieldReturnA   = "return_amount_a"
	mapFieldReturnB   = "return_amount_b"
)

func singleStakeChange(ev *events.Event, closedAt time.Time, act, user, token string) []consumer.Event {
	return []consumer.Event{StakeEvent{Change: StakeChange{
		Action:     act,
		Contract:   ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    ev.OperationIndex,
		EventIndex: ev.EventIndex,
		ClosedAt:   closedAt,
		User:       user,
		LPToken:    token,
	}}}
}

// decodeCreateDistributionFlowEvent: a stake contract opening a reward
// flow for one asset. Pool-wide, so User stays empty.
func decodeCreateDistributionFlowEvent(ev *events.Event, closedAt time.Time) ([]consumer.Event, error) {
	asset, err := decodeAddress(ev.Value)
	if err != nil {
		return nil, fmt.Errorf("create_distribution_flow asset: %w", err)
	}
	return singleStakeChange(ev, closedAt, EventActionCreateDistributionFlow, "", asset), nil
}

// decodeStakeMigrationEvent: one step of a stake contract migrating a
// user's stakes into its new storage layout; the body is the user.
func decodeStakeMigrationEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	slug := stakeMigrationSlugByTopic[fieldTopic]
	user, err := decodeAddress(ev.Value)
	if err != nil {
		return nil, fmt.Errorf("stake %s user: %w", slug, err)
	}
	return singleStakeChange(ev, closedAt, slug, user, ""), nil
}

func newAdminEvent(ev *events.Event, closedAt time.Time, slug string) AdminEvent {
	return AdminEvent{
		Pool:        ev.ContractID,
		Ledger:      ev.Ledger,
		TxHash:      ev.TxHash,
		OpIndex:     uint32(ev.OperationIndex), //nolint:gosec // OperationIndex non-negative by Soroban spec.
		EventIndex:  uint32(ev.EventIndex),     //nolint:gosec // EventIndex non-negative by Soroban spec.
		ObservedAt:  closedAt,
		AdminAction: slug,
	}
}

// decodeFactoryConfigEvent records the factory's ("Factory","Updated
// Config") event. The new config is not on the wire; a non-Void body is a
// shape change to audit, so it errors instead of being dropped.
func decodeFactoryConfigEvent(ev *events.Event, closedAt time.Time) ([]consumer.Event, error) {
	sv, err := scval.Parse(ev.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: factory config body: %w", ErrMalformedPayload, err)
	}
	if !scval.IsVoid(sv) {
		return nil, fmt.Errorf("%w: factory config body is %s, want Void", ErrMalformedPayload, sv.Type)
	}
	return []consumer.Event{newAdminEvent(ev, closedAt, AdminActionFactoryConfigUpdated)}, nil
}

// decodeBlendPoolAdminEvent records a blend-pool setting change: the new
// delegate address, or the new i128 minimum trading amount for a token.
func decodeBlendPoolAdminEvent(ev *events.Event, fieldTopic string, closedAt time.Time) ([]consumer.Event, error) {
	out := newAdminEvent(ev, closedAt, blendAdminSlugByTopic[fieldTopic])
	var err error
	if fieldTopic == TopicBlendSetDelegate {
		out.Admin, err = decodeAddress(ev.Value)
	} else {
		out.Value, err = decodeI128(ev.Value)
	}
	if err != nil {
		return nil, fmt.Errorf("blend_pool %s: %w", out.AdminAction, err)
	}
	return []consumer.Event{out}, nil
}

// mapBodyReaders parses a Map body and returns by-name field readers.
// The entry type stays inferred so this package needn't import xdr.
func mapBodyReaders(valueB64 string) (
	addr func(string) (string, error), amount func(string) (canonical.Amount, error), err error,
) {
	sv, err := scval.Parse(valueB64)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: map body: %w", ErrMalformedPayload, err)
	}
	entries, err := scval.AsMap(sv)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: map body: %w", ErrMalformedPayload, err)
	}
	addr = func(key string) (string, error) {
		v, ferr := scval.MustMapField(entries, key)
		if ferr != nil {
			return "", ferr
		}
		return scval.AsAddressStrkey(v)
	}
	amount = func(key string) (canonical.Amount, error) {
		v, ferr := scval.MustMapField(entries, key)
		if ferr != nil {
			return canonical.Amount{}, ferr
		}
		return scval.AsAmountFromI128(v)
	}
	return addr, amount, nil
}

func newLiquidityChange(ev *events.Event, closedAt time.Time, act string) LiquidityChange {
	return LiquidityChange{
		Action:     act,
		Pool:       ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    ev.OperationIndex,
		EventIndex: ev.EventIndex,
		ClosedAt:   closedAt,
	}
}

// decodeProvideLiquidityMapEvent maps actual_received_a/_b, the amounts
// the pool received, onto the same AmountA/AmountB as the String schema.
func decodeProvideLiquidityMapEvent(ev *events.Event, closedAt time.Time) ([]consumer.Event, error) {
	addr, amount, err := mapBodyReaders(ev.Value)
	if err != nil {
		return nil, fmt.Errorf("provide_liquidity: %w", err)
	}
	c := newLiquidityChange(ev, closedAt, EventActionProvideLiquidity)
	if c.Sender, err = addr(mapFieldSender); err != nil {
		return nil, fmt.Errorf("provide_liquidity sender: %w", err)
	}
	if c.TokenA, err = addr(mapFieldTokenA); err != nil {
		return nil, fmt.Errorf("provide_liquidity token_a: %w", err)
	}
	if c.TokenB, err = addr(mapFieldTokenB); err != nil {
		return nil, fmt.Errorf("provide_liquidity token_b: %w", err)
	}
	if c.AmountA, err = amount(mapFieldReceivedA); err != nil {
		return nil, fmt.Errorf("provide_liquidity actual_received_a: %w", err)
	}
	if c.AmountB, err = amount(mapFieldReceivedB); err != nil {
		return nil, fmt.Errorf("provide_liquidity actual_received_b: %w", err)
	}
	return []consumer.Event{LiquidityEvent{Change: c}}, nil
}

// decodeWithdrawLiquidityMapEvent ignores auto_unstake_*: the stake
// contract emits its own unbond for an auto-unstake.
func decodeWithdrawLiquidityMapEvent(ev *events.Event, closedAt time.Time) ([]consumer.Event, error) {
	addr, amount, err := mapBodyReaders(ev.Value)
	if err != nil {
		return nil, fmt.Errorf("withdraw_liquidity: %w", err)
	}
	c := newLiquidityChange(ev, closedAt, EventActionWithdrawLiquidity)
	if c.Sender, err = addr(mapFieldSender); err != nil {
		return nil, fmt.Errorf("withdraw_liquidity sender: %w", err)
	}
	if c.SharesAmount, err = amount(mapFieldShares); err != nil {
		return nil, fmt.Errorf("withdraw_liquidity shares_amount: %w", err)
	}
	if c.AmountA, err = amount(mapFieldReturnA); err != nil {
		return nil, fmt.Errorf("withdraw_liquidity return_amount_a: %w", err)
	}
	if c.AmountB, err = amount(mapFieldReturnB); err != nil {
		return nil, fmt.Errorf("withdraw_liquidity return_amount_b: %w", err)
	}
	return []consumer.Event{LiquidityEvent{Change: c}}, nil
}
