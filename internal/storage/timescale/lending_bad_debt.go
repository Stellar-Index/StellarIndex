package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kinds of LendingBadDebtEvent.
const (
	BadDebtKindMoved     = "bad_debt"       // a user's d-tokens moved to the backstop
	BadDebtKindDefaulted = "defaulted_debt" // the backstop's d-tokens burnt, the loss socialised to suppliers
	BadDebtKindFill      = "fill"           // one bid entry of a bad-debt auction fill: d-tokens the filler took on
	// BadDebtKindFillZeroBid is a fill whose filled auction data was on the wire with an empty bid: it covered nothing.
	BadDebtKindFillZeroBid = "fill_zero_bid"
	// BadDebtKindFillBidUnrecorded is a fill whose event carries no auction data (Blend V1): its bid is unknown.
	BadDebtKindFillBidUnrecorded = "fill_bid_unrecorded"
	BadDebtKindRepay             = "repay" // a repay of the backstop's own d-tokens
)

// LendingBadDebtEventCap bounds one pool's bad-debt event read; a pool beyond it is refused, never summed short.
const LendingBadDebtEventCap = 20000

// ErrLendingBadDebtCap is a pool with more bad-debt events than LendingBadDebtEventCap.
var ErrLendingBadDebtCap = errors.New("timescale: lending bad-debt events exceed the read cap")

// LendingBadDebtEvent is one event that moves d-tokens into or out of a pool's backstop position. DTokens is the decimal d-token
// amount; "" when the event did not record one (and always for the two fill kinds that carry no bid entry).
type LendingBadDebtEvent struct {
	Kind    string
	Asset   string
	DTokens string
	Ledger  uint32
	TxHash  string
	Ts      time.Time
}

// blendBadDebtEventsSQL: each branch is bounded by pool and event kind on its pool-leading index and compression segment.
// InsertBlendFillAuction stores block NULL exactly when the event carried no auction data, and a NULL bid beside a stored block
// exactly when the bid map was empty, so the two fill kinds below are read from the writer's encoding, not inferred.
const blendBadDebtEventsSQL = `
	SELECT event_kind, COALESCE(asset, ''), COALESCE(amount::text, ''), ledger, tx_hash::text, ledger_close_time
	  FROM blend_emissions
	 WHERE pool = $1 AND event_kind IN ('bad_debt', 'defaulted_debt')
	UNION ALL
	SELECT CASE WHEN a.block IS NULL THEN 'fill_bid_unrecorded' WHEN x.elem IS NULL THEN 'fill_zero_bid' ELSE 'fill' END,
	       COALESCE(x.elem->>'asset', ''), COALESCE(x.elem->>'amount', ''), a.ledger, a.tx_hash::text, a.ts
	  FROM blend_auctions a
	  LEFT JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(a.bid) = 'array' THEN a.bid END) AS x(elem) ON true
	 WHERE a.pool = $1 AND a.auction_type = 1 AND a.event_kind = 'fill'
	UNION ALL
	SELECT 'repay', asset, COALESCE(b_or_d_amount::text, ''), ledger, tx_hash::text, ledger_close_time
	  FROM blend_positions
	 WHERE pool = $1 AND user_address = $2 AND event_kind = 'repay' AND $2 <> ''
	 ORDER BY 4, 5, 1, 2
	 LIMIT $3`

// blntBadDebtEventsSQL is blendBadDebtEventsSQL over blnt_events, whose filled_auction_data bid is an object keyed by asset; a
// fill without that object is unrecorded, an empty object covered nothing. No repay branch: BLNT's backstop role is unverified.
const blntBadDebtEventsSQL = `
	SELECT event_kind, COALESCE(asset, ''), COALESCE(amount::text, ''), ledger, tx_hash::text, ledger_close_time
	  FROM blnt_events
	 WHERE contract_id = $1 AND contract_role = 'pool' AND event_kind IN ('bad_debt', 'defaulted_debt')
	UNION ALL
	SELECT CASE WHEN jsonb_typeof(a.attributes->'filled_auction_data'->'bid') IS DISTINCT FROM 'object' THEN 'fill_bid_unrecorded'
	            WHEN b.key IS NULL THEN 'fill_zero_bid' ELSE 'fill' END,
	       COALESCE(b.key, ''), COALESCE(b.value, ''), a.ledger, a.tx_hash::text, a.ledger_close_time
	  FROM blnt_events a
	  LEFT JOIN LATERAL jsonb_each_text(CASE WHEN jsonb_typeof(a.attributes->'filled_auction_data'->'bid') = 'object'
	                                         THEN a.attributes->'filled_auction_data'->'bid' END) AS b ON true
	 WHERE a.contract_id = $1 AND a.contract_role = 'pool' AND a.event_kind = 'fill_auction'
	   AND a.attributes->>'auction_type' = '1'
	 ORDER BY 4, 5, 1, 2
	 LIMIT $2`

// LendingBadDebtEvents reads a pool's bad-debt events oldest first: bad_debt and defaulted_debt, every bad-debt auction fill, and
// (Blend only, when backstop is set) the repays of the backstop's own position. blnt selects blnt_events.
func (s *Store) LendingBadDebtEvents(ctx context.Context, pool, backstop string, blnt bool) ([]LendingBadDebtEvent, error) {
	q, args := blendBadDebtEventsSQL, []any{pool, backstop, LendingBadDebtEventCap + 1}
	if blnt {
		q, args = blntBadDebtEventsSQL, []any{pool, LendingBadDebtEventCap + 1}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: LendingBadDebtEvents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingBadDebtEvent
	for rows.Next() {
		var e LendingBadDebtEvent
		var ledger int64
		if err := rows.Scan(&e.Kind, &e.Asset, &e.DTokens, &ledger, &e.TxHash, &e.Ts); err != nil {
			return nil, fmt.Errorf("timescale: LendingBadDebtEvents scan: %w", err)
		}
		e.Ledger, e.Ts = uint32(ledger), e.Ts.UTC() //nolint:gosec // ledger is a non-negative integer column
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LendingBadDebtEvents rows: %w", err)
	}
	if len(out) > LendingBadDebtEventCap {
		return nil, ErrLendingBadDebtCap
	}
	return out, nil
}
