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
	Shares string
	Queued []BLNTQueuedWithdrawal // oldest first; nil when Shares is "" or ExpirationsUnknown
	// ExpirationsUnknown: a ledger mixed queueing with dequeueing or withdrawing across transactions, whose order within a ledger is
	// not stored, so which entries remain is unknown until the queue empties; QueuedShares is the exact queued total.
	ExpirationsUnknown bool
	QueuedShares       string
	LastActivity       time.Time
	LastLedger         uint32
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
               COALESCE(attributes->>'shares', ''), ledger_close_time, ledger, tx_hash::text
          FROM blnt_events
         WHERE user_address = $1 AND contract_role = 'backstop'
           AND event_kind IN ('deposit', 'queue_withdrawal', 'dequeue_withdrawal', 'withdraw', 'claim')
           AND pool IS NOT NULL AND tier IS NOT NULL
         ORDER BY ledger_close_time, ledger, tx_hash, op_index, event_index
         LIMIT $2`

type blntBackstopEvent struct {
	pool, tier, kind, amount2, expiration, claimShares, txHash string
	ts                                                         time.Time
	ledger                                                     int64
}

// BLNTBackstopByUser folds a user's BLNT backstop events into one row per (pool, tier), ordered by pool then tier.
func (s *Store) BLNTBackstopByUser(ctx context.Context, address string) ([]BLNTBackstopFold, error) {
	events, err := queryFold(ctx, s.db, "BLNTBackstopByUser", blntBackstopEventsSQL, []any{address, blntBackstopEventLimit},
		func(rows *sql.Rows) (blntBackstopEvent, error) {
			var e blntBackstopEvent
			err := rows.Scan(&e.pool, &e.tier, &e.kind, &e.amount2, &e.expiration, &e.claimShares, &e.ts, &e.ledger, &e.txHash)
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
	ambiguous  bool // queue composition unknown; see BLNTBackstopFold.ExpirationsUnknown
	last       time.Time
	lastLedger int64
}

// foldBLNTBackstop replays the contract's UserBalance: deposit and claim mint shares; queue moves shares into a FIFO queue entry,
// dequeue takes back from the newest entry, withdraw burns from the oldest (backstop/user.rs). Each ledger's events of one
// (pool, tier) are applied as a group, since their stored order is by tx hash, not execution.
func foldBLNTBackstop(events []blntBackstopEvent) []BLNTBackstopFold {
	byKey := map[[2]string][]blntBackstopEvent{}
	var order [][2]string
	for _, e := range events {
		k := [2]string{e.pool, e.tier}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], e)
	}
	slices.SortFunc(order, func(x, y [2]string) int {
		return cmp.Or(cmp.Compare(x[0], y[0]), cmp.Compare(blntTierRank[x[1]], blntTierRank[y[1]]))
	})
	out := make([]BLNTBackstopFold, 0, len(order))
	for _, k := range order {
		acc := &blntBackstopAcc{pool: k[0], tier: k[1], shares: new(big.Int)}
		evs := byKey[k]
		for len(evs) > 0 {
			n := 1
			for n < len(evs) && evs[n].ledger == evs[0].ledger {
				n++
			}
			acc.applyLedger(evs[:n])
			evs = evs[n:]
		}
		out = append(out, acc.fold())
	}
	return out
}

// applyLedger applies one ledger's events. Share totals are order-independent sums; only the queue's composition and an apparent
// underflow depend on order. When transactions mix queueing with taking, an as-stored underflow is replayed queues-first (the
// order that keeps the queue largest, so if it still underflows no order fits), and the composition is marked unknown.
func (a *blntBackstopAcc) applyLedger(group []blntBackstopEvent) {
	if mixesQueueAndTake(group) {
		a.ambiguous = true
		if a.underflows(group) {
			group = slices.Clone(group)
			slices.SortStableFunc(group, func(x, y blntBackstopEvent) int {
				return cmp.Compare(boolRank(x.kind != "queue_withdrawal"), boolRank(y.kind != "queue_withdrawal"))
			})
		}
	}
	for _, e := range group {
		a.apply(e)
	}
	if len(a.queue) == 0 {
		a.ambiguous = false
	}
	last := group[len(group)-1]
	a.last, a.lastLedger = last.ts.UTC(), last.ledger
}

// mixesQueueAndTake reports a queue and a dequeue or withdraw in different transactions; within one transaction order is known.
func mixesQueueAndTake(group []blntBackstopEvent) bool {
	queueTx, takeTx := map[string]bool{}, map[string]bool{}
	for _, e := range group {
		switch e.kind {
		case "queue_withdrawal":
			queueTx[e.txHash] = true
		case "dequeue_withdrawal", "withdraw":
			takeTx[e.txHash] = true
		}
	}
	for q := range queueTx {
		for t := range takeTx {
			if q != t {
				return true
			}
		}
	}
	return false
}

// underflows reports whether the group, as stored, takes more from the queue than it holds at some point.
func (a *blntBackstopAcc) underflows(group []blntBackstopEvent) bool {
	total := queueTotal(a.queue)
	for _, e := range group {
		n, ok := new(big.Int).SetString(e.amount2, 10)
		if !ok {
			continue
		}
		switch e.kind {
		case "queue_withdrawal":
			total.Add(total, n)
		case "dequeue_withdrawal", "withdraw":
			if total.Sub(total, n).Sign() < 0 {
				return true
			}
		}
	}
	return false
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
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
	return queueTotal(q).Cmp(n) >= 0
}

func queueTotal(q []blntQueueEntry) *big.Int {
	total := new(big.Int)
	for _, e := range q {
		total.Add(total, e.shares)
	}
	return total
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
	if a.ambiguous {
		f.ExpirationsUnknown, f.QueuedShares = true, queueTotal(a.queue).String()
		return f
	}
	for _, e := range a.queue {
		f.Queued = append(f.Queued, BLNTQueuedWithdrawal{Shares: e.shares.String(), Expiration: e.exp})
	}
	return f
}

var blntTierRank = map[string]int{"FirstLoss": 0, "SecondLoss": 1, "ThirdLoss": 2}
