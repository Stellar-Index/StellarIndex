package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// PhoenixStakeAction discriminates the stake-contract actions.
// String values match the phoenix_stake_events.action CHECK
// constraint (migration 0044, widened by migration 0097) and
// internal/sources/phoenix's EventActionBond / EventActionUnbond /
// EventActionWithdrawRewards / EventActionDistributeRewards
// constants.
type PhoenixStakeAction string

const (
	PhoenixBond   PhoenixStakeAction = "bond"
	PhoenixUnbond PhoenixStakeAction = "unbond"
	// PhoenixWithdrawRewards / PhoenixDistributeRewards — the stake
	// contract's reward-claim surface (ROADMAP #89 residual, migration
	// 0097). Neither carries an amount on the wire (see
	// internal/sources/phoenix/events.go); distribute_rewards is
	// pool-wide and carries no user either.
	PhoenixWithdrawRewards   PhoenixStakeAction = "withdraw_rewards"
	PhoenixDistributeRewards PhoenixStakeAction = "distribute_rewards"
)

// IsValid reports whether a is one of the known actions.
func (a PhoenixStakeAction) IsValid() bool {
	switch a {
	case PhoenixBond, PhoenixUnbond, PhoenixWithdrawRewards, PhoenixDistributeRewards:
		return true
	}
	return false
}

// PhoenixStakeEvent is one phoenix_stake_events row — a single
// observed bond / unbond / withdraw_rewards / distribute_rewards
// event from a Phoenix per-pool stake contract. Mirrors the
// migration-0044 columns (widened by migration 0097).
//
// Amount is a decimal-string numeric (per ADR-0003 i128 ->
// *big.Int -> string), always positive — the action discriminator
// carries the direction. Empty for withdraw_rewards /
// distribute_rewards (NULL column, migration 0097) — neither event
// carries an amount on the wire.
//
// LPToken holds the LP share-token address on bond/unbond, and is
// REPURPOSED to the reward-token / distributed-asset address on
// withdraw_rewards / distribute_rewards — same column, different
// per-action meaning, per the "reuse existing columns" preference
// (see phoenix/events.go doc). User is empty for distribute_rewards
// (a pool-wide announcement with no per-user attribution).
type PhoenixStakeEvent struct {
	StakeContract string
	Ledger        uint32
	ObservedAt    time.Time
	TxHash        string
	OpIndex       uint32
	// EventIndex is the first field-event's in-op index — the per-event
	// discriminator added to the phoenix_stake_events PK by migration
	// 0060 (F-1324) so two bonds/unbonds in one op don't collide.
	EventIndex uint32
	Action     PhoenixStakeAction
	User       string
	LPToken    string
	Amount     string // decimal i128; empty for reward actions (NULL column)
}

// InsertPhoenixStakeEvent appends one phoenix_stake_events row,
// idempotent on the (ledger_close_time, stake_contract, ledger,
// tx_hash, op_index, action, event_index) PK (event_index added by
// migration 0060 / F-1324 so two bonds/unbonds in one op don't
// collide). Re-running the indexer over the same range or replaying
// a backfill writes the same rows; ON CONFLICT DO NOTHING makes the
// replay a no-op.
//
// Defensive: rejects empty StakeContract / TxHash / LPToken and an
// invalid Action before touching the DB. User is required for every
// action except distribute_rewards (pool-wide, no per-user
// attribution on the wire). Amount is required only for bond/unbond
// — withdraw_rewards / distribute_rewards carry no amount on the
// event itself (migration 0097 made the column nullable for exactly
// this reason).
func (s *Store) InsertPhoenixStakeEvent(ctx context.Context, e PhoenixStakeEvent) error {
	if e.StakeContract == "" {
		return errors.New("timescale: InsertPhoenixStakeEvent: StakeContract is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertPhoenixStakeEvent: TxHash is empty")
	}
	if !e.Action.IsValid() {
		return fmt.Errorf("timescale: InsertPhoenixStakeEvent: invalid Action %q", e.Action)
	}
	if e.User == "" && e.Action != PhoenixDistributeRewards {
		return errors.New("timescale: InsertPhoenixStakeEvent: User is empty")
	}
	if e.LPToken == "" {
		return errors.New("timescale: InsertPhoenixStakeEvent: LPToken is empty")
	}
	if e.Amount == "" && (e.Action == PhoenixBond || e.Action == PhoenixUnbond) {
		return fmt.Errorf("timescale: InsertPhoenixStakeEvent: Amount required (contract=%s tx=%s)",
			e.StakeContract, e.TxHash)
	}

	const q = `
        INSERT INTO phoenix_stake_events (
            stake_contract, ledger, ledger_close_time, tx_hash, op_index,
            action, event_index, user_addr, lp_token, amount
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10
        )
        ON CONFLICT (ledger_close_time, stake_contract, ledger, tx_hash, op_index, action, event_index) DO NOTHING
    `
	_, err := s.db.ExecContext(ctx, q,
		e.StakeContract, int(e.Ledger), e.ObservedAt.UTC(), e.TxHash, int(e.OpIndex),
		string(e.Action), int(e.EventIndex), nullString(e.User), e.LPToken, nullNumeric(e.Amount),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertPhoenixStakeEvent %s@%d: %w",
			e.StakeContract, e.Ledger, err)
	}
	return nil
}

// PhoenixStakeSummary is the windowed Phoenix LP-staking activity summary —
// the READ side of phoenix_stake_events (migration 0044). Amounts are the
// staked LP-share-token amount in base units (i128/NUMERIC, never int64 —
// ADR-0003); NetStaked can be negative when unbonds exceed bonds in the
// window.
type PhoenixStakeSummary struct {
	Bonds         int64
	Unbonds       int64
	Bonded        canonical.Amount
	Unbonded      canonical.Amount
	NetStaked     canonical.Amount
	UniqueStakers int64
}

// PhoenixStakeWindowStats reads the windowed Phoenix LP-staking summary
// (bond / unbond volumes + participants) from phoenix_stake_events. Amounts
// are LP-share-token base units (per-asset decimals), preserved as
// canonical.Amount (ADR-0003).
//
// Empty-safe: returns (nil, nil) when no bond/unbond event exists in the
// window, so the bespoke KPI is omitted cleanly. windowDays <= 0 is treated
// as 90.
func (s *Store) PhoenixStakeWindowStats(ctx context.Context, windowDays int) (*PhoenixStakeSummary, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	var out PhoenixStakeSummary
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE action = 'bond'),
		       count(*) FILTER (WHERE action = 'unbond'),
		       COALESCE(sum(amount) FILTER (WHERE action = 'bond'),0)::text,
		       COALESCE(sum(amount) FILTER (WHERE action = 'unbond'),0)::text,
		       COALESCE(sum(CASE WHEN action = 'bond' THEN amount
		                         WHEN action = 'unbond' THEN -amount END),0)::text,
		       count(DISTINCT user_addr)
		  FROM phoenix_stake_events WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&out.Bonds, &out.Unbonds, &out.Bonded, &out.Unbonded, &out.NetStaked, &out.UniqueStakers); err != nil {
		return nil, fmt.Errorf("timescale: PhoenixStakeWindowStats: %w", err)
	}
	if out.Bonds == 0 && out.Unbonds == 0 {
		return nil, nil
	}
	return &out, nil
}
