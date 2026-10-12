package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// AquariusRewardsKind discriminates the twelve rewards-gauge event kinds (migration 0099).
type AquariusRewardsKind string

// Rewards-gauge event kinds — see internal/sources/aquarius/README.md
// for the per-kind lifetime counts + wire-shape citations.
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

// IsValid reports whether k is one of the twelve known rewards-gauge kinds.
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

// AquariusRewardsEvent is one rewards-gauge event (any of twelve kinds); UserAddress and Amount are NULL when the kind lacks them and i128 fields in Attributes are decimal strings because jsonb NUMERIC is lossy.
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

// InsertAquariusRewardsEvent appends one rewards-gauge event, guarded by `derive_generation <= EXCLUDED.derive_generation` so an equal-or-higher replay overwrites values and a lower one is refused.
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

	// Generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the reward `amount` (or user_address /
	// attributes) lands in place when its generation is >= the stored one; a
	// live gen-0 replay can never revert it.
	const q = `
        INSERT INTO aquarius_rewards_events (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, event_kind, user_address, amount,
            attributes, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_kind, event_index) DO UPDATE SET
            user_address      = EXCLUDED.user_address,
            amount            = EXCLUDED.amount,
            attributes        = EXCLUDED.attributes,
            derive_generation = EXCLUDED.derive_generation
          WHERE aquarius_rewards_events.derive_generation <= EXCLUDED.derive_generation
    `
	_, err = s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), string(e.Kind),
		nullString(e.UserAddress), amount,
		attrsJSON, s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusRewardsEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// ─── Read side: rewards-gauge analytics for the Aquarius bespoke block ───
//
// These reads serve the multi-million-row aquarius_rewards_events table for the Aquarius bespoke
// block (protocol_bespoke.go, aquariusRewardsBlocks), so each must stay index-bounded.

// aquariusRewardsAllKinds is the ordered set of the twelve rewards-gauge
// kinds — the migration-0099 census order (busiest kind first). Reused so
// the "Rewards events by kind" table renders in a stable order without an
// extra ORDER BY count(*) DESC (a genuine full-table sort).
var aquariusRewardsAllKinds = []AquariusRewardsKind{
	AquariusRewardsPoolState, AquariusRewardsClaimReward, AquariusRewardsSetRewardsConfig,
	AquariusRewardsPositionUpdate, AquariusRewardsDeposit, AquariusRewardsClaimFees,
	AquariusRewardsGaugeClaim, AquariusRewardsClaim, AquariusRewardsGaugeScheduleReward,
	AquariusRewardsSetRewardsState, AquariusRewardsGaugeAdd, AquariusRewardsConfigRewards,
}

// AquariusRewardsKindCount is one kind's LIFETIME (all-time, unwindowed) event count.
type AquariusRewardsKindCount struct {
	Kind   AquariusRewardsKind
	Events int64
}

// AquariusRewardsLifetimeByKind reads the lifetime per-kind event count for all twelve rewards-gauge kinds, in migration-0099 census order.
func (s *Store) AquariusRewardsLifetimeByKind(ctx context.Context) ([]AquariusRewardsKindCount, error) {
	kinds := make([]string, len(aquariusRewardsAllKinds))
	for i, k := range aquariusRewardsAllKinds {
		kinds[i] = string(k)
	}
	const q = `
		WITH kinds AS (
		    SELECT k, ordinality FROM unnest($1::text[]) WITH ORDINALITY AS t(k, ordinality)
		)
		SELECT kinds.k, COALESCE(agg.events, 0)
		  FROM kinds
		  LEFT JOIN LATERAL (
		         SELECT count(*) AS events
		           FROM aquarius_rewards_events
		          WHERE event_kind = kinds.k
		       ) agg ON true
		 ORDER BY kinds.ordinality`
	rows, err := s.db.QueryContext(ctx, q, kinds)
	if err != nil {
		return nil, fmt.Errorf("timescale: AquariusRewardsLifetimeByKind: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]AquariusRewardsKindCount, 0, len(kinds))
	for rows.Next() {
		var (
			c    AquariusRewardsKindCount
			kind string
		)
		if err := rows.Scan(&kind, &c.Events); err != nil {
			return nil, fmt.Errorf("timescale: AquariusRewardsLifetimeByKind scan: %w", err)
		}
		c.Kind = AquariusRewardsKind(kind)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: AquariusRewardsLifetimeByKind rows: %w", err)
	}
	return out, nil
}

// AquariusClaimRewardWindow is the windowed claim_reward drill-down; volume exists only per reward token (ByToken) because tokens differ in decimals and have no price here.
type AquariusClaimRewardWindow struct {
	Events            int64
	DistinctClaimants int64
	ByToken           []AquariusClaimRewardTokenVolume
}

// AquariusClaimRewardTokenVolume is one reward token's share of the claim window; Amount is base units (canonical.Amount), never USD and never fed to VWAP, and RewardToken is "" when absent.
type AquariusClaimRewardTokenVolume struct {
	RewardToken string
	Events      int64
	Amount      canonical.Amount
}

// AquariusRewardsClaimWindow reads the windowed claim_reward summary (overall count and distinct claimants, plus per-reward-token count and summed amount) in one GROUPING SETS scan on aquarius_rewards_events_kind_ts_idx.
func (s *Store) AquariusRewardsClaimWindow(ctx context.Context, windowDays int) (*AquariusClaimRewardWindow, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	since := fmt.Sprintf("%d days", windowDays)

	rows, err := s.db.QueryContext(ctx, `
		SELECT GROUPING(reward_token) = 1, COALESCE(reward_token, ''), count(*),
		       COALESCE(sum(amount), 0)::text, count(DISTINCT user_address)
		  FROM (SELECT COALESCE(attributes->>'reward_token', '') AS reward_token, amount, user_address
		          FROM aquarius_rewards_events
		         WHERE event_kind = 'claim_reward' AND ledger_close_time > now() - $1::interval) w
		 GROUP BY GROUPING SETS ((reward_token), ())
		 ORDER BY 1 DESC, 3 DESC, 2 ASC`, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: AquariusRewardsClaimWindow: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out AquariusClaimRewardWindow
	for rows.Next() {
		var (
			isTotal   bool
			v         AquariusClaimRewardTokenVolume
			claimants int64
		)
		if err := rows.Scan(&isTotal, &v.RewardToken, &v.Events, &v.Amount, &claimants); err != nil {
			return nil, fmt.Errorf("timescale: AquariusRewardsClaimWindow scan: %w", err)
		}
		if isTotal {
			out.Events, out.DistinctClaimants = v.Events, claimants
			continue
		}
		out.ByToken = append(out.ByToken, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: AquariusRewardsClaimWindow rows: %w", err)
	}
	if out.Events == 0 {
		return nil, nil
	}
	return &out, nil
}

// AquariusRewardsDailyClaimSeries reads the daily claim_reward event count over the trailing windowDays (<= 0 means 90) on the same index predicate as AquariusRewardsClaimWindow.
func (s *Store) AquariusRewardsDailyClaimSeries(ctx context.Context, windowDays int) ([]BespokeSeriesPt, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)
	return s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), count(*)::text
		  FROM aquarius_rewards_events
		 WHERE event_kind = 'claim_reward' AND ledger_close_time > now() - $1::interval
		 GROUP BY 1 ORDER BY 1 ASC`, since)
}
