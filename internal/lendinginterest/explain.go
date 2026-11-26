package lendinginterest

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// Cause is a pool action that moves a reserve's backstop_credit outside accrual and emits an event saying so.
type Cause string

// The causes Fold classifies a backstop residual by; CauseRules holds each one's rule and evidence.
const (
	CauseInterestFill   Cause = "interest_auction_fill"    // V2/BLNT fill_auction, auction_type 2
	CauseInterestFillV1 Cause = "interest_auction_fill_v1" // V1 fill_auction, auction_type 2: no lot in the body
	CauseGulp           Cause = "gulp"
	CauseReconcileLoss  Cause = "reconcile_loss" // BLNT; its backstop_credit_loss leg
	// CauseDefaultedDebt cuts b_rate, not backstop_credit: it explains a b_rate mismatch, never a backstop residual.
	CauseDefaultedDebt Cause = "defaulted_debt"
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
	// DTokens: the event carries the d-tokens it burns from one asset's reserve, which cut its b_rate.
	DTokens  bool
	Rule     string
	Evidence string
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
	{
		Cause: CauseDefaultedDebt, Protocols: []Protocol{BlendV2}, Event: "defaulted_debt", DTokens: true,
		Rule: "b_rate −= ceil(ceil(d_tokens_burnt·d_rate/S)·S/b_supply), floored at 0, after the accrual, per event " +
			"(blend.DefaultedBRate), divided by the pre-transaction b_supply: a supply or withdraw before the cut breaks " +
			"the equality and the version stays unexplained",
		Evidence: "blend-contracts-v2 v2.0.0 pool/src/pool/user.rs default_liabilities; reproduces all 8 mainnet versions " +
			"in internal/sources/blend/testdata/defaulted_debt (TestDefaultedBRate_ReproducesMainnet)",
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
	// DTokens is a defaulted_debt's d_tokens_burnt for Asset; nil for every other cause.
	DTokens *big.Int
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
	case rule.DTokens != (e.DTokens != nil) || (rule.DTokens && (e.Asset == "" || e.DTokens.Sign() < 0 || e.Amount != nil)):
		return fmt.Errorf("%s: %s with d-tokens %v for asset %q", where, e.Cause, e.DTokens, e.Asset)
	case rule.Checkable && (e.Asset == "" || e.Amount == nil):
		return fmt.Errorf("%s: %s without an asset amount", where, e.Cause)
	case !rule.Checkable && !rule.DTokens && (e.Asset != "" || e.Amount != nil):
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
			if prev.Cause != e.Cause || !eqInt(prev.Amount, e.Amount) || !eqInt(prev.DTokens, e.DTokens) ||
				!prev.CloseTime.Equal(e.CloseTime) {
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
				e.Amount, e.DTokens = cloneInt(e.Amount), cloneInt(e.DTokens)
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

// explain classifies one unreconciled version by the events of its own transaction. Explained: b_rate reconciles
// (with accrual, or with the transaction's defaults), the fee entry does, and the Checkable events' changes for the asset sum to the backstop residual exactly.
// Attributed: the same, but the transaction carries a cause whose amount is not emitted and the residual is a
// draw. Anything else, including a version whose transaction the apply order leaves open or holds an undecodable
// event, stays unexplained. defaultLoss is the version's supply residual when its b_rate mismatch is the
// transaction's defaults (st.defaulted); it counts only if the version is classified. Only a classified version's
// ledger needs its events proved complete.
func (st *state) explain(row *HourRow, nexts []Version, residual *big.Int, onlyBackstop bool, defaultLoss *big.Int) {
	v := nexts[0]
	class := unexplained
	k := txKey{v.Pool, v.TxHash, v.Ledger, v.OpIndex}
	used := eventID{txKey: k, asset: v.Asset}
	byEvents := residual.Sign() != 0 || defaultLoss != nil
	if onlyBackstop && byEvents && oneTx(nexts) && !st.used[used] && !st.poisoned[txRef{v.Pool, v.TxHash, v.Ledger}] {
		row.ResidualLedgers = appendLedger(row.ResidualLedgers, v.Ledger)
		class = explained
		// A default's zero residual still classifies its asset's amounts: a fill and a gulp may net to it.
		if residual.Sign() != 0 || hasAmounts(st.events[k], v.Asset) {
			class = classify(st.events[k], v.Asset, residual)
		}
	}
	switch class {
	case explained:
		row.ExplainedVersions++
		row.ResidualExplained.Add(row.ResidualExplained, residual)
		for _, e := range st.events[k] {
			if c, ok := causeColumns[e.Cause]; ok && e.Asset == v.Asset {
				c.col(row).Add(c.col(row), e.Amount)
			}
		}
	case attributed:
		row.AttributedVersions++
		row.ResidualAttributed.Add(row.ResidualAttributed, residual)
	default:
		row.UnexplainedVersions++
		row.ResidualUnexplained.Add(row.ResidualUnexplained, residual)
		return
	}
	st.used[used] = true
	if defaultLoss != nil {
		row.DefaultVersions++
		row.DefaultLoss.Add(row.DefaultLoss, defaultLoss)
	}
}

// defaulted reports whether the version's b_rate is exactly the accrual's expected b_rate cut by each defaulted_debt
// its own transaction emits for the asset, dividing by prev's b_supply. Cuts read neither b_rate nor each other, so
// their order cannot matter. A supply or withdraw after the cut moves b_supply but not b_rate; one before it changes
// the divisor, and then the exact b_rate does not match. Only a V2 pool can hold the event (checkEvent).
func (st *state) defaulted(prev blend.ReserveData, nexts []Version, expected *big.Int) bool {
	v := nexts[0]
	if len(nexts) != 1 {
		return false
	}
	rd := blend.ReserveData{DRate: v.Reserve.DRate, BRate: expected, BSupply: prev.BSupply, Version: prev.Version}
	n := 0
	for _, e := range st.events[txKey{v.Pool, v.TxHash, v.Ledger, v.OpIndex}] {
		if e.Cause != CauseDefaultedDebt || e.Asset != v.Asset {
			continue
		}
		b, err := blend.DefaultedBRate(rd, e.DTokens)
		if err != nil {
			return false
		}
		rd.BRate, n = b, n+1
	}
	return n > 0 && rd.BRate.Cmp(v.Reserve.BRate) == 0
}

// causeColumn is the HourRow column a Checkable cause's amount sums into within ResidualExplained, and the sign
// (-1: ≤ 0, +1: ≥ 0) its CauseRule allows that amount.
type causeColumn struct {
	col  func(*HourRow) *big.Int
	sign int
}

var causeColumns = map[Cause]causeColumn{
	CauseInterestFill:  {func(r *HourRow) *big.Int { return r.InterestDrawn }, -1},
	CauseGulp:          {func(r *HourRow) *big.Int { return r.Gulped }, +1},
	CauseReconcileLoss: {func(r *HourRow) *big.Int { return r.ReconcileBackstopLoss }, -1},
}

func hasAmounts(es []Event, asset string) bool {
	for _, e := range es {
		if e.Amount != nil && e.Asset == asset {
			return true
		}
	}
	return false
}

func classify(es []Event, asset string, residual *big.Int) residualClass {
	sum, checkable, uncheckable := new(big.Int), false, false
	for _, e := range es {
		// checkEvent admits an amount only on a Checkable cause, each of which has a column (the lockstep test).
		c := causeColumns[e.Cause]
		switch {
		case e.Cause == CauseDefaultedDebt:
		case e.Amount == nil:
			uncheckable = true
		case e.Asset == asset && e.Amount.Sign() == -c.sign:
			return unexplained // a sign its rule cannot produce explains nothing
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
