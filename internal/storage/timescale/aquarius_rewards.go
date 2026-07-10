package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// AquariusRewardsKind discriminates the twelve rewards-gauge event
// kinds (migration 0099). String values match the
// aquarius_rewards_events.event_kind CHECK constraint and the
// RewardsAction constants in internal/sources/aquarius/consumer.go.
type AquariusRewardsKind string

// Rewards-gauge event kinds — see internal/sources/aquarius/README.md
// (ROADMAP #89) for the per-kind lifetime counts + wire-shape
// citations.
const (
	AquariusRewardsPoolState           AquariusRewardsKind = "pool_state"
	AquariusRewardsClaimReward         AquariusRewardsKind = "claim_reward"
	AquariusRewardsSetRewardsConfig    AquariusRewardsKind = "set_rewards_config"
	AquariusRewardsPositionUpdate      AquariusRewardsKind = "position_update"
	AquariusRewardsDeposit             AquariusRewardsKind = "deposit"
	AquariusRewardsClaimFees           AquariusRewardsKind = "claim_fees"
	AquariusRewardsGaugeClaim          AquariusRewardsKind = "rewards_gauge_claim"
	AquariusRewardsClaim               AquariusRewardsKind = "claim"
	AquariusRewardsGaugeScheduleReward AquariusRewardsKind = "rewards_gauge_schedule_reward"
	AquariusRewardsSetRewardsState     AquariusRewardsKind = "set_rewards_state"
	AquariusRewardsGaugeAdd            AquariusRewardsKind = "rewards_gauge_add"
	AquariusRewardsConfigRewards       AquariusRewardsKind = "config_rewards"
)

// IsValid reports whether k is one of the twelve known rewards-gauge
// kinds. Mirrors the CHECK constraint in migration 0099.
func (k AquariusRewardsKind) IsValid() bool {
	switch k {
	case AquariusRewardsPoolState, AquariusRewardsClaimReward, AquariusRewardsSetRewardsConfig,
		AquariusRewardsPositionUpdate, AquariusRewardsDeposit, AquariusRewardsClaimFees,
		AquariusRewardsGaugeClaim, AquariusRewardsClaim, AquariusRewardsGaugeScheduleReward,
		AquariusRewardsSetRewardsState, AquariusRewardsGaugeAdd, AquariusRewardsConfigRewards:
		return true
	}
	return false
}

// AquariusRewardsEvent is one observed rewards-gauge event (any of
// the twelve kinds). UserAddress / Amount are universal promoted
// columns, NULL when the kind doesn't carry them; Attributes holds
// the kind-specific remainder (i128 fields as decimal strings —
// NUMERIC inside jsonb is lossy per ADR-0003).
type AquariusRewardsEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	Kind            AquariusRewardsKind
	UserAddress     string // "" when the kind carries none
	Amount          *canonical.Amount
	Attributes      map[string]any
}

// InsertAquariusRewardsEvent appends one rewards-gauge event to
// aquarius_rewards_events. Idempotent on the (ledger_close_time,
// contract_id, ledger, tx_hash, op_index, event_kind, event_index) PK
// — a projector-replay over the same range writes the same rows
// (ON CONFLICT DO NOTHING).
func (s *Store) InsertAquariusRewardsEvent(ctx context.Context, e AquariusRewardsEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertAquariusRewardsEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertAquariusRewardsEvent: TxHash is empty")
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("timescale: InsertAquariusRewardsEvent: invalid Kind %q", e.Kind)
	}

	attrs := e.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusRewardsEvent: marshal attributes: %w", err)
	}

	var amount sql.NullString
	if e.Amount != nil {
		if e.Amount.Sign() < 0 {
			return fmt.Errorf("timescale: InsertAquariusRewardsEvent: amount must be >= 0 (got %s)", e.Amount)
		}
		amount = sql.NullString{String: e.Amount.String(), Valid: true}
	}

	const q = `
        INSERT INTO aquarius_rewards_events (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, event_kind, user_address, amount,
            attributes
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_kind, event_index) DO NOTHING
    `
	_, err = s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), string(e.Kind),
		nullString(e.UserAddress), amount,
		attrsJSON,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusRewardsEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}
