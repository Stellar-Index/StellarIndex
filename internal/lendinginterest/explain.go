package lendinginterest

import (
	"fmt"
	"math/big"
	"sort"
	"time"
)

// Cause is a pool action that moves a reserve's backstop_credit outside accrual and emits an event saying so.
type Cause string

// The causes Fold classifies a backstop residual by; CauseRules holds each one's rule and evidence.
const (
	CauseInterestFill   Cause = "interest_auction_fill"    // V2/BLNT fill_auction, auction_type 2
	CauseInterestFillV1 Cause = "interest_auction_fill_v1" // V1 fill_auction, auction_type 2: no lot in the body
	CauseGulp           Cause = "gulp"
	CauseReconcileLoss  Cause = "reconcile_loss" // BLNT; its backstop_credit_loss leg
	// CauseUndecodable marks a pool's cause-topic event the caller could not decode. It explains nothing, and its
	// transaction's residuals stay unexplained: the events left may sum to a residual by chance.
	CauseUndecodable Cause = "undecodable"
)

// CauseRule is how one cause's event moves backstop_credit, and where that is read from.
type CauseRule struct {
	Cause     Cause
	Protocols []Protocol
	Event     string // topic[0]
	// Checkable: the event carries the exact signed backstop_credit change of each asset it names. Otherwise
	// a same-transaction event attributes a draw but never explains it.
	Checkable bool
	Rule      string
	Evidence  string
}

// CauseRules is every cause Fold accepts. A cause absent here leaves its residuals unexplained.
var CauseRules = []CauseRule{
	{
		Cause: CauseInterestFill, Protocols: []Protocol{BlendV2, BLNT}, Event: "fill_auction", Checkable: true,
		Rule: "−lot[asset] of filled_auction_data: no rounding, the reserve is loaded with accrual first",
		Evidence: "blend-contracts-v2 v2.0.0 pool/src/auctions/backstop_interest_auction.rs fill_interest_auction " +
			"(backstop_credit -= lot_amount over the filled lot pool/src/pool/actions.rs emits); BLNT 5d055ca " +
			"pool/src/auctions/tier_interest.rs (same, AuctionType::InterestAuction = 2)",
	},
	{
		Cause: CauseInterestFillV1, Protocols: []Protocol{BlendV1}, Event: "fill_auction",
		Rule:     "attributed only: the body is (filler, fill_percent), the lot is not emitted",
		Evidence: "blend-contracts v1.0.0 pool/src/pool/actions.rs; golden V1 ledger 53013801",
	},
	{
		Cause: CauseGulp, Protocols: []Protocol{BlendV2, BLNT}, Event: "gulp", Checkable: true,
		Rule:     "+token_delta: no rounding; a non-positive delta stores nothing and emits 0",
		Evidence: "blend-contracts-v2 v2.0.0 pool/src/pool/gulp.rs execute_gulp, contract.rs gulp; BLNT 5d055ca pool/src/pool/gulp.rs",
	},
	{
		Cause: CauseReconcileLoss, Protocols: []Protocol{BLNT}, Event: "reconcile_loss", Checkable: true,
		Rule: "−backstop_credit_loss: no rounding; its supplier and protocol-credit legs move b_rate and the fee " +
			"entry, so such a version stays unexplained",
		Evidence: "BLNT 5d055ca pool/src/pool/gulp.rs execute_reconcile_loss",
	},
}

func causeRule(c Cause, p Protocol) (CauseRule, bool) {
	for _, r := range CauseRules {
		if r.Cause == c {
			for _, rp := range r.Protocols {
				if rp == p {
					return r, true
				}
			}
		}
	}
	return CauseRule{}, false
}

// Event is one decoded pool event of a cause, the caller having gated it on the pool's contract id.
type Event struct {
	Pool       string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	CloseTime  time.Time
	Cause      Cause
	// Asset and Amount are the signed backstop_credit change the event states for one asset; empty and nil
	// for a cause that is not Checkable. One event naming several assets is one Event per asset.
	Asset  string
	Amount *big.Int
}

type txKey struct {
	pool, tx string
	ledger   uint32
	op       uint32
}

// txRef is one pool's transaction, whatever the operation.
type txRef struct {
	pool, tx string
	ledger   uint32
}

func poisonedTxs(idx map[txKey][]Event) map[txRef]bool {
	out := map[txRef]bool{}
	for k, es := range idx {
		for _, e := range es {
			if e.Cause == CauseUndecodable {
				out[txRef{k.pool, k.tx, k.ledger}] = true
			}
		}
	}
	return out
}

type eventID struct {
	txKey
	index uint32
	asset string
}

func (e Event) id() eventID {
	return eventID{txKey{e.Pool, e.TxHash, e.Ledger, e.OpIndex}, e.EventIndex, e.Asset}
}

