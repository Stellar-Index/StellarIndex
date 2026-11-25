package lendinginterest

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"time"

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
	feeOnly  map[rowKey]*feeMove
}

// feeMove is one asset-hour's ProtocolFees movement outside its reserve's pairs.
type feeMove struct {
	credit, carry *big.Int
	n, unknown    uint32
}

func newState(seed Seeds, opts Options) (*state, error) {
	st := &state{
		opts:     opts,
		reserves: make(map[ReserveKey][]blend.ReserveData, len(seed.Reserves)),
		configs:  make(map[string][]uint32, len(seed.Configs)),
		fees:     make(map[string][]blnt.ProtocolFees, len(seed.Fees)),
		rows:     map[rowKey]*HourRow{},
		feeOnly:  map[rowKey]*feeMove{},
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

// carry deep-copies the state, so a caller changing Fold's inputs later cannot change it.
func (st *state) carry(pending []Version, committedBefore time.Time) Seeds {
	s := Seeds{
		Reserves: make(map[ReserveKey][]blend.ReserveData, len(st.reserves)), Configs: make(map[string][]uint32, len(st.configs)),
		Fees: make(map[string][]blnt.ProtocolFees, len(st.fees)), Pending: make([]Version, len(pending)), CommittedBefore: committedBefore,
	}
	for k, v := range st.reserves {
		s.Reserves[k] = make([]blend.ReserveData, len(v))
		for i, rd := range v {
			s.Reserves[k][i] = cloneReserve(rd)
		}
	}
	for k, v := range st.configs {
		s.Configs[k] = slices.Clone(v)
	}
	for k, v := range st.fees {
		s.Fees[k] = make([]blnt.ProtocolFees, len(v))
		for i, f := range v {
			s.Fees[k][i] = cloneFees(f)
		}
	}
	for i, v := range pending {
		v.Reserve, v.Fees = cloneReserve(v.Reserve), cloneFees(v.Fees)
		s.Pending[i] = v
	}
	return s
}

func cloneInt(n *big.Int) *big.Int {
	if n == nil {
		return nil
	}
	return new(big.Int).Set(n)
}

func cloneReserve(rd blend.ReserveData) blend.ReserveData {
	rd.DRate, rd.BRate, rd.IRMod = cloneInt(rd.DRate), cloneInt(rd.BRate), cloneInt(rd.IRMod)
	rd.BSupply, rd.DSupply, rd.BackstopCredit = cloneInt(rd.BSupply), cloneInt(rd.DSupply), cloneInt(rd.BackstopCredit)
	return rd
}

func cloneFees(f blnt.ProtocolFees) blnt.ProtocolFees {
	if f == nil {
		return nil
	}
	out := make(blnt.ProtocolFees, len(f))
	for a, e := range f {
		out[a] = blnt.ProtocolFee{Credit: cloneInt(e.Credit), Carry: cloneInt(e.Carry)}
	}
	return out
}

func complete(rd blend.ReserveData) bool {
	return rd.DRate != nil && rd.BRate != nil && rd.BSupply != nil && rd.DSupply != nil && rd.BackstopCredit != nil
}

// poolLedger pairs one pool-ledger. Positions that contradict each other leave the apply order
// unknown, as a zero intra_ledger_seq does, but its order-dependent versions count as accrual errors.
func (st *state) poolLedger(vs []Version) {
	proto := st.opts.Protocols[vs[0].Pool]
	if unorderedLedger(vs) {
		st.unorderedLedger(proto, vs, nil)
		return
	}
	groups, err := txGroups(vs)
	if err != nil {
		st.unorderedLedger(proto, vs, err)
		return
	}
	for _, g := range groups {
		st.tx(proto, g)
	}
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
		paired := map[string]bool{}
		for _, v := range g {
			if v.Kind == KindReserve && !v.Removed {
				paired[v.Asset] = true
			}
		}
		st.feeOnlyMoves(g[0], st.fees[pool], []blnt.ProtocolFees{f}, paired)
		st.fees[pool] = []blnt.ProtocolFees{f}
	}
}

// feeOnlyMoves records, per asset not in paired, the ProtocolFees movement from befores to afters (each the
// candidates an admissible apply order leaves): exact when each side agrees, else counted unknown.
func (st *state) feeOnlyMoves(at Version, befores, afters []blnt.ProtocolFees, paired map[string]bool) {
	for _, a := range feeAssets(paired, befores, afters) {
		var b blnt.ProtocolFee
		okB := len(befores) > 0
		if okB {
			b, okB = agree(feeEntries(befores, a), sameFee)
		}
		f, okA := agree(feeEntries(afters, a), sameFee)
		if okB && okA && sameFee(b, f) {
			continue
		}
		k := rowKey{at.Pool, a, hourOf(at).Unix()}
		m := st.feeOnly[k]
		if m == nil {
			m = &feeMove{credit: new(big.Int), carry: new(big.Int)}
			st.feeOnly[k] = m
		}
		if !okB || !okA {
			m.unknown++
			continue
		}
		m.n++
		m.credit.Add(m.credit, new(big.Int).Sub(f.Credit, b.Credit))
		m.carry.Add(m.carry, new(big.Int).Sub(f.Carry, b.Carry))
	}
}

// feeAssets is every asset the fee entries name outside skip, sorted.
func feeAssets(skip map[string]bool, sets ...[]blnt.ProtocolFees) []string {
	seen := map[string]bool{}
	var out []string
	for _, fs := range sets {
		for _, f := range fs {
			for a := range f {
				if !seen[a] && !skip[a] {
					seen[a] = true
					out = append(out, a)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func feeEntries(fs []blnt.ProtocolFees, asset string) []blnt.ProtocolFee {
	out := make([]blnt.ProtocolFee, len(fs))
	for i, f := range fs {
		out[i] = feeEntry(f, asset)
	}
	return out
}

// unorderedLedger pairs a pool-ledger whose apply order is unknown. Each reserve's versions there
// are candidates for its first; Config and ProtocolFees written by a transaction that did not touch
// the reserve may or may not have run before it, so they join the candidates.
func (st *state) unorderedLedger(proto Protocol, vs []Version, orderErr error) {
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
		st.pair(proto, pairIn{nexts: w.byAsset[a], bstops: bstops, prevFees: prevFees, txFees: w.feeByTx, orderErr: orderErr})
	}
	cfgs, fees := w.pool(nil)
	if fees != nil {
		// A reserve written here pairs under every candidate fee entry: a fee-only write to its asset
		// unorders that pair, so only the other assets' movements are recorded here.
		paired := map[string]bool{}
		for _, a := range w.assets {
			paired[a] = true
		}
		st.feeOnlyMoves(vs[0], st.fees[pool], fees, paired)
	}
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
	orderErr error                        // set when contradicting positions, not a zero seq, hid the order
}

func reserves(vs []Version) []blend.ReserveData {
	out := make([]blend.ReserveData, len(vs))
	for i, v := range vs {
		out[i] = v.Reserve
	}
	return out
}

// pair accounts for every version in in.nexts: the first is a baseline, a pair or excluded; the rest
// share its timestamp and pair with each other in an unknown order.
func (st *state) pair(proto Protocol, in pairIn) {
	first := in.nexts[0]
	key := ReserveKey{first.Pool, first.Asset}
	row := st.row(proto, first)
	for _, v := range in.nexts {
		row.Versions++
		row.LastLedger = v.Ledger
	}
	if prevs := st.reserves[key]; len(prevs) == 0 {
		row.Baselines++
	} else {
		st.accrue(row, proto, prevs, in)
	}
	st.later(row, proto, in)
	st.reserves[key] = reserves(in.nexts)
}

// later accounts for the reserve's versions after the first in an unordered ledger. Each pairs with
// another of them; when all agree on what AccrueBetween reads from next, d_rate is unchanged in every
// order, so gross, fee, backstop, realised and credit delta are zero and b_rate matches whatever the
// supplies. They are summed only when the Config's presence and the BLNT fee entry are order-free too.
func (st *state) later(row *HourRow, proto Protocol, in pairIn) {
	if len(in.nexts) < 2 {
		return
	}
	bstop, fee, exact := st.zeroGap(proto, in)
	chain := in.nexts[0].Reserve
	for _, v := range in.nexts[1:] {
		excluded := new(big.Int).Sub(v.Reserve.BackstopCredit, chain.BackstopCredit)
		if !exact {
			orderUnknown(row, v, in, excluded)
		} else if err := accrueZeroGap(row, proto, chain, v.Reserve, bstop, fee); err != nil {
			fail(row, v, err, excluded)
		}
		chain = v.Reserve
	}
}

// zeroGap reports whether in.nexts agree on every field AccrueBetween reads from next, a Config held
// before the ledger, and on BLNT every candidate fee entry of the asset is the same.
func (st *state) zeroGap(proto Protocol, in pairIn) (bstop uint32, fee blnt.ProtocolFee, ok bool) {
	pool, asset := in.nexts[0].Pool, in.nexts[0].Asset
	cfgs := st.configs[pool]
	if _, same := agree(reserves(in.nexts), sameNext); !same || len(cfgs) == 0 {
		return 0, fee, false
	}
	if proto != BLNT {
		return cfgs[len(cfgs)-1], fee, true
	}
	if len(st.fees[pool]) == 0 {
		return 0, fee, false
	}
	fee = feeEntry(st.fees[pool][0], asset)
	for _, f := range in.prevFees {
		if !sameFee(fee, feeEntry(f, asset)) {
			return 0, fee, false
		}
	}
	for _, f := range in.txFees {
		if !sameFee(fee, feeEntry(f, asset)) {
			return 0, fee, false
		}
	}
	return cfgs[len(cfgs)-1], fee, true
}

func accrueZeroGap(row *HourRow, proto Protocol, prev, next blend.ReserveData, bstop uint32, fee blnt.ProtocolFee) error {
	if proto != BLNT {
		a, err := blend.AccrueBetween(prev, next, bstop)
		if err == nil {
			add(row, a, nil, prev, next)
		}
		return err
	}
	a, err := blnt.AccrueBetween(prev, next, fee, fee, bstop)
	if err == nil {
		add(row, a.Accrual, &a, prev, next)
	}
	return err
}

func (st *state) accrue(row *HourRow, proto Protocol, prevs []blend.ReserveData, in pairIn) {
	first := in.nexts[0]
	excluded := new(big.Int).Sub(first.Reserve.BackstopCredit, prevs[len(prevs)-1].BackstopCredit)
	prev, okPrev := agree(prevs, samePrev)
	next, okNext := agree(reserves(in.nexts), sameNext)
	if !okPrev || !okNext {
		orderUnknown(row, first, in, excluded)
		return
	}
	if len(in.bstops) == 0 {
		fail(row, first, errNoConfig, excluded)
		return
	}
	bstop, ok := agree(in.bstops, func(a, b uint32) bool { return a == b })
	if !ok {
		orderUnknown(row, first, in, excluded)
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
		orderUnknown(row, first, in, excluded)
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

// orderUnknown counts a version whose pairing depends on the unknown apply order.
func orderUnknown(row *HourRow, v Version, in pairIn, excluded *big.Int) {
	if in.orderErr != nil {
		fail(row, v, in.orderErr, excluded)
		return
	}
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
		FeeOnlyCreditDelta: new(big.Int), FeeOnlyCarryDelta: new(big.Int),
	}
	st.rows[k] = r
	return r
}

func (st *state) emit() []HourRow {
	out := make([]HourRow, 0, len(st.rows))
	for k, r := range st.rows {
		// A move in an hour with no row of its reserve is in the next row's opening entry.
		if m := st.feeOnly[k]; m != nil {
			r.FeeOnlyCreditDelta.Add(r.FeeOnlyCreditDelta, m.credit)
			r.FeeOnlyCarryDelta.Add(r.FeeOnlyCarryDelta, m.carry)
			r.FeeOnlyVersions += m.n
			r.FeeOnlyUnknown += m.unknown
		}
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
