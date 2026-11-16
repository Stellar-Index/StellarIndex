package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// LendingEventPos is the position of one lending pool event.
type LendingEventPos struct {
	Ts         time.Time
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
}

// LendingAuction is one Blend or BLNT auction: its new_auction event, the fills and the delete paired to it.
type LendingAuction struct {
	LendingEventPos
	AuctionType uint32
	User        string
	Percent     *uint32
	Block       *uint32
	Bid         map[string]string // asset C-strkey -> i128 decimal string
	Lot         map[string]string
	Fills       []LendingAuctionFill
	Deleted     *LendingEventPos
}

// LendingAuctionFill is one fill_auction paired to its auction.
type LendingAuctionFill struct {
	LendingEventPos
	Filler      string
	FillPercent string
}

// lendingAuctionLifecycleLimit caps the fill/delete read behind one page; a page that reaches it is reported truncated.
const lendingAuctionLifecycleLimit = 5000

// lendingAuctionsSQL pages both tables' new-auction events (a pool lives in one), newest first, each branch on its pool index.
const lendingAuctionsSQL = `
        SELECT * FROM (
            (SELECT ts, ledger, tx_hash::text, op_index, event_index, auction_type::bigint, user_address,
                    percent::bigint, block::bigint, COALESCE(bid::text, ''), COALESCE(lot::text, ''), false
               FROM blend_auctions
              WHERE pool = $1 AND event_kind = 'new'
                AND (ts, ledger, tx_hash::text, op_index, event_index) < ($2::timestamptz, $3::integer, $4::text, $5::integer, $6::integer)
              ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, event_index DESC
              LIMIT $7)
            UNION ALL
            (SELECT ledger_close_time, ledger, tx_hash::text, op_index, event_index, (attributes->>'auction_type')::bigint, user_address,
                    (attributes->>'percent')::bigint, (attributes#>>'{auction_data,block}')::bigint,
                    COALESCE(attributes#>>'{auction_data,bid}', ''), COALESCE(attributes#>>'{auction_data,lot}', ''), true
               FROM blnt_events
              WHERE contract_id = $1 AND contract_role = 'pool' AND event_kind = 'new_auction'
                AND (ledger_close_time, ledger, tx_hash::text, op_index, event_index) < ($2::timestamptz, $3::integer, $4::text, $5::integer, $6::integer)
              ORDER BY ledger_close_time DESC, ledger DESC, tx_hash DESC, op_index DESC, event_index DESC
              LIMIT $7)
        ) a
        ORDER BY 1 DESC, 2 DESC, 3 DESC, 4 DESC, 5 DESC
        LIMIT $7`

