package timescale

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"time"
)

// BLNTBackstopFold is one (pool, tier) backstop position of a user folded from blnt_events.
type BLNTBackstopFold struct {
	Pool string
	Tier string
	// Shares is every share the user owns, queued ones included; "" when the history is inconsistent (a dequeue or withdraw
	// larger than the reconstructed queue), never a guessed number.
	Shares       string
	Queued       []BLNTQueuedWithdrawal // oldest first; nil when Shares is ""
	LastActivity time.Time
	LastLedger   uint32
}

// BLNTQueuedWithdrawal is one queued backstop withdrawal: its shares and when they unlock.
type BLNTQueuedWithdrawal struct {
	Shares     string
	Expiration time.Time
}

// blntBackstopEventLimit bounds one user's backstop history; a longer one fails the fold rather than serve a truncated sum.
const blntBackstopEventLimit = 5000

// blntBackstopEventsSQL reads the share-moving backstop events of a user on the (user_address, ledger_close_time) index. draw and
// donate change tokens per share, not a user's shares, so they are not read.
const blntBackstopEventsSQL = `
        SELECT pool, tier, event_kind, COALESCE(amount2::text, ''), COALESCE(attributes->>'expiration', ''),
               COALESCE(attributes->>'shares', ''), ledger_close_time, ledger
          FROM blnt_events
         WHERE user_address = $1 AND contract_role = 'backstop'
           AND event_kind IN ('deposit', 'queue_withdrawal', 'dequeue_withdrawal', 'withdraw', 'claim')
           AND pool IS NOT NULL AND tier IS NOT NULL
         ORDER BY ledger_close_time, ledger, tx_hash, op_index, event_index
         LIMIT $2`

type blntBackstopEvent struct {
	pool, tier, kind, amount2, expiration, claimShares string
	ts                                                 time.Time
	ledger                                             int64
}

// BLNTBackstopByUser folds a user's BLNT backstop events into one row per (pool, tier), ordered by pool then tier.
func (s *Store) BLNTBackstopByUser(ctx context.Context, address string) ([]BLNTBackstopFold, error) {
	events, err := queryFold(ctx, s.db, "BLNTBackstopByUser", blntBackstopEventsSQL, []any{address, blntBackstopEventLimit},
		func(rows *sql.Rows) (blntBackstopEvent, error) {
			var e blntBackstopEvent
			err := rows.Scan(&e.pool, &e.tier, &e.kind, &e.amount2, &e.expiration, &e.claimShares, &e.ts, &e.ledger)
			return e, err
		})
	if err != nil {
		return nil, err
	}
	if len(events) == blntBackstopEventLimit {
		return nil, fmt.Errorf("timescale: BLNTBackstopByUser: more than %d events", blntBackstopEventLimit-1)
	}
	return foldBLNTBackstop(events), nil
}

type blntQueueEntry struct {
	shares *big.Int
	exp    time.Time
}

type blntBackstopAcc struct {
	pool, tier string
	shares     *big.Int // owned shares, queued included
	queue      []blntQueueEntry
	broken     bool
	last       time.Time
	lastLedger int64
}

// foldBLNTBackstop replays the contract's UserBalance: deposit and claim mint shares; queue moves shares into a FIFO queue entry,
// dequeue takes back from the newest entry, withdraw burns from the oldest (backstop/user.rs).
func foldBLNTBackstop(events []blntBackstopEvent) []BLNTBackstopFold {
	accs := map[[2]string]*blntBackstopAcc{}
	var order [][2]string
	for _, e := range events {
		k := [2]string{e.pool, e.tier}
		acc := accs[k]
		if acc == nil {
			acc = &blntBackstopAcc{pool: e.pool, tier: e.tier, shares: new(big.Int)}
			accs[k] = acc
			order = append(order, k)
		}
		acc.apply(e)
		acc.last, acc.lastLedger = e.ts.UTC(), e.ledger
	}
	slices.SortFunc(order, func(x, y [2]string) int {
		return cmp.Or(cmp.Compare(x[0], y[0]), cmp.Compare(blntTierRank[x[1]], blntTierRank[y[1]]))
	})
	out := make([]BLNTBackstopFold, 0, len(order))
	for _, k := range order {
		out = append(out, accs[k].fold())
	}
	return out
}

func (a *blntBackstopAcc) apply(e blntBackstopEvent) {
	amt := e.amount2
	if e.kind == "claim" {
		amt = e.claimShares
	}
	n, ok := new(big.Int).SetString(amt, 10)
	if !ok || n.Sign() < 0 {
		a.broken = true
		return
	}
	switch e.kind {
	case "deposit", "claim":
		a.shares.Add(a.shares, n)
	case "queue_withdrawal":
		sec, err := strconv.ParseInt(e.expiration, 10, 64)
		a.broken = a.broken || err != nil
		a.queue = append(a.queue, blntQueueEntry{shares: n, exp: time.Unix(sec, 0).UTC()})
	case "dequeue_withdrawal", "withdraw":
		withdraw := e.kind == "withdraw"
		a.broken = a.broken || !queueCovers(a.queue, n)
		a.queue = takeFromQueue(a.queue, n, withdraw)
		if withdraw {
			a.shares.Sub(a.shares, n)
		}
	}
}

func queueCovers(q []blntQueueEntry, n *big.Int) bool {
	total := new(big.Int)
	for _, e := range q {
		total.Add(total, e.shares)
	}
	return total.Cmp(n) >= 0
}

// takeFromQueue removes n shares from the front (withdraw) or the back (dequeue), splitting the last entry it touches.
func takeFromQueue(q []blntQueueEntry, n *big.Int, front bool) []blntQueueEntry {
	left := new(big.Int).Set(n)
	for len(q) > 0 && left.Sign() > 0 {
		i := len(q) - 1
		if front {
			i = 0
		}
		if q[i].shares.Cmp(left) > 0 {
			q[i].shares = new(big.Int).Sub(q[i].shares, left)
			break
		}
		left.Sub(left, q[i].shares)
		if front {
			q = q[1:]
		} else {
			q = q[:i]
		}
	}
	return q
}

func (a *blntBackstopAcc) fold() BLNTBackstopFold {
	f := BLNTBackstopFold{Pool: a.pool, Tier: a.tier, LastActivity: a.last, LastLedger: uint32(a.lastLedger)} //nolint:gosec // ledger seq fits uint32
	if a.broken || a.shares.Sign() < 0 {
		return f
	}
	f.Shares = a.shares.String()
	for _, e := range a.queue {
		f.Queued = append(f.Queued, BLNTQueuedWithdrawal{Shares: e.shares.String(), Expiration: e.exp})
	}
	return f
}

var blntTierRank = map[string]int{"FirstLoss": 0, "SecondLoss": 1, "ThirdLoss": 2}
