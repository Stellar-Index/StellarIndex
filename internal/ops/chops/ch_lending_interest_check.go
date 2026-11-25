package chops

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingInterestWASM maps a pool WASM hash to the accrual arithmetic the pass applies to it. A listing records that
// the audit docs bind the hash to one protocol's pools and that the arithmetic's evidence ("Interest accrual
// arithmetic" in docs/operations/wasm-audits/) covers it; it is not a review of that code's arithmetic.
var lendingInterestWASM = map[string]lendinginterest.Protocol{
	// blend.md Phase 1/3: all nine Pool Factory V2 children run it; Phase 2 walk: no upgrades.
	"a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e": lendinginterest.BlendV2,
	// blend.md "V1 pool WASM baf978f10efdbcd8": the four V1-factory pools, each for its whole life.
	"baf978f10efdbcd85747868bef8832845ea6809f7643b67a4ac0cd669327fc2c": lendinginterest.BlendV1,
	// blnt.md lineage table: the pool built from blnt-contracts-v3 5d055ca; the factory deploys only this hash.
	"0e4f8f45b94e6b0d017a496ac5620ad0e3f866533cf45acd2b070bedda1e7356": lendinginterest.BLNT,
}

// gateLendingWASM returns the window's end, lowered to the ledger before the first code change inside (lo, hi], and
// refuses unless every pool deployed by then runs, from max(lo, deploy), a listed hash mapped to its registry
// protocol. A window starting on an upgrade ledger also needs the code before it mapped to the same protocol: the
// ledger's transactions before the upgrade ran it.
func gateLendingWASM(code map[string][]clickhouse.ContractCodeVersion, pools []clickhouse.LendingContract, lo, hi uint32) (uint32, error) {
	for _, c := range pools {
		vs := code[c.ID]
		for i := 1; i < len(vs); i++ { // the first version is the deploy, not a change
			if vs[i].Ledger > lo && vs[i].Ledger <= hi {
				hi = vs[i].Ledger - 1
			}
		}
	}
	var bad []string
	for _, c := range pools {
		if c.Deployed > hi {
			continue
		}
		at := max(lo, c.Deployed)
		vs := code[c.ID]
		i := activeCode(vs, at)
		if i < 0 {
			bad = append(bad, fmt.Sprintf("pool %s has no WASM recorded at ledger %d", c.ID, at))
			continue
		}
		active := vs[i]
		proto, listed := lendingInterestWASM[active.WasmHash]
		switch {
		case !listed:
			bad = append(bad, fmt.Sprintf("pool %s runs WASM %s (from ledger %d), which lendingInterestWASM does not map to accrual arithmetic", c.ID, active.WasmHash, active.Ledger))
		case string(proto) != c.Protocol:
			bad = append(bad, fmt.Sprintf("pool %s is %s in the registry but runs WASM %s, mapped to %s", c.ID, c.Protocol, active.WasmHash, proto))
		case i > 0 && active.Ledger == at && lendingInterestWASM[vs[i-1].WasmHash] != proto:
			bad = append(bad, fmt.Sprintf("pool %s upgraded from WASM %s to %s in ledger %d, whose earlier transactions ran the old code under other arithmetic",
				c.ID, vs[i-1].WasmHash, active.WasmHash, at))
		}
	}
	if len(bad) > 0 {
		return 0, fmt.Errorf("lending interest: refusing [%d,%d]: %s; record each hash's evidence under docs/operations/wasm-audits/ and map it in lendingInterestWASM",
			lo, hi, strings.Join(bad, "; "))
	}
	return hi, nil
}

// activeCode is the index of the version in force at ledger at, -1 before the first.
func activeCode(vs []clickhouse.ContractCodeVersion, at uint32) int {
	i := -1
	for j, v := range vs {
		if v.Ledger <= at {
			i = j
		}
	}
	return i
}

// checkInstanceCode holds every instance version the window read against the WASM timeline the gate used: the
// executable it stores must be the code the timeline has in force at its ledger (on an upgrade ledger, the code before
// or after). It catches a timeline that stops short of an upgrade, since an upgrade rewrites the instance.
func checkInstanceCode(code map[string][]clickhouse.ContractCodeVersion, vs []lendinginterest.Version) error {
	for _, v := range vs {
		if v.Kind != lendinginterest.KindConfig || v.Removed {
			continue
		}
		cv := code[v.Pool]
		i := activeCode(cv, v.Ledger)
		ok := i >= 0 && cv[i].WasmHash == v.WasmHash
		if !ok && i > 0 && cv[i].Ledger == v.Ledger {
			ok = cv[i-1].WasmHash == v.WasmHash
		}
		if !ok {
			want := "none"
			if i >= 0 {
				want = cv[i].WasmHash
			}
			return fmt.Errorf("lending interest: pool %s instance at ledger %d tx %s stores WASM %q, the code history has %s in force; "+
				"the contract_instance_changes timeline is behind the lake (check contract_instance_changes_mv)", v.Pool, v.Ledger, v.TxHash, v.WasmHash, want)
		}
	}
	return nil
}

