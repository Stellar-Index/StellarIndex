package lendinginterest

import (
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

var (
	errNoConfig = errors.New("lendinginterest: no pool Config stored before the transaction")
	errNoFees   = errors.New("lendinginterest: no ProtocolFees state before the transaction")
)

type rowKey struct {
	pool, asset string
	hour        int64
}

type state struct {
	opts     Options
	reserves map[ReserveKey][]blend.ReserveData
	configs  map[string][]uint32
	fees     map[string][]blnt.ProtocolFees
	rows     map[rowKey]*HourRow
}

func newState(seed Seeds, opts Options) (*state, error) {
	st := &state{
		opts:     opts,
		reserves: make(map[ReserveKey][]blend.ReserveData, len(seed.Reserves)),
		configs:  make(map[string][]uint32, len(seed.Configs)),
		fees:     make(map[string][]blnt.ProtocolFees, len(seed.Fees)),
		rows:     map[rowKey]*HourRow{},
	}
	for k, v := range seed.Reserves {
		for _, rd := range v {
			if !complete(rd) {
				return nil, fmt.Errorf("lendinginterest: seed for pool %s asset %s has a missing field", k.Pool, k.Asset)
			}
		}
		if len(v) > 0 {
			st.reserves[k] = v
		}
	}
	for k, v := range seed.Configs {
		if len(v) > 0 {
			st.configs[k] = v
		}
	}
	for k, v := range seed.Fees {
		if len(v) > 0 {
			st.fees[k] = v
		}
	}
	return st, nil
}

func complete(rd blend.ReserveData) bool {
	return rd.DRate != nil && rd.BRate != nil && rd.BSupply != nil && rd.DSupply != nil && rd.BackstopCredit != nil
}

func (st *state) poolLedger(vs []Version) error {
	proto := st.opts.Protocols[vs[0].Pool]
	if unorderedLedger(vs) {
		st.unorderedLedger(proto, vs)
		return nil
	}
	groups, err := txGroups(vs)
	if err != nil {
		return err
	}
	for _, g := range groups {
		st.tx(proto, g)
	}
	return nil
}

// tx pairs one transaction's ResData versions. Its own Config does not count for them; its own
// ProtocolFees is their nextFee wherever it sits in the transaction.
func (st *state) tx(proto Protocol, g []Version) {
	pool := g[0].Pool
	var cfg []uint32
	txFees := map[string]blnt.ProtocolFees{}
	for _, v := range g {
		switch {
		case v.Removed:
		case v.Kind == KindConfig:
			cfg = []uint32{v.BstopRate}
		case v.Kind == KindFees:
			txFees[v.TxHash] = v.Fees
		}
	}
	for _, v := range g {
		if v.Kind == KindReserve && !v.Removed {
			st.pair(proto, pairIn{nexts: []Version{v}, bstops: st.configs[pool], prevFees: st.fees[pool], txFees: txFees})
		}
	}
	if cfg != nil {
		st.configs[pool] = cfg
	}
	if f, ok := txFees[g[0].TxHash]; ok {
		st.fees[pool] = []blnt.ProtocolFees{f}
	}
}

// unorderedLedger pairs a pool-ledger whose apply order is unknown. Each reserve's versions there
// are candidates for its first; Config and ProtocolFees written by a transaction that did not touch
// the reserve may or may not have run before it, so they join the candidates.
func (st *state) unorderedLedger(proto Protocol, vs []Version) {
	pool := vs[0].Pool
	w := collectWrites(vs)
	for _, a := range w.assets {
		touched := map[string]bool{}
		for _, v := range w.byAsset[a] {
			touched[v.TxHash] = true
		}
		bstops, prevFees := w.pool(touched)
		bstops = append(append([]uint32(nil), st.configs[pool]...), bstops...)
		prevFees = append(append([]blnt.ProtocolFees(nil), st.fees[pool]...), prevFees...)
		st.pair(proto, pairIn{nexts: w.byAsset[a], bstops: bstops, prevFees: prevFees, txFees: w.feeByTx})
	}
	cfgs, fees := w.pool(nil)
	if cfgs != nil {
		st.configs[pool] = cfgs
	}
	if fees != nil {
		st.fees[pool] = fees
	}
}

// ledgerWrites is one pool-ledger's versions by transaction and reserve.
type ledgerWrites struct {
	txOrder, assets []string
	cfgByTx         map[string]uint32
	feeByTx         map[string]blnt.ProtocolFees
	byAsset         map[string][]Version
}

func collectWrites(vs []Version) ledgerWrites {
	w := ledgerWrites{cfgByTx: map[string]uint32{}, feeByTx: map[string]blnt.ProtocolFees{}, byAsset: map[string][]Version{}}
	seen := map[string]bool{}
	for _, v := range vs {
		if !seen[v.TxHash] {
			seen[v.TxHash] = true
			w.txOrder = append(w.txOrder, v.TxHash)
		}
		switch {
		case v.Removed:
		case v.Kind == KindConfig:
			w.cfgByTx[v.TxHash] = v.BstopRate
		case v.Kind == KindFees:
			w.feeByTx[v.TxHash] = v.Fees
		case v.Kind == KindReserve:
			if _, ok := w.byAsset[v.Asset]; !ok {
				w.assets = append(w.assets, v.Asset)
			}
			w.byAsset[v.Asset] = append(w.byAsset[v.Asset], v)
		}
	}
	return w
}

// pool returns the Config and ProtocolFees values written by transactions not in skip, in stored order.
func (w ledgerWrites) pool(skip map[string]bool) (cfgs []uint32, fees []blnt.ProtocolFees) {
	for _, tx := range w.txOrder {
		if skip[tx] {
			continue
		}
		if c, ok := w.cfgByTx[tx]; ok {
			cfgs = append(cfgs, c)
		}
		if f, ok := w.feeByTx[tx]; ok {
			fees = append(fees, f)
		}
	}
	return cfgs, fees
}

// pairIn is one reserve's pairing in one ledger; each input lists every value an admissible apply
// order could give it, and the pair is summed only when all of them agree.
type pairIn struct {
	nexts    []Version // the reserve's versions in the ledger, in stored order; one when ordered
	bstops   []uint32
	prevFees []blnt.ProtocolFees
	txFees   map[string]blnt.ProtocolFees // ProtocolFees written per transaction
}

// pair accounts for every version in in.nexts: the first is a baseline, a pair or excluded; the rest
// share its timestamp and have no known predecessor, so they are unordered.
func (st *state) pair(proto Protocol, in pairIn) {
	first := in.nexts[0]
	key := ReserveKey{first.Pool, first.Asset}
	row := st.row(proto, first)
	for _, v := range in.nexts {
		row.Versions++
		row.LastLedger = v.Ledger
	}
	prevs := st.reserves[key]
	chain := first.Reserve
	if len(prevs) == 0 {
		row.Baselines++
	} else {
		st.accrue(row, proto, prevs, in)
	}
	for _, v := range in.nexts[1:] {
		row.UnorderedVersions++
		row.ExcludedCreditDelta.Add(row.ExcludedCreditDelta, new(big.Int).Sub(v.Reserve.BackstopCredit, chain.BackstopCredit))
		chain = v.Reserve
	}
	next := make([]blend.ReserveData, len(in.nexts))
	for i, v := range in.nexts {
		next[i] = v.Reserve
	}
	st.reserves[key] = next
}

func (st *state) accrue(row *HourRow, proto Protocol, prevs []blend.ReserveData, in pairIn) {
	first := in.nexts[0]
	excluded := new(big.Int).Sub(first.Reserve.BackstopCredit, prevs[len(prevs)-1].BackstopCredit)
	nexts := make([]blend.ReserveData, len(in.nexts))
	for i, v := range in.nexts {
		nexts[i] = v.Reserve
	}
	prev, okPrev := agree(prevs, samePrev)
	next, okNext := agree(nexts, sameNext)
	if !okPrev || !okNext {
		unordered(row, excluded)
		return
	}
	if len(in.bstops) == 0 {
		fail(row, first, errNoConfig, excluded)
		return
	}
	bstop, ok := agree(in.bstops, func(a, b uint32) bool { return a == b })
	if !ok {
		unordered(row, excluded)
		return
	}
	if proto != BLNT {
		a, err := blend.AccrueBetween(prev, next, bstop)
		if err != nil {
			fail(row, first, err, excluded)
			return
		}
		add(row, a, nil, prev, next)
		return
	}
	prevFee, nextFee, ok, err := pairFees(first.Asset, in)
	switch {
	case err != nil:
		fail(row, first, err, excluded)
	case !ok:
		unordered(row, excluded)
	default:
		a, err := blnt.AccrueBetween(prev, next, prevFee, nextFee, bstop)
		if err != nil {
			fail(row, first, err, excluded)
			return
		}
		add(row, a.Accrual, &a, prev, next)
	}
}

// pairFees resolves the asset's entry before and after the version's transaction; an asset absent
// from the map is an explicit zero entry.
func pairFees(asset string, in pairIn) (prevFee, nextFee blnt.ProtocolFee, ok bool, err error) {
	if len(in.prevFees) == 0 {
		return prevFee, nextFee, false, errNoFees
	}
	prevs := make([]blnt.ProtocolFee, len(in.prevFees))
	for i, f := range in.prevFees {
		prevs[i] = feeEntry(f, asset)
	}
	if prevFee, ok = agree(prevs, sameFee); !ok {
		return prevFee, nextFee, false, nil
	}
	nexts := make([]blnt.ProtocolFee, len(in.nexts))
	for i, v := range in.nexts {
		nexts[i] = prevFee
		if f, wrote := in.txFees[v.TxHash]; wrote {
			nexts[i] = feeEntry(f, asset)
		}
	}
	nextFee, ok = agree(nexts, sameFee)
	return prevFee, nextFee, ok, nil
}

func feeEntry(f blnt.ProtocolFees, asset string) blnt.ProtocolFee {
	if e, ok := f[asset]; ok {
		return e
	}
	return blnt.ProtocolFee{Credit: new(big.Int), Carry: new(big.Int)}
}

func agree[T any](vs []T, same func(a, b T) bool) (T, bool) {
	for _, v := range vs[1:] {
		if !same(vs[0], v) {
			return vs[0], false
		}
	}
	return vs[0], true
}

func eqInt(a, b *big.Int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Cmp(b) == 0
}

// samePrev compares every field AccrueBetween reads from prev.
func samePrev(a, b blend.ReserveData) bool {
	return sameNext(a, b) && eqInt(a.BSupply, b.BSupply) && eqInt(a.DSupply, b.DSupply)
}

// sameNext compares every field AccrueBetween reads from next.
func sameNext(a, b blend.ReserveData) bool {
	return a.Version == b.Version && a.LastTime == b.LastTime && eqInt(a.DRate, b.DRate) &&
		eqInt(a.BRate, b.BRate) && eqInt(a.BackstopCredit, b.BackstopCredit)
}

func sameFee(a, b blnt.ProtocolFee) bool { return eqInt(a.Credit, b.Credit) && eqInt(a.Carry, b.Carry) }

func unordered(row *HourRow, excluded *big.Int) {
	row.UnorderedVersions++
	row.ExcludedCreditDelta.Add(row.ExcludedCreditDelta, excluded)
}

func fail(row *HourRow, v Version, err error, excluded *big.Int) {
	row.AccrualErrors++
	if len(row.ErrorSamples) < maxErrorSamples {
		row.ErrorSamples = append(row.ErrorSamples, fmt.Sprintf("%d %s: %v", v.Ledger, v.TxHash, err))
	}
	row.ExcludedCreditDelta.Add(row.ExcludedCreditDelta, excluded)
}

func add(row *HourRow, a blend.Accrual, fee *blnt.Accrual, prev, next blend.ReserveData) {
	row.AccruedVersions++
	for _, s := range [...]struct{ dst, v *big.Int }{
		{row.Gross, a.Gross},
		{row.ProtocolFee, a.ProtocolFee},
		{row.Backstop, a.Backstop},
		{row.SupplierIntended, a.SupplierIntended},
		{row.SupplierRealised, a.SupplierRealised},
		{row.BackstopCreditDelta, a.BackstopCreditDelta},
		{row.BackstopResidual, a.BackstopResidual},
	} {
		s.dst.Add(s.dst, s.v)
	}
	reconciled := a.BackstopResidual.Sign() == 0
	if !a.BRateMatches {
		reconciled = false
		row.BRateMismatches++
		row.SupplyResidual.Add(row.SupplyResidual, supplyResidual(prev, next, a.ExpectedBRate))
	}
	if fee != nil {
		row.FeeCreditResidual.Add(row.FeeCreditResidual, fee.FeeCreditResidual)
		if !fee.FeesMatch {
			reconciled = false
			row.FeeMismatches++
		}
	}
	if !reconciled {
		row.UnreconciledVersions++
	}
}

// supplyResidual is what moved total_supply besides accrual: prev's b_supply at the stored b_rate
// less at the recomputed one, each floored as reserve.rs::total_supply floors.
func supplyResidual(prev, next blend.ReserveData, expected *big.Int) *big.Int {
	stored := blend.ReserveData{BSupply: prev.BSupply, BRate: next.BRate, Version: prev.Version}.SuppliedUnderlying()
	want := blend.ReserveData{BSupply: prev.BSupply, BRate: expected, Version: prev.Version}.SuppliedUnderlying()
	return stored.Sub(stored, want)
}

func (st *state) row(proto Protocol, v Version) *HourRow {
	h := hourOf(v)
	k := rowKey{v.Pool, v.Asset, h.Unix()}
	if r, ok := st.rows[k]; ok {
		return r
	}
	r := &HourRow{
		Pool: v.Pool, Asset: v.Asset, Protocol: proto, Hour: h, FirstLedger: v.Ledger,
		Gross: new(big.Int), ProtocolFee: new(big.Int), Backstop: new(big.Int), SupplierIntended: new(big.Int),
		SupplierRealised: new(big.Int), BackstopCreditDelta: new(big.Int), BackstopResidual: new(big.Int),
		SupplyResidual: new(big.Int), FeeCreditResidual: new(big.Int), ExcludedCreditDelta: new(big.Int),
	}
	st.rows[k] = r
	return r
}

func (st *state) emit() []HourRow {
	out := make([]HourRow, 0, len(st.rows))
	for _, r := range st.rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Pool != b.Pool {
			return a.Pool < b.Pool
		}
		if a.Asset != b.Asset {
			return a.Asset < b.Asset
		}
		return a.Hour.Before(b.Hour)
	})
	return out
}
