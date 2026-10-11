package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// PhoenixStakeAction discriminates the stake-contract actions. Its values must match the
// phoenix_stake_events.action CHECK constraint and the EventAction* constants in
// internal/sources/phoenix.
type PhoenixStakeAction string

const (
	PhoenixBond   PhoenixStakeAction = "bond"
	PhoenixUnbond PhoenixStakeAction = "unbond"
	// PhoenixWithdrawRewards / PhoenixDistributeRewards — the stake
	// contract's reward-claim surface (migration 0097). Neither carries
	// an amount on the wire (see internal/sources/phoenix/events.go);
	// distribute_rewards is pool-wide and carries no user either.
	PhoenixWithdrawRewards   PhoenixStakeAction = "withdraw_rewards"
	PhoenixDistributeRewards PhoenixStakeAction = "distribute_rewards"
	// Stake-contract lifecycle (migration 0195): a reward flow opened for
	// an asset (no user), and the three steps of a per-user stake
	// migration (no token).
	PhoenixCreateDistributionFlow PhoenixStakeAction = "create_distribution_flow"
	PhoenixMigrationStarted       PhoenixStakeAction = "migration_started"
	PhoenixMigrationQueried       PhoenixStakeAction = "migration_queried"
	PhoenixMigrationCompleted     PhoenixStakeAction = "migration_completed"
)

// IsValid reports whether a is one of the known actions.
func (a PhoenixStakeAction) IsValid() bool {
	switch a {
	case PhoenixBond, PhoenixUnbond, PhoenixWithdrawRewards, PhoenixDistributeRewards,
		PhoenixCreateDistributionFlow, PhoenixMigrationStarted, PhoenixMigrationQueried, PhoenixMigrationCompleted:
		return true
	}
	return false
}

// hasUser reports whether the action's event names a user.
func (a PhoenixStakeAction) hasUser() bool {
	return a != PhoenixDistributeRewards && a != PhoenixCreateDistributionFlow
}

// hasToken reports whether the action's event names a token.
func (a PhoenixStakeAction) hasToken() bool {
	switch a {
	case PhoenixMigrationStarted, PhoenixMigrationQueried, PhoenixMigrationCompleted:
		return false
	case PhoenixBond, PhoenixUnbond, PhoenixWithdrawRewards, PhoenixDistributeRewards, PhoenixCreateDistributionFlow:
		return true
	}
	return true
}

// PhoenixStakeEvent is one phoenix_stake_events row: a single observed stake-contract event from
// a Phoenix pool.
//
// Amount is a decimal string (i128 -> *big.Int -> string), always positive; the action carries
// the direction. It is empty for withdraw_rewards / distribute_rewards, which carry no amount.
//
// LPToken is the LP share-token address on bond/unbond but is REPURPOSED to the reward-token /
// distributed-asset address on withdraw_rewards / distribute_rewards (see phoenix/events.go).
// User is empty for pool-wide distribute_rewards and create_distribution_flow; LPToken is empty
// for migration_*.
type PhoenixStakeEvent struct {
	StakeContract string
	Ledger        uint32
	ObservedAt    time.Time
	TxHash        string
	OpIndex       uint32
	// EventIndex is the first field-event's in-op index — the per-event
	// discriminator added to the phoenix_stake_events PK by migration
	// 0060 so two bonds/unbonds in one op don't collide.
	EventIndex uint32
	Action     PhoenixStakeAction
	User       string
	LPToken    string
	Amount     string // decimal i128; empty for reward actions (NULL column)
}

// InsertPhoenixStakeEvent appends one phoenix_stake_events row, idempotent on the
// (ledger_close_time, stake_contract, ledger, tx_hash, op_index, action, event_index) PK
// (event_index keeps two bonds/unbonds in one op from colliding). The upsert is a
// generation-guarded DO UPDATE, not DO NOTHING: a replay at the SAME generation is a no-op, but a
// re-derive at a higher generation lands its correction.
//
// It rejects empty StakeContract / TxHash and an invalid Action before touching the DB. User is
// required unless the action is pool-wide; LPToken unless it is a migration_* step; Amount only
// for bond/unbond, since the reward events carry none (the column is nullable for that reason).
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
	if e.User == "" && e.Action.hasUser() {
		return errors.New("timescale: InsertPhoenixStakeEvent: User is empty")
	}
	if e.LPToken == "" && e.Action.hasToken() {
		return errors.New("timescale: InsertPhoenixStakeEvent: LPToken is empty")
	}
	if e.Amount == "" && (e.Action == PhoenixBond || e.Action == PhoenixUnbond) {
		return fmt.Errorf("timescale: InsertPhoenixStakeEvent: Amount required (contract=%s tx=%s)",
			e.StakeContract, e.TxHash)
	}

	// Generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the staked `amount` (or user_addr / lp_token)
	// lands in place when its generation is >= the stored one; a live gen-0
	// replay can never revert it.
	const q = `
        INSERT INTO phoenix_stake_events (
            stake_contract, ledger, ledger_close_time, tx_hash, op_index,
            action, event_index, user_addr, lp_token, amount,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10,
            $11
        )
        ON CONFLICT (ledger_close_time, stake_contract, ledger, tx_hash, op_index, action, event_index) DO UPDATE SET
            user_addr         = EXCLUDED.user_addr,
            lp_token          = EXCLUDED.lp_token,
            amount            = EXCLUDED.amount,
            derive_generation = EXCLUDED.derive_generation
          WHERE phoenix_stake_events.derive_generation <= EXCLUDED.derive_generation
    `
	_, err := s.db.ExecContext(ctx, q,
		e.StakeContract, int(e.Ledger), e.ObservedAt.UTC(), e.TxHash, int(e.OpIndex),
		string(e.Action), int(e.EventIndex), nullString(e.User), nullString(e.LPToken), nullNumeric(e.Amount),
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertPhoenixStakeEvent %s@%d: %w",
			e.StakeContract, e.Ledger, err)
	}
	return nil
}

// PhoenixStakeSummary is the windowed Phoenix LP-staking summary, the READ side of
// phoenix_stake_events. Amounts are LP-share-token base units (i128/NUMERIC, never int64);
// NetStaked is negative when unbonds exceed bonds in the window.
type PhoenixStakeSummary struct {
	Bonds         int64
	Unbonds       int64
	Bonded        canonical.Amount
	Unbonded      canonical.Amount
	NetStaked     canonical.Amount
	UniqueStakers int64
}

// PhoenixStakeWindowStats reads the windowed bond / unbond volumes and participants from
// phoenix_stake_events. Amounts stay LP-share-token base units as canonical.Amount.
//
// It returns (nil, nil) when the window has no bond/unbond event, so the KPI is omitted.
// windowDays <= 0 means 90.
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