// feeScale is BLNT's carry unit: Carry is in 1e-7 base units.
var feeScale = big.NewInt(10_000_000)

// interestState is lending_state_hourly's view of one key over the checked hours.
type interestState struct {
	before *clickhouse.LendingInterestStateRow
	in     []clickhouse.LendingInterestStateRow // by hour
}

// interestChecked counts what checkLendingInterest compared, for the window's log line.
type interestChecked struct {
	telescoped, telescopeSplit int // rows whose gross was recomputed; of them, rows recomputed around excluded pairs
	excludedPairs              int // pairs those rows exclude, not recomputed
	feeChecked, feeSkipped     int // BLNT rows whose fee carry was checked; not checked (excluded or order-open pairs)
}

// checkLendingInterest holds the fold's rows of hours [from, to) against lending_state_hourly and the raw versions
// before anything is written. Per reserve:
//   - hour set: the hours with a row are exactly those lending_state holds a ResData value for (a removal there
//     refuses: the read refuses one too); each row's last ledger is that hour's state row, its first after the previous.
//   - chain endpoints: backstop_credit moved, between lending_state's rows, by the fold's backstop + residual +
//     excluded. The residual is defined as the credit delta less the backstop share, so this proves only that the
//     fold's version chain starts and ends where lending_state's does, not any arithmetic.
//   - gross: TelescopedGross over the hour's raw versions from lending_state's prior value, cut around each errored
//     or unordered pair, equals the fold's gross. That re-derives the d_rate movement without AccrueBetween's pairing; it
//     proves nothing about the fee, backstop or b_rate split, which stay counted residuals (unreconciled, b_rate
//     mismatches) reported per window.
//   - BLNT fee carry: the entry moved by exactly gross·rate less the credited whole units, plus the fee-only moves
//     (claims, credit losses) walked from the fee versions; skipped, and counted, on a row whose fee movement is
//     not all recorded (excluded pairs, or a fee-only move whose order is open).
func checkLendingInterest(protos map[string]lendinginterest.Protocol, rows []lendinginterest.HourRow,
	state []clickhouse.LendingInterestStateRow, versions []lendinginterest.Version, from, to time.Time,
) (interestChecked, error) {
	var c interestChecked
	reserves, fees := groupInterestState(state)
	byKey := map[lendinginterest.ReserveKey][]lendinginterest.HourRow{}
	for _, r := range rows {
		if r.Hour.Before(from) || !r.Hour.Before(to) {
			return c, fmt.Errorf("lending interest: pool %s asset %s: row for hour %s outside [%s, %s)", r.Pool, r.Asset, r.Hour, from, to)
		}
		k := lendinginterest.ReserveKey{Pool: r.Pool, Asset: r.Asset}
		byKey[k] = append(byKey[k], r)
		if _, ok := reserves[k]; !ok {
			reserves[k] = &interestState{}
		}
	}
	firsts := firstReserveVersions(versions)
	hours := reserveHourVersions(versions)
	keys := make([]lendinginterest.ReserveKey, 0, len(reserves))
	for k := range reserves {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Pool+keys[i].Asset < keys[j].Pool+keys[j].Asset })
	for _, k := range keys {
		if err := checkReserve(k, byKey[k], reserves[k], firsts, hours[k], &c); err != nil {
			return c, err
		}
		if protos[k.Pool] != lendinginterest.BLNT {
			continue
		}
		if err := checkFeeRows(byKey[k], fees[k.Pool], &c); err != nil {
			return c, err
		}
	}
	return c, nil
}

func checkFeeRows(rows []lendinginterest.HourRow, fees *interestState, c *interestChecked) error {
	for _, r := range rows {
		checked, err := checkFeeCarry(r, fees)
		if err != nil {
			return err
		}
		if checked {
			c.feeChecked++
		} else {
			c.feeSkipped++
		}
	}
	return nil
}

// reserveHourVersions is each reserve's versions by hour (Unix seconds).
func reserveHourVersions(vs []lendinginterest.Version) map[lendinginterest.ReserveKey]map[int64][]lendinginterest.Version {
	out := map[lendinginterest.ReserveKey]map[int64][]lendinginterest.Version{}
	for _, v := range vs {
		if v.Kind != lendinginterest.KindReserve {
			continue
		}
		k := lendinginterest.ReserveKey{Pool: v.Pool, Asset: v.Asset}
		if out[k] == nil {
			out[k] = map[int64][]lendinginterest.Version{}
		}
		h := v.CloseTime.UTC().Truncate(time.Hour).Unix()
		out[k][h] = append(out[k][h], v)
	}
	return out
}