func checkEvent(e Event, opts Options, committedBefore time.Time) error {
	where := fmt.Sprintf("lendinginterest: pool %s ledger %d tx %s event %d", e.Pool, e.Ledger, e.TxHash, e.EventIndex)
	rule, ok := causeRule(e.Cause, opts.Protocols[e.Pool])
	if _, known := opts.Protocols[e.Pool]; known && e.Cause == CauseUndecodable {
		ok = true // rule stays zero: not Checkable, so the marker carries no amount
	}
	switch {
	case !ok:
		return fmt.Errorf("%s: cause %q is not one for a %q pool", where, e.Cause, opts.Protocols[e.Pool])
	case e.CloseTime.IsZero() || e.TxHash == "":
		return fmt.Errorf("%s: no close time or transaction", where)
	case e.CloseTime.UTC().Truncate(time.Hour).Before(committedBefore):
		return fmt.Errorf("%s: event in an hour already committed", where)
	case rule.Checkable && (e.Asset == "" || e.Amount == nil):
		return fmt.Errorf("%s: %s without an asset amount", where, e.Cause)
	case !rule.Checkable && (e.Asset != "" || e.Amount != nil):
		return fmt.Errorf("%s: %s carries an amount it cannot have", where, e.Cause)
	}
	return nil
}

// indexEvents checks and de-duplicates the events by identity, and groups them by transaction; a re-read is one
// event, two contents at one identity refuse the call.
func indexEvents(all []Event, opts Options, committedBefore time.Time) (map[txKey][]Event, error) {
	seen := map[eventID]Event{}
	idx := map[txKey][]Event{}
	for _, e := range all {
		if err := checkEvent(e, opts, committedBefore); err != nil {
			return nil, err
		}
		if prev, ok := seen[e.id()]; ok {
			if prev.Cause != e.Cause || !eqInt(prev.Amount, e.Amount) || !prev.CloseTime.Equal(e.CloseTime) {
				return nil, fmt.Errorf("lendinginterest: pool %s ledger %d tx %s event %d stored with two contents",
					e.Pool, e.Ledger, e.TxHash, e.EventIndex)
			}
			continue
		}
		seen[e.id()] = e
		k := e.id().txKey
		idx[k] = append(idx[k], e)
	}
	return idx, nil
}

// pendingEvents are the events of hours not yet committed, sorted, for the carry.
func pendingEvents(idx map[txKey][]Event, commitBefore time.Time) []Event {
	var out []Event
	for _, es := range idx {
		for _, e := range es {
			if !e.CloseTime.UTC().Truncate(time.Hour).Before(commitBefore) {
				e.Amount = cloneInt(e.Amount)
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].id(), out[j].id()
		switch {
		case a.ledger != b.ledger:
			return a.ledger < b.ledger
		case a.tx != b.tx:
			return a.tx < b.tx
		case a.pool != b.pool:
			return a.pool < b.pool
		case a.index != b.index:
			return a.index < b.index
		}
		return a.asset < b.asset
	})
	return out
}

type residualClass uint8

const (
	unexplained residualClass = iota
	explained
	attributed
)

// explain classifies one unreconciled version by the events of its own transaction. Explained: b_rate and the
// fee entry reconcile, and the Checkable events' changes for the asset sum to the backstop residual exactly.
// Attributed: the same, but the transaction carries a cause whose amount is not emitted and the residual is a
// draw. Anything else, including a version whose transaction the apply order leaves open or holds an undecodable
// event, stays unexplained. Only a classified version's ledger needs its events proved complete.
func (st *state) explain(row *HourRow, nexts []Version, residual *big.Int, onlyBackstop bool) {
	v := nexts[0]
	class := unexplained
	k := txKey{v.Pool, v.TxHash, v.Ledger, v.OpIndex}
	used := eventID{txKey: k, asset: v.Asset}
	if onlyBackstop && residual.Sign() != 0 && oneTx(nexts) && !st.used[used] && !st.poisoned[txRef{v.Pool, v.TxHash, v.Ledger}] {
		row.ResidualLedgers = appendLedger(row.ResidualLedgers, v.Ledger)
		class = classify(st.events[k], v.Asset, residual)
	}
	switch class {
	case explained:
		st.used[used] = true
		row.ExplainedVersions++
		row.ResidualExplained.Add(row.ResidualExplained, residual)
	case attributed:
		st.used[used] = true
		row.AttributedVersions++
		row.ResidualAttributed.Add(row.ResidualAttributed, residual)
	default:
		row.UnexplainedVersions++
		row.ResidualUnexplained.Add(row.ResidualUnexplained, residual)
	}
}

func classify(es []Event, asset string, residual *big.Int) residualClass {
	sum, checkable, uncheckable := new(big.Int), false, false
	for _, e := range es {
		switch {
		case e.Amount == nil:
			uncheckable = true
		case e.Asset == asset:
			checkable = true
			sum.Add(sum, e.Amount)
		}
	}
	switch {
	case uncheckable && residual.Sign() < 0:
		return attributed
	case !uncheckable && checkable && sum.Cmp(residual) == 0:
		return explained
	}
	return unexplained
}

func oneTx(vs []Version) bool {
	for _, v := range vs[1:] {
		if v.TxHash != vs[0].TxHash || v.OpIndex != vs[0].OpIndex {
			return false
		}
	}
	return true
}

func appendLedger(ls []uint32, l uint32) []uint32 {
	if n := len(ls); n > 0 && ls[n-1] == l {
		return ls
	}
	return append(ls, l)
}
