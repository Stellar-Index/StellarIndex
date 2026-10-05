package phoenix

import (
	"fmt"
	"time"

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

// checkToggleTradingBody fails closed on a non-Bool body so a shape change
// surfaces as an error instead of a silent zero-row recognition.
func checkToggleTradingBody(ev *events.Event) error {
	sv, err := scval.Parse(ev.Value)
	if err != nil {
		return fmt.Errorf("%w: toggle_trading body: %w", ErrMalformedPayload, err)
	}
	if _, err := scval.AsBool(sv); err != nil {
		return fmt.Errorf("%w: toggle_trading body: %w", ErrMalformedPayload, err)
	}
	return nil
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