func groupInterestState(state []clickhouse.LendingInterestStateRow) (map[lendinginterest.ReserveKey]*interestState, map[string]*interestState) {
	reserves := map[lendinginterest.ReserveKey]*interestState{}
	fees := map[string]*interestState{}
	for i := range state {
		s := state[i]
		var g *interestState
		if s.Key.Kind == lendinginterest.KindFees {
			if g = fees[s.Key.Pool]; g == nil {
				g = &interestState{}
				fees[s.Key.Pool] = g
			}
		} else {
			k := lendinginterest.ReserveKey{Pool: s.Key.Pool, Asset: s.Key.Asset}
			if g = reserves[k]; g == nil {
				g = &interestState{}
				reserves[k] = g
			}
		}
		if s.Before {
			g.before = &state[i]
		} else {
			g.in = append(g.in, s)
		}
	}
	for _, g := range fees {
		sort.Slice(g.in, func(i, j int) bool { return g.in[i].Hour.Before(g.in[j].Hour) })
	}
	for _, g := range reserves {
		sort.Slice(g.in, func(i, j int) bool { return g.in[i].Hour.Before(g.in[j].Hour) })
	}
	return reserves, fees
}

// firstReserveVersions is each reserve's first version in the fold's input, the baseline of an hour with no state
// row before it.
func firstReserveVersions(vs []lendinginterest.Version) map[lendinginterest.ReserveKey]lendinginterest.Version {
	out := map[lendinginterest.ReserveKey]lendinginterest.Version{}
	for _, v := range vs {
		k := lendinginterest.ReserveKey{Pool: v.Pool, Asset: v.Asset}
		if _, seen := out[k]; v.Kind == lendinginterest.KindReserve && !v.Removed && !seen {
			out[k] = v
		}
	}
	return out
}

func checkReserve(k lendinginterest.ReserveKey, rows []lendinginterest.HourRow, s *interestState,
	firsts map[lendinginterest.ReserveKey]lendinginterest.Version, hours map[int64][]lendinginterest.Version, c *interestChecked,
) error {
	fail := func(h time.Time, format string, a ...any) error {
		return fmt.Errorf("lending interest: identity failed for pool %s asset %s hour %s: %s", k.Pool, k.Asset,
			h.Format(time.RFC3339), fmt.Sprintf(format, a...))
	}
	prev := s.before
	for i, e := range s.in {
		if e.Value.Removed {
			return fail(e.Hour, "lending_state holds a ResData removal at ledger %d", e.Ledger)
		}
		if i >= len(rows) || !rows[i].Hour.Equal(e.Hour) {
			return fail(e.Hour, "lending_state has a row at ledger %d, the fold none", e.Ledger)
		}
		r := rows[i]
		if r.LastLedger != e.Ledger {
			return fail(e.Hour, "last_ledger %d, lending_state's hour ends at %d", r.LastLedger, e.Ledger)
		}
		start, chainFrom, msg := chainStart(r, e.Hour, prev, firsts[k])
		if msg != "" {
			return fail(e.Hour, "%s", msg)
		}
		moved := new(big.Int).Sub(e.Value.Reserve.BackstopCredit, start)
		summed := new(big.Int).Add(r.Backstop, r.BackstopResidual)
		summed.Add(summed, r.ExcludedCreditDelta)
		if moved.Cmp(summed) != 0 {
			return fail(e.Hour, "chain endpoints: backstop_credit moved %s in lending_state, the fold's chain %s (backstop %s + residual %s + excluded %s)",
				moved, summed, r.Backstop, r.BackstopResidual, r.ExcludedCreditDelta)
		}
		if msg := checkGross(r, chainFrom, hours[e.Hour.Unix()], c); msg != "" {
			return fail(e.Hour, "%s", msg)
		}
		prev = &s.in[i]
	}
	if len(rows) > len(s.in) {
		r := rows[len(s.in)]
		return fail(r.Hour, "the fold has a row (ledgers %d-%d), lending_state none", r.FirstLedger, r.LastLedger)
	}
	return nil
}