// lendingAuctionLifecycleSQL reads the auction events of the page's (auction_type, user) keys from its oldest auction on, oldest
// first, each key bounded at its next new auction after the page's newest one: later events belong to auctions off the page. A new
// auction starts at the next ledger (block = sequence + 1), so a same-ledger fill or delete belongs to the earlier auction: new
// sorts last.
const lendingAuctionLifecycleSQL = `
        WITH bounds AS (
            SELECT k.typ, k.usr, COALESCE(LEAST(
                       (SELECT min(ledger) FROM blend_auctions
                         WHERE pool = $1 AND event_kind = 'new' AND auction_type = k.typ AND user_address = k.usr AND ledger > $5),
                       (SELECT min(ledger) FROM blnt_events
                         WHERE contract_id = $1 AND contract_role = 'pool' AND event_kind = 'new_auction' AND user_address = k.usr
                           AND (attributes->>'auction_type')::bigint = k.typ AND ledger > $5)), 2147483647) AS max_ledger
              FROM unnest($2::bigint[], $3::text[]) AS k(typ, usr)
        )
        SELECT kind, ts, ledger, tx_hash, op_index, event_index, auction_type, user_address, filler, fill_percent FROM (
            SELECT a.event_kind AS kind, a.ts, a.ledger, a.tx_hash::text AS tx_hash, a.op_index, a.event_index,
                   a.auction_type::bigint AS auction_type, a.user_address, COALESCE(a.filler, '') AS filler,
                   COALESCE(a.fill_percent::text, '') AS fill_percent
              FROM blend_auctions a JOIN bounds b ON a.auction_type = b.typ AND a.user_address = b.usr
             WHERE a.pool = $1 AND a.ts >= $4 AND a.ledger <= b.max_ledger
            UNION ALL
            SELECT split_part(e.event_kind, '_', 1), e.ledger_close_time, e.ledger, e.tx_hash::text, e.op_index, e.event_index,
                   (e.attributes->>'auction_type')::bigint, e.user_address, COALESCE(e.attributes->>'filler', ''),
                   COALESCE(e.attributes->>'fill_percent', '')
              FROM blnt_events e JOIN bounds b ON (e.attributes->>'auction_type')::bigint = b.typ AND e.user_address = b.usr
             WHERE e.contract_id = $1 AND e.contract_role = 'pool' AND e.event_kind IN ('new_auction', 'fill_auction', 'delete_auction')
               AND e.ledger_close_time >= $4 AND e.ledger <= b.max_ledger
        ) e
        ORDER BY ts, ledger, CASE kind WHEN 'new' THEN 1 ELSE 0 END, tx_hash, op_index, event_index
        LIMIT $6`

// LendingAuctions returns a pool's auctions newest first, at most limit after cur (Kind is ignored), each with its fills and
// delete; truncated reports that the lifecycle read hit its cap, so the newest auctions may miss fills.
func (s *Store) LendingAuctions(ctx context.Context, pool string, limit int, cur LendingConfigCursor) (auctions []LendingAuction, truncated bool, err error) {
	after := LendingConfigCursor{Ts: endOfTime}
	if cur.IsSet() {
		after = cur
	}
	args := []any{pool, after.Ts.UTC(), int64(after.Ledger), after.TxHash, int64(after.OpIndex), int64(after.EventIndex), limit}
	auctions, err = queryFold(ctx, s.db, "LendingAuctions", lendingAuctionsSQL, args, scanLendingAuction)
	if err != nil || len(auctions) == 0 {
		return auctions, false, err
	}
	seen := map[auctionKey]bool{}
	var types []int64
	var users []string
	for _, a := range auctions {
		if k := (auctionKey{a.AuctionType, a.User}); !seen[k] {
			seen[k] = true
			types, users = append(types, int64(a.AuctionType)), append(users, a.User)
		}
	}
	oldest, newest := auctions[len(auctions)-1].Ts, auctions[0].Ledger
	events, err := queryFold(ctx, s.db, "LendingAuctionLifecycle", lendingAuctionLifecycleSQL,
		[]any{pool, types, users, oldest.UTC(), int64(newest), lendingAuctionLifecycleLimit}, scanAuctionLifecycle)
	if err != nil {
		return nil, false, err
	}
	pairAuctionLifecycle(auctions, events)
	return auctions, len(events) == lendingAuctionLifecycleLimit, nil
}

type auctionLifecycleEvent struct {
	LendingEventPos
	kind        string
	auctionType uint32
	user        string
	filler      string
	fillPercent string
}

type auctionKey struct {
	auctionType uint32
	user        string
}

