package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the reward `amount` (or user_address /
	// attributes) lands in place when its generation is >= the stored one; a
	// live gen-0 replay can never revert it. Replaces the old DO NOTHING.
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
// The v0.12 decoders (this file's Insert side) shipped with nowhere
// serving the resulting 7.3M+-row full-history backfill. These reads back
// aquarius_rewards_events for internal/storage/timescale/protocol_bespoke.go
// (aquariusRewardsBlocks), which augments the Aquarius DEX bespoke block —
// see docs/protocols/aquarius.md "Rewards + governance analytics surface".

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

// AquariusRewardsKindCount is one kind's LIFETIME (all-time, unwindowed)
// event count + summed amount. Amount is 0 for kinds that never carry a
// user-facing amount (pool_state, set_rewards_config, position_update,
// rewards_gauge_schedule_reward, set_rewards_state, rewards_gauge_add,
// config_rewards) — see the per-kind doc in migration 0099.
type AquariusRewardsKindCount struct {
	Kind   AquariusRewardsKind
	Events int64
	Amount canonical.Amount
}

// AquariusRewardsLifetimeByKind reads the LIFETIME per-kind event count +
// summed amount for all twelve rewards-gauge kinds, in migration-0099
// census order.
//
// "Lifetime" over a multi-million-row hypertable would be a genuinely
// unbounded scan as a plain `GROUP BY event_kind` (visiting every row to
// build the twelve groups, regardless of index). Instead this LATERAL-joins
// each kind literal against aquarius_rewards_events, forcing Postgres to
// plan each of the twelve branches as its own `event_kind = <literal>`
// index scan against aquarius_rewards_events_kind_ts_idx (migration 0099) —
// bounded to that kind's own row range. The table's compression policy
// segments by (contract_id, event_kind) (migration 0099
// timescaledb.compress_segmentby), so on compressed chunks this predicate
// additionally lets TimescaleDB skip whole non-matching segments rather
// than decompressing them — the per-kind filter is the physically cheap
// access path here, not an approximation of one. Single round trip.
func (s *Store) AquariusRewardsLifetimeByKind(ctx context.Context) ([]AquariusRewardsKindCount, error) {
	kinds := make([]string, len(aquariusRewardsAllKinds))
	for i, k := range aquariusRewardsAllKinds {
		kinds[i] = string(k)
	}
	const q = `
		WITH kinds AS (
		    SELECT k, ordinality FROM unnest($1::text[]) WITH ORDINALITY AS t(k, ordinality)
		)
		SELECT kinds.k, COALESCE(agg.events, 0), COALESCE(agg.amount, '0')
		  FROM kinds
		  LEFT JOIN LATERAL (
		         SELECT count(*) AS events, COALESCE(sum(amount), 0)::text AS amount
		           FROM aquarius_rewards_events
		          WHERE event_kind = kinds.k
		       ) agg ON true
		 ORDER BY kinds.ordinality`
	rows, err := s.db.QueryContext(ctx, q, pq.Array(kinds))
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
		if err := rows.Scan(&kind, &c.Events, &c.Amount); err != nil {
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

// AquariusClaimRewardWindow is the windowed claim_reward drill-down —
// Aquarius's dominant user-facing rewards action (a user claiming accrued
// gauge rewards). Amount is reward-token base units (canonical.Amount,
// ADR-0003) — Aquarius reward tokens have no published price at this
// layer, so this is never USD and never feeds VWAP.
type AquariusClaimRewardWindow struct {
	Events            int64
	Amount            canonical.Amount
	DistinctClaimants int64
}

// AquariusRewardsClaimWindow reads the windowed claim_reward summary: event
// count, summed amount, and distinct claimant addresses. Bounded by BOTH
// event_kind = 'claim_reward' (equality, the leading column of
// aquarius_rewards_events_kind_ts_idx) AND ledger_close_time > now() -
// windowDays (the trailing range on that same index) — one sargable,
// fully-indexed predicate; neither side wraps the indexed columns in a
// function.
//
// Empty-safe: returns (nil, nil) when no claim_reward fired in the window.
// windowDays <= 0 is treated as 30 (the bespoke block's drill-down window).
func (s *Store) AquariusRewardsClaimWindow(ctx context.Context, windowDays int) (*AquariusClaimRewardWindow, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	since := fmt.Sprintf("%d days", windowDays)

	var out AquariusClaimRewardWindow
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0)::text, count(DISTINCT user_address)
		  FROM aquarius_rewards_events
		 WHERE event_kind = 'claim_reward' AND ledger_close_time > now() - $1::interval`, since).
		Scan(&out.Events, &out.Amount, &out.DistinctClaimants); err != nil {
		return nil, fmt.Errorf("timescale: AquariusRewardsClaimWindow: %w", err)
	}
	if out.Events == 0 {
		return nil, nil
	}
	return &out, nil
}

// AquariusRewardsDailyClaimSeries reads the daily claim_reward event count
// over the trailing windowDays — bounded/indexed identically to
// AquariusRewardsClaimWindow (event_kind equality + ledger_close_time range
// against aquarius_rewards_events_kind_ts_idx). windowDays <= 0 is treated
// as 90 (matching scanDailySeries's sibling callers in protocol_bespoke.go).
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
