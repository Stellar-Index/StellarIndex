package chops

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingInterestWASM is the pool code whose accrual arithmetic the pass knows, by WASM hash. A hash enters only with
// evidence in docs/operations/wasm-audits/ binding it to one protocol's pools; a pool running any other refuses.
var lendingInterestWASM = map[string]lendinginterest.Protocol{
	// blend.md Phase 1/3: all nine Pool Factory V2 children run it; Phase 2 walk: no upgrades.
	"a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e": lendinginterest.BlendV2,
	// blend.md "V1 pool WASM baf978f10efdbcd8": the four V1-factory pools, each for its whole life.
	"baf978f10efdbcd85747868bef8832845ea6809f7643b67a4ac0cd669327fc2c": lendinginterest.BlendV1,
	// blnt.md lineage table: the pool built from blnt-contracts-v3 5d055ca; the factory deploys only this hash.
	"0e4f8f45b94e6b0d017a496ac5620ad0e3f866533cf45acd2b070bedda1e7356": lendinginterest.BLNT,
}

// gateLendingWASM returns the window's end, lowered to the ledger before the first code change inside (lo, hi], and
// refuses unless every pool deployed by then runs, from max(lo, deploy), a listed hash audited as its registry
// protocol.
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
		var active *clickhouse.ContractCodeVersion
		for i, v := range code[c.ID] {
			if v.Ledger <= at {
				active = &code[c.ID][i]
			}
		}
		if active == nil {
			bad = append(bad, fmt.Sprintf("pool %s has no WASM recorded at ledger %d", c.ID, at))
			continue
		}
		proto, listed := lendingInterestWASM[active.WasmHash]
		switch {
		case !listed:
			bad = append(bad, fmt.Sprintf("pool %s runs WASM %s (from ledger %d), which no audit binds to accrual arithmetic", c.ID, active.WasmHash, active.Ledger))
		case string(proto) != c.Protocol:
			bad = append(bad, fmt.Sprintf("pool %s is %s in the registry but runs WASM %s, audited as %s", c.ID, c.Protocol, active.WasmHash, proto))
		}
	}
	if len(bad) > 0 {
		return 0, fmt.Errorf("lending interest: refusing [%d,%d]: %s; audit each hash under docs/operations/wasm-audits/ and list it in lendingInterestWASM",
			lo, hi, strings.Join(bad, "; "))
	}
	return hi, nil
}

// feeScale is BLNT's carry unit: Carry is in 1e-7 base units.
var feeScale = big.NewInt(10_000_000)

// interestState is lending_state_hourly's view of one key over the checked hours.
type interestState struct {
	before *clickhouse.LendingInterestStateRow
	in     []clickhouse.LendingInterestStateRow // by hour
}

