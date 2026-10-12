package timescale

import (
	"context"
	"fmt"
)

// This file is the Postgres read side for GET /v1/accounts/{g_strkey}/activity. Two reads:
//
//   - DefiActionCountsByUser: per-(protocol, action) event counts over the per-account served-tier
//     tables the positions endpoint folds (positions.go), plus blend_auctions.
//   - BridgeActivityByAddress: rozo_events + cctp_events per-direction counts.
//
// Tables deliberately NOT counted carry no per-account column: phoenix_liquidity,
// aquarius_liquidity/admin and comet_liquidity are pool-level, soroswap_* captures no acting account,
// and defindex strategy-layer rows have the vault contract as actor, which the actor = $1 predicate
// excludes naturally since $1 is a G-strkey.

// DefiActionCount is one (protocol, action) count for an address.
type DefiActionCount struct {
	Protocol string
	Action   string
	Count    int64
}

// defiActionCountsQuery UNIONs one GROUP BY per per-account table. Every arm except blend_auctions
// rides the user-leading index its positions-fold sibling uses. blend_auctions has none but is a
// small lifecycle table (~10k rows), so a filtered scan is accepted. sorocredit's credit_events has
// no owner column, so its only per-account count is positions opened (credit_positions.owner).
const defiActionCountsQuery = `
	SELECT 'blend' AS protocol, event_kind AS action, count(*) AS n
	  FROM blend_positions WHERE user_address = $1 GROUP BY event_kind
	UNION ALL
	SELECT 'blend_backstop', event_kind, count(*)
	  FROM blend_backstop_events WHERE user_address = $1 GROUP BY event_kind
	UNION ALL
	SELECT 'blend_auctions', event_kind, count(*)
	  FROM blend_auctions WHERE user_address = $1 GROUP BY event_kind
	UNION ALL
	SELECT 'phoenix_stake', action, count(*)
	  FROM phoenix_stake_events WHERE user_addr = $1 GROUP BY action
	UNION ALL
	SELECT 'defindex', direction, count(*)
	  FROM defindex_flows WHERE actor = $1 AND layer = 'vault' GROUP BY direction
	UNION ALL
	SELECT 'sorocredit', 'position_opened', count(*)
	  FROM credit_positions WHERE owner = $1 HAVING count(*) > 0
	UNION ALL
	SELECT 'aquarius', event_kind, count(*)
	  FROM aquarius_rewards_events WHERE user_address = $1 GROUP BY event_kind
	ORDER BY protocol, n DESC`

// DefiActionCountsByUser returns the address's per-(protocol, action) DeFi event counts from served-tier tables with a per-account column; zero-count rows are omitted.
func (s *Store) DefiActionCountsByUser(ctx context.Context, address string) ([]DefiActionCount, error) {
	rows, err := s.db.QueryContext(ctx, defiActionCountsQuery, address)
	if err != nil {
		return nil, fmt.Errorf("timescale: DefiActionCountsByUser: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DefiActionCount
	for rows.Next() {
		var c DefiActionCount
		if err := rows.Scan(&c.Protocol, &c.Action, &c.Count); err != nil {
			return nil, fmt.Errorf("timescale: DefiActionCountsByUser scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: DefiActionCountsByUser rows: %w", err)
	}
	return out, nil
}

// BridgeActivity is an address's bridge-transfer counts across the two integrated bridge protocols.
type BridgeActivity struct {
	// Rozo v1 Payment (rozo_events): payments the address sent
	// (from_addr) vs received (destination).
	RozoOutbound int64
	RozoInbound  int64
	// Circle CCTP v2 (cctp_events): outbound deposit_for_burn rows
	// whose depositor is the address; inbound mint_and_withdraw rows
	// whose Stellar-side mint recipient is the address.
	CCTPOutboundBurns int64
	CCTPInboundMints  int64
}

// bridgeActivityQuery counts both bridge tables in one round-trip.
//
// Neither predicate is index-served, deliberately: rozo_events is a few hundred rows and
// cctp_events tens of thousands, far under where an index or a GIN over the jsonb would pay off.
// CCTP parties live inside the `attributes` jsonb (a storage blob, not a query surface):
//   - deposit_for_burn `depositor` is a Stellar Address strkey and matchable.
//   - mint_and_withdraw `mint_recipient` is a Stellar Address strkey and matchable, though often a
//     forwarder CONTRACT rather than the user's G-account, so that mint is not attributed to the
//     user (disclosed in the endpoint's note).
//   - deposit_for_burn `mint_recipient` is a DESTINATION-CHAIN 32-byte hex value, structurally not
//     a Stellar account, so inbound counts deliberately never match on it.
const bridgeActivityQuery = `
	SELECT
	  (SELECT count(*) FROM rozo_events WHERE event_type = 'payment' AND from_addr = $1),
	  (SELECT count(*) FROM rozo_events WHERE event_type = 'payment' AND destination = $1),
	  (SELECT count(*) FROM cctp_events
	    WHERE event_type = 'deposit_for_burn' AND attributes->>'depositor' = $1),
	  (SELECT count(*) FROM cctp_events
	    WHERE event_type = 'mint_and_withdraw' AND attributes->>'mint_recipient' = $1)`

// BridgeActivityByAddress returns the address's bridge-transfer counts; see bridgeActivityQuery for the matching semantics and limits.
func (s *Store) BridgeActivityByAddress(ctx context.Context, address string) (BridgeActivity, error) {
	var b BridgeActivity
	if err := s.db.QueryRowContext(ctx, bridgeActivityQuery, address).
		Scan(&b.RozoOutbound, &b.RozoInbound, &b.CCTPOutboundBurns, &b.CCTPInboundMints); err != nil {
		return BridgeActivity{}, fmt.Errorf("timescale: BridgeActivityByAddress: %w", err)
	}
	return b, nil
}
