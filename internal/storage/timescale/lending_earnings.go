package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// LendingClaimFold is the emissions one account claimed from one lending pool, in the pool's emission token base units.
type LendingClaimFold struct {
	Pool    string
	Claimed string
}

// LendingBackstopTokenFold is one account's net backstop tokens for one (pool, tier): tokens deposited minus tokens
// withdrawn. Tier is "" for Blend.
type LendingBackstopTokenFold struct {
	Pool         string
	Tier         string
	NetDeposited string
	// LastLedger is the newest ledger of any of the account's backstop events for the (pool, tier), queueing included: every
	// one of them rewrites the account's backstop balance entry.
	LastLedger uint32
}

// LendingAccountFlows is the claim and backstop-token history of one account, per protocol.
type LendingAccountFlows struct {
	BlendClaims   []LendingClaimFold
	BLNTClaims    []LendingClaimFold
	BlendBackstop []LendingBackstopTokenFold
	BLNTBackstop  []LendingBackstopTokenFold
}

const (
	// blend_emissions_user_kind_ts_idx.
	blendClaimsByUserSQL = `
		SELECT pool, SUM(amount)::text
		  FROM blend_emissions
		 WHERE user_address = $1 AND event_kind = 'claim' AND amount IS NOT NULL
		 GROUP BY pool ORDER BY pool LIMIT $2`
	// blnt_events_user_ts_idx; a pool's claim event is emitted by the pool itself.
	blntClaimsByUserSQL = `
		SELECT contract_id, SUM(amount)::text
		  FROM blnt_events
		 WHERE user_address = $1 AND contract_role = 'pool' AND event_kind = 'claim' AND amount IS NOT NULL
		 GROUP BY contract_id ORDER BY contract_id LIMIT $2`
	// blend_backstop_events user index (migration 0107); amount is tokens in on deposit and tokens out on withdraw.
	blendBackstopTokensByUserSQL = `
		SELECT pool, '',
		       COALESCE(SUM(CASE WHEN event_kind = 'deposit' THEN amount
		                         WHEN event_kind = 'withdraw' THEN -amount END),0)::text,
		       MAX(ledger)
		  FROM blend_backstop_events
		 WHERE user_address = $1 AND pool IS NOT NULL
		 GROUP BY pool
		HAVING COUNT(*) FILTER (WHERE event_kind IN ('deposit','withdraw')) > 0
		 ORDER BY pool LIMIT $2`
	// amount is tokens_in on deposit and tokens_out on withdraw (blnt abi.go).
	blntBackstopTokensByUserSQL = `
		SELECT pool, tier,
		       COALESCE(SUM(CASE WHEN event_kind = 'deposit' THEN amount
		                         WHEN event_kind = 'withdraw' THEN -amount END),0)::text,
		       MAX(ledger)
		  FROM blnt_events
		 WHERE user_address = $1 AND contract_role = 'backstop' AND pool IS NOT NULL AND tier IS NOT NULL
		 GROUP BY pool, tier
		HAVING COUNT(*) FILTER (WHERE event_kind IN ('deposit','withdraw')) > 0
		 ORDER BY pool, tier LIMIT $2`
)

// LendingAccountFlows reads one account's emission claims and backstop token flows; every query is bound to the account.
func (s *Store) LendingAccountFlows(ctx context.Context, address string) (LendingAccountFlows, error) {
	var out LendingAccountFlows
	args := []any{address, positionsVenueLimit}
	var err error
	if out.BlendClaims, err = queryFold(ctx, s.db, "LendingAccountFlows blend claims", blendClaimsByUserSQL, args, scanClaimFold); err != nil {
		return out, err
	}
	if out.BLNTClaims, err = queryFold(ctx, s.db, "LendingAccountFlows blnt claims", blntClaimsByUserSQL, args, scanClaimFold); err != nil {
		return out, err
	}
	if out.BlendBackstop, err = queryFold(ctx, s.db, "LendingAccountFlows blend backstop", blendBackstopTokensByUserSQL, args, scanBackstopTokenFold); err != nil {
		return out, err
	}
	if out.BLNTBackstop, err = queryFold(ctx, s.db, "LendingAccountFlows blnt backstop", blntBackstopTokensByUserSQL, args, scanBackstopTokenFold); err != nil {
		return out, err
	}
	return out, nil
}

func scanClaimFold(rows *sql.Rows) (LendingClaimFold, error) {
	var f LendingClaimFold
	if err := rows.Scan(&f.Pool, &f.Claimed); err != nil {
		return f, fmt.Errorf("claim fold: %w", err)
	}
	return f, nil
}

func scanBackstopTokenFold(rows *sql.Rows) (LendingBackstopTokenFold, error) {
	var f LendingBackstopTokenFold
	var ledger int64
	if err := rows.Scan(&f.Pool, &f.Tier, &f.NetDeposited, &ledger); err != nil {
		return f, fmt.Errorf("backstop token fold: %w", err)
	}
	f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
	return f, nil
}