// checkLendingInterest holds the fold's rows of hours [from, to) against lending_state_hourly before anything is
// written. Per reserve: the hours with a row are exactly the hours lending_state holds a ResData value for (a removal
// there refuses: the read refuses one too), each row's last ledger is that hour's state row and its first lies after
// the previous one, and the backstop credit moved by exactly the summed backstop, residual and excluded deltas. On
// BLNT the fee entry's carry moved by exactly gross·rate less the credited whole units, unless the hour holds pairs
// left out of the sums (accrual errors or unordered), whose fee movement no row records.
func checkLendingInterest(protos map[string]lendinginterest.Protocol, rows []lendinginterest.HourRow,
	state []clickhouse.LendingInterestStateRow, versions []lendinginterest.Version, from, to time.Time,
) error {
	reserves, fees := groupInterestState(state)
	byKey := map[lendinginterest.ReserveKey][]lendinginterest.HourRow{}
	for _, r := range rows {
		if r.Hour.Before(from) || !r.Hour.Before(to) {
			return fmt.Errorf("lending interest: pool %s asset %s: row for hour %s outside [%s, %s)", r.Pool, r.Asset, r.Hour, from, to)
		}
		k := lendinginterest.ReserveKey{Pool: r.Pool, Asset: r.Asset}
		byKey[k] = append(byKey[k], r)
		if _, ok := reserves[k]; !ok {
			reserves[k] = &interestState{}
		}
	}
	firsts := firstReserveVersions(versions)
	keys := make([]lendinginterest.ReserveKey, 0, len(reserves))
	for k := range reserves {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Pool+keys[i].Asset < keys[j].Pool+keys[j].Asset })
	for _, k := range keys {
		if err := checkReserve(k, byKey[k], reserves[k], firsts); err != nil {
			return err
		}
		if protos[k.Pool] != lendinginterest.BLNT {
			continue
		}
		for _, r := range byKey[k] {
			if err := checkFeeCarry(r, fees[k.Pool]); err != nil {
				return err
			}
		}
	}
	return nil
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
	firsts map[lendinginterest.ReserveKey]lendinginterest.Version,
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
		var start *big.Int
		switch {
		case prev != nil && prev.Value.Removed:
			return fail(e.Hour, "lending_state holds a ResData removal before it, at ledger %d", prev.Ledger)
		case prev != nil && r.FirstLedger <= prev.Ledger:
			return fail(e.Hour, "first_ledger %d is not after the previous state row's ledger %d", r.FirstLedger, prev.Ledger)
		case prev != nil:
			start = prev.Value.Reserve.BackstopCredit
		default:
			f, ok := firsts[k]
			if !ok || r.Baselines == 0 || !f.CloseTime.UTC().Truncate(time.Hour).Equal(e.Hour) {
				return fail(e.Hour, "no state row before the hour, and the fold holds no baseline in it")
			}
			start = f.Reserve.BackstopCredit
		}
		moved := new(big.Int).Sub(e.Value.Reserve.BackstopCredit, start)
		summed := new(big.Int).Add(r.Backstop, r.BackstopResidual)
		summed.Add(summed, r.ExcludedCreditDelta)
		if moved.Cmp(summed) != 0 {
			return fail(e.Hour, "backstop_credit moved %s in lending_state, the fold sums %s (backstop %s + residual %s + excluded %s)",
				moved, summed, r.Backstop, r.BackstopResidual, r.ExcludedCreditDelta)
		}
		prev = &s.in[i]
	}
	if len(rows) > len(s.in) {
		r := rows[len(s.in)]
		return fail(r.Hour, "the fold has a row (ledgers %d-%d), lending_state none", r.FirstLedger, r.LastLedger)
	}
	return nil
}

// checkFeeCarry is the BLNT fee identity over one row's hour: Pf.carry + rate·gross − 1e7·(Ef.credit − Pf.credit −
// fee_credit_residual) == Ef.carry, Pf the entry in force at the hour's start, Ef at its end.
func checkFeeCarry(r lendinginterest.HourRow, s *interestState) error {
	if r.AccrualErrors+r.UnorderedVersions > 0 {
		return nil
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
	got := new(big.Int).Mul(r.Gross, big.NewInt(blnt.ProtocolInterestFeeRate))
	got.Add(got, p.Carry)
	got.Sub(got, credited.Mul(credited, feeScale))
	if got.Cmp(e.Carry) != 0 {
		return fmt.Errorf("lending interest: identity failed for pool %s asset %s hour %s: fee carry %s + %d·gross %s less "+
			"the credited units leaves %s, ProtocolFees holds carry %s (credit %s → %s, credit residual %s)",
			r.Pool, r.Asset, r.Hour.Format(time.RFC3339), p.Carry, blnt.ProtocolInterestFeeRate, r.Gross, got, e.Carry,
			p.Credit, e.Credit, r.FeeCreditResidual)
	}
	return nil
}

func feeOf(f blnt.ProtocolFees, asset string) blnt.ProtocolFee {
	if e, ok := f[asset]; ok {
		return e
	}
	return blnt.ProtocolFee{Credit: new(big.Int), Carry: new(big.Int)}
}