// chainStart is where the row's chain starts: the previous state row, or with none the hour's baseline version
// (first, the reserve's first version read; zero when there was none). chainFrom is nil for a baseline.
func chainStart(r lendinginterest.HourRow, hour time.Time, prev *clickhouse.LendingInterestStateRow, first lendinginterest.Version,
) (start *big.Int, chainFrom *blend.ReserveData, msg string) {
	switch {
	case prev != nil && prev.Value.Removed:
		return nil, nil, fmt.Sprintf("lending_state holds a ResData removal before it, at ledger %d", prev.Ledger)
	case prev != nil && r.FirstLedger <= prev.Ledger:
		return nil, nil, fmt.Sprintf("first_ledger %d is not after the previous state row's ledger %d", r.FirstLedger, prev.Ledger)
	case prev != nil:
		return prev.Value.Reserve.BackstopCredit, &prev.Value.Reserve, ""
	case first.Pool == "" || r.Baselines == 0 || !first.CloseTime.UTC().Truncate(time.Hour).Equal(hour):
		return nil, nil, "no state row before the hour, and the fold holds no baseline in it"
	}
	return first.Reserve.BackstopCredit, nil, ""
}

// checkGross recomputes a row's gross by TelescopedGross from chainFrom (lending_state's value before the hour, nil
// for a baseline) over the hour's raw versions; on a row with excluded pairs, over the sub-chains between them.
func checkGross(r lendinginterest.HourRow, chainFrom *blend.ReserveData, vs []lendinginterest.Version, c *interestChecked) string {
	if n := r.AccrualErrors + r.UnorderedVersions; int(n) != len(r.Excluded) {
		return fmt.Sprintf("%d excluded pairs counted, %d listed", n, len(r.Excluded))
	}
	g, err := lendinginterest.TelescopedGrossExcluding(chainFrom, vs, r.Excluded)
	if err != nil {
		return fmt.Sprintf("telescoping gross: %v", err)
	}
	if g.Cmp(r.Gross) != 0 {
		return fmt.Sprintf("gross %s, the hour's versions telescope to %s from lending_state's prior value", r.Gross, g)
	}
	c.telescoped++
	if len(r.Excluded) > 0 {
		c.telescopeSplit++
		c.excludedPairs += len(r.Excluded)
	}
	return ""
}

// checkFeeCarry is the BLNT fee identity over one row's hour: Pf.carry + fee-only carry + rate·gross − 1e7·(Ef.credit
// − Pf.credit − fee_credit_residual − fee-only credit) == Ef.carry, Pf the entry in force at the hour's start, Ef at its
// end. It reports false, checking nothing, when the row's fee movement is not all recorded.
func checkFeeCarry(r lendinginterest.HourRow, s *interestState) (bool, error) {
	if r.AccrualErrors+r.UnorderedVersions+r.FeeOnlyUnknown > 0 {
		return false, nil
	}
	pf, ef := blnt.ProtocolFees{}, blnt.ProtocolFees(nil)
	if s != nil {
		if s.before != nil {
			pf = s.before.Value.Fees
		}
		for _, f := range s.in {
			if f.Hour.Before(r.Hour) {
				pf = f.Value.Fees
			} else if f.Hour.Equal(r.Hour) {
				ef = f.Value.Fees
			}
		}
	}
	if ef == nil {
		ef = pf
	}
	p, e := feeOf(pf, r.Asset), feeOf(ef, r.Asset)
	credited := new(big.Int).Sub(e.Credit, p.Credit)
	credited.Sub(credited, r.FeeCreditResidual)
	credited.Sub(credited, zeroIfNil(r.FeeOnlyCreditDelta))
	got := new(big.Int).Mul(r.Gross, big.NewInt(blnt.ProtocolInterestFeeRate))
	got.Add(got, p.Carry)
	got.Add(got, zeroIfNil(r.FeeOnlyCarryDelta))
	got.Sub(got, credited.Mul(credited, feeScale))
	if got.Cmp(e.Carry) != 0 {
		return false, fmt.Errorf("lending interest: identity failed for pool %s asset %s hour %s: fee carry %s + %d·gross %s less "+
			"the credited units leaves %s, ProtocolFees holds carry %s (credit %s → %s, credit residual %s, %d fee-only moves: credit %s carry %s)",
			r.Pool, r.Asset, r.Hour.Format(time.RFC3339), p.Carry, blnt.ProtocolInterestFeeRate, r.Gross, got, e.Carry,
			p.Credit, e.Credit, r.FeeCreditResidual, r.FeeOnlyVersions, zeroIfNil(r.FeeOnlyCreditDelta), zeroIfNil(r.FeeOnlyCarryDelta))
	}
	return true, nil
}

func zeroIfNil(n *big.Int) *big.Int {
	if n == nil {
		return new(big.Int)
	}
	return n
}

func feeOf(f blnt.ProtocolFees, asset string) blnt.ProtocolFee {
	if e, ok := f[asset]; ok {
		return e
	}
	return blnt.ProtocolFee{Credit: new(big.Int), Carry: new(big.Int)}
}