// pairAuctionLifecycle attaches each fill and delete to the latest new auction of the same (auction_type, user) before it.
func pairAuctionLifecycle(auctions []LendingAuction, events []auctionLifecycleEvent) {
	byPos := make(map[LendingEventPos]*LendingAuction, len(auctions))
	for i := range auctions {
		byPos[auctions[i].LendingEventPos] = &auctions[i]
	}
	type ledgerKey struct {
		ledger uint32
		key    auctionKey
	}
	newIn := map[ledgerKey]LendingEventPos{}
	for _, e := range events {
		if e.kind == "new" {
			newIn[ledgerKey{e.Ledger, auctionKey{e.auctionType, e.user}}] = e.LendingEventPos
		}
	}
	open := map[auctionKey]*LendingAuction{} // nil: the open auction is not on this page
	closed := map[auctionKey]bool{}          // the open auction was deleted or filled to 100%
	deletedEarly := map[LendingEventPos]bool{}
	for _, e := range events {
		k := auctionKey{e.auctionType, e.user}
		a := open[k]
		switch e.kind {
		case "new":
			open[k], closed[k] = byPos[e.LendingEventPos], deletedEarly[e.LendingEventPos]
		case "fill":
			if a != nil {
				a.Fills = append(a.Fills, LendingAuctionFill{LendingEventPos: e.LendingEventPos, Filler: e.filler, FillPercent: e.fillPercent})
			}
			closed[k] = closed[k] || e.fillPercent == "100"
		case "delete":
			pos := e.LendingEventPos
			// Transaction order within a ledger is unknown: a delete after the open auction closed belongs to this ledger's new one.
			if next, ok := newIn[ledgerKey{e.Ledger, k}]; ok && closed[k] {
				deletedEarly[next] = true
				a = byPos[next]
			}
			if a != nil {
				a.Deleted = &pos
			}
			closed[k] = true
		}
	}
}

func scanEventPos(p *LendingEventPos, ledger, op, ev *int64) {
	p.Ts = p.Ts.UTC()
	p.Ledger, p.OpIndex, p.EventIndex = uint32(*ledger), uint32(*op), uint32(*ev) //nolint:gosec // non-negative int columns
}

func scanAuctionLifecycle(rows *sql.Rows) (auctionLifecycleEvent, error) {
	var e auctionLifecycleEvent
	var ledger, op, ev, typ int64
	if err := rows.Scan(&e.kind, &e.Ts, &ledger, &e.TxHash, &op, &ev, &typ, &e.user, &e.filler, &e.fillPercent); err != nil {
		return e, err
	}
	scanEventPos(&e.LendingEventPos, &ledger, &op, &ev)
	e.auctionType = uint32(typ) //nolint:gosec // auction_type is 0..3
	return e, nil
}

func scanLendingAuction(rows *sql.Rows) (LendingAuction, error) {
	var a LendingAuction
	var ledger, op, ev, typ int64
	var percent, block sql.NullInt64
	var bid, lot string
	var blntShape bool
	if err := rows.Scan(&a.Ts, &ledger, &a.TxHash, &op, &ev, &typ, &a.User, &percent, &block, &bid, &lot, &blntShape); err != nil {
		return a, err
	}
	scanEventPos(&a.LendingEventPos, &ledger, &op, &ev)
	a.AuctionType = uint32(typ) //nolint:gosec // auction_type is 0..3
	a.Percent, a.Block = nullU32(percent), nullU32(block)
	var err error
	if a.Bid, err = auctionAmounts(bid, blntShape); err != nil {
		return a, fmt.Errorf("bid: %w", err)
	}
	if a.Lot, err = auctionAmounts(lot, blntShape); err != nil {
		return a, fmt.Errorf("lot: %w", err)
	}
	return a, nil
}

func nullU32(n sql.NullInt64) *uint32 {
	if !n.Valid {
		return nil
	}
	v := uint32(n.Int64) //nolint:gosec // percent and block are u32 on the wire
	return &v
}

// auctionAmounts reads an asset -> amount map: blend_auctions stores [{asset, amount}], blnt_events the AuctionData map itself.
func auctionAmounts(raw string, blntShape bool) (map[string]string, error) {
	out := map[string]string{}
	if raw == "" {
		return out, nil
	}
	if blntShape {
		err := json.Unmarshal([]byte(raw), &out)
		return out, err
	}
	rows, err := decodeBlendAssetAmounts(raw)
	for _, r := range rows {
		out[r.Asset] = r.Amount
	}
	return out, err
}
