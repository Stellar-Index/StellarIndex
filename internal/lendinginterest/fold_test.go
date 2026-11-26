package lendinginterest

import (
	"fmt"
	"maps"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

func dec(t testing.TB, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad decimal %q", s)
	}
	return n
}

func unix(sec uint64) time.Time { return time.Unix(int64(sec), 0).UTC() } //nolint:gosec // test timestamps are far below 2^63

// dataset is golden slices folded together; slices of one pool must not interleave per asset.
type dataset struct {
	name   string
	proto  Protocol
	slices []blendtest.Slice
}

var (
	v2Supplies = dataset{"v2_supplies", BlendV2, []blendtest.Slice{blendtest.BlendV2Supplies}}
	v2Repays   = dataset{"v2_repays", BlendV2, []blendtest.Slice{blendtest.BlendV2Repays}}
	v1Auction  = dataset{"v1_interest_auction", BlendV1, []blendtest.Slice{blendtest.BlendV1}}
	blntCACSQ  = dataset{"blnt_CACSQ", BLNT, blendtest.BLNT[:3]}
	blntCCH3B  = dataset{"blnt_CCH3B", BLNT, blendtest.BLNT[3:]}
	golden     = []dataset{v2Supplies, v2Repays, v1Auction, blntCACSQ, blntCCH3B}
)

func (d dataset) poolVersion() blend.PoolVersion {
	v, _ := d.proto.poolVersion()
	return v
}

func (d dataset) opts(maxUnordered int) Options {
	o := Options{Protocols: map[string]Protocol{}, MaxUnordered: maxUnordered}
	for _, s := range d.slices {
		o.Protocols[s.Pool] = d.proto
	}
	return o
}

func (d dataset) seed() Seeds {
	s := Seeds{Configs: map[string][]uint32{}}
	for _, sl := range d.slices {
		s.Configs[sl.Pool] = []uint32{sl.BstopRate}
	}
	return s
}

func reserve(t testing.TB, r blendtest.Row, v blend.PoolVersion) blend.ReserveData {
	t.Helper()
	return blend.ReserveData{
		DRate: dec(t, r.DRate), BRate: dec(t, r.BRate), IRMod: big.NewInt(1), BSupply: dec(t, r.BSupply),
		DSupply: dec(t, r.DSupply), BackstopCredit: dec(t, r.BackstopCredit), LastTime: r.LastTime, Version: v,
	}
}

type goldenRow struct {
	slice blendtest.Slice
	row   blendtest.Row
}

// versions renders the dataset as Fold input: one ResData version per row, apply order numbered per
// ledger, and on BLNT a ProtocolFees version in the same transaction carrying every asset's latest entry.
func (d dataset) versions(t testing.TB) []Version {
	t.Helper()
	var all []goldenRow
	for _, s := range d.slices {
		for _, r := range s.Rows {
			all = append(all, goldenRow{s, r})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].row.Ledger < all[j].row.Ledger })
	fees := map[string]blnt.ProtocolFees{}
	var out []Version
	var intra uint32
	for i, g := range all {
		intra++
		if i == 0 || g.row.Ledger != all[i-1].row.Ledger {
			intra = 1
		}
		base := Version{Pool: g.slice.Pool, Ledger: g.row.Ledger, IntraLedgerSeq: intra, TxHash: g.row.Tx, CloseTime: unix(g.row.LastTime)}
		rv := base
		rv.Kind, rv.Asset, rv.Reserve = KindReserve, g.slice.Asset, reserve(t, g.row, d.poolVersion())
		out = append(out, rv)
		if d.proto != BLNT {
			continue
		}
		f := maps.Clone(fees[g.slice.Pool])
		if f == nil {
			f = blnt.ProtocolFees{}
		}
		f[g.slice.Asset] = blnt.ProtocolFee{Credit: dec(t, g.row.FeeCredit), Carry: dec(t, g.row.FeeCarry)}
		fees[g.slice.Pool] = f
		fv := base
		fv.Kind, fv.Fees, fv.ChangeIndex = KindFees, f, 1
		out = append(out, fv)
	}
	return out
}

func lastHour(vs []Version) time.Time {
	var h time.Time
	for _, v := range vs {
		if hourOf(v).After(h) {
			h = hourOf(v)
		}
	}
	return h.Add(time.Hour)
}

func fold(t testing.TB, seed Seeds, vs []Version, opts Options) []HourRow {
	t.Helper()
	rows, carry, err := Fold(seed, vs, lastHour(vs), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(carry.Pending) != 0 {
		t.Fatalf("%d versions pending after the last hour", len(carry.Pending))
	}
	checkInvariant(t, rows)
	return rows
}

// checkInvariant: every version is a baseline, summed, refused or unordered.
func checkInvariant(t testing.TB, rows []HourRow) {
	t.Helper()
	for _, r := range rows {
		if r.Versions != r.Baselines+r.AccruedVersions+r.AccrualErrors+r.UnorderedVersions {
			t.Errorf("%s %s %s: versions %d != baselines %d + accrued %d + errors %d + unordered %d", r.Pool, r.Asset, r.Hour,
				r.Versions, r.Baselines, r.AccruedVersions, r.AccrualErrors, r.UnorderedVersions)
		}
		if r.ExplainedVersions+r.AttributedVersions+r.UnexplainedVersions != r.UnreconciledVersions ||
			new(big.Int).Add(new(big.Int).Add(r.ResidualExplained, r.ResidualAttributed), r.ResidualUnexplained).Cmp(r.BackstopResidual) != 0 ||
			// The census holds only classified versions' ledgers, and every explained or attributed one.
			len(r.ResidualLedgers) > int(r.UnreconciledVersions) ||
			(r.ExplainedVersions+r.AttributedVersions > 0 && len(r.ResidualLedgers) == 0) {
			t.Errorf("%s %s %s: residual classes %d+%d+%d of %d unreconciled, %s+%s+%s of %s, ledgers %v", r.Pool, r.Asset, r.Hour,
				r.ExplainedVersions, r.AttributedVersions, r.UnexplainedVersions, r.UnreconciledVersions, r.ResidualExplained,
				r.ResidualAttributed, r.ResidualUnexplained, r.BackstopResidual, r.ResidualLedgers)
		}
		parts := new(big.Int).Add(new(big.Int).Add(r.InterestDrawn, r.Gulped), r.ReconcileBackstopLoss)
		if parts.Cmp(r.ResidualExplained) != 0 || r.InterestDrawn.Sign() > 0 || r.Gulped.Sign() < 0 || r.ReconcileBackstopLoss.Sign() > 0 {
			t.Errorf("%s %s %s: residual_explained %s, by cause drawn %s + gulped %s + reconcile %s", r.Pool, r.Asset, r.Hour,
				r.ResidualExplained, r.InterestDrawn, r.Gulped, r.ReconcileBackstopLoss)
		}
		if len(r.ErrorSamples) > maxErrorSamples || len(r.ErrorSamples) > int(r.AccrualErrors) {
			t.Errorf("%s %s: %d samples for %d errors", r.Asset, r.Hour, len(r.ErrorSamples), r.AccrualErrors)
		}
	}
}

// telescoped is Σgross from chain values alone: ceil(d_supply·d_rate/S) differences over each run of
// constant d_supply (the accrual operand).
func telescoped(t testing.TB, rows []blendtest.Row, v blend.PoolVersion) *big.Int {
	t.Helper()
	s := big.NewInt(1_000_000_000_000)
	if v == blend.PoolV1 {
		s = big.NewInt(1_000_000_000)
	}
	liab := func(d, r string) *big.Int {
		n := new(big.Int).Mul(dec(t, d), dec(t, r))
		n.Add(n, new(big.Int).Sub(s, big.NewInt(1)))
		return n.Quo(n, s)
	}
	total, j := new(big.Int), 0
	for m := 1; m < len(rows); m++ {
		if m == len(rows)-1 || rows[m].DSupply != rows[j].DSupply {
			total.Add(total, liab(rows[j].DSupply, rows[m].DRate))
			total.Sub(total, liab(rows[j].DSupply, rows[j].DRate))
			j = m
		}
	}
	return total
}

func findRow(t testing.TB, rows []HourRow, pool, asset string, hour time.Time) HourRow {
	t.Helper()
	for _, r := range rows {
		if r.Pool == pool && r.Asset == asset && r.Hour.Equal(hour) {
			return r
		}
	}
	t.Fatalf("no row for %s %s %s", pool, asset, hour)
	return HourRow{}
}

func eqStr(t testing.TB, what string, got *big.Int, want *big.Int) {
	t.Helper()
	if got.Cmp(want) != 0 {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// Every hour of every golden slice: gross equals the telescoped chain values, backstop and residual
// add up to the backstop_credit endpoints, the known non-reconciling versions are counted residuals
// (not errors), and on BLNT the fee entry moved by exactly the hour's fee.
func TestFold_GoldenHours(t *testing.T) {
	for _, d := range golden {
		t.Run(d.name, func(t *testing.T) {
			rows := fold(t, d.seed(), d.versions(t), d.opts(0))
			n := 0
			for _, s := range d.slices {
				n += checkSliceHours(t, d, s, rows)
			}
			if n != len(rows) {
				t.Errorf("%d rows, %d expected from the slices' hours", len(rows), n)
			}
		})
	}
}

func checkSliceHours(t *testing.T, d dataset, s blendtest.Slice, rows []HourRow) int {
	t.Helper()
	hours := 0
	for i0 := 0; i0 < len(s.Rows); {
		h := unix(s.Rows[i0].LastTime).Truncate(time.Hour)
		i1 := i0
		for i1+1 < len(s.Rows) && unix(s.Rows[i1+1].LastTime).Truncate(time.Hour).Equal(h) {
			i1++
		}
		checkHour(t, d, s, findRow(t, rows, s.Pool, s.Asset, h), max(i0-1, 0), i0, i1)
		hours++
		i0 = i1 + 1
	}
	return hours
}

func checkHour(t *testing.T, d dataset, s blendtest.Slice, r HourRow, from, i0, i1 int) {
	t.Helper()
	baselines := uint32(0)
	if i0 == 0 {
		baselines = 1
	}
	n := uint32(i1 - i0 + 1) //nolint:gosec // golden slices are short
	if r.Versions != n || r.Baselines != baselines || r.AccruedVersions != n-baselines || r.AccrualErrors != 0 || r.UnorderedVersions != 0 {
		t.Errorf("%s %s: counts %+v", s.Name, r.Hour, r)
	}
	if r.FirstLedger != s.Rows[i0].Ledger || r.LastLedger != s.Rows[i1].Ledger {
		t.Errorf("%s %s: ledgers %d-%d, want %d-%d", s.Name, r.Hour, r.FirstLedger, r.LastLedger, s.Rows[i0].Ledger, s.Rows[i1].Ledger)
	}
	eqStr(t, s.Name+" gross", r.Gross, telescoped(t, s.Rows[from:i1+1], d.poolVersion()))
	credit := new(big.Int).Sub(dec(t, s.Rows[i1].BackstopCredit), dec(t, s.Rows[from].BackstopCredit))
	eqStr(t, s.Name+" backstop+residual+excluded", new(big.Int).Add(new(big.Int).Add(r.Backstop, r.BackstopResidual), r.ExcludedCreditDelta), credit)
	reported, unreconciled := new(big.Int), uint32(0)
	for _, row := range s.Rows[i0 : i1+1] {
		if res, ok := s.Reported[row.Ledger]; ok {
			reported.Add(reported, dec(t, res))
			unreconciled++
		}
	}
	eqStr(t, s.Name+" backstop residual", r.BackstopResidual, reported)
	if r.UnreconciledVersions != unreconciled || r.BRateMismatches != 0 || r.FeeMismatches != 0 || r.SupplyResidual.Sign() != 0 {
		t.Errorf("%s %s: unreconciled %d want %d, b_rate %d, fee %d, supply residual %s", s.Name, r.Hour,
			r.UnreconciledVersions, unreconciled, r.BRateMismatches, r.FeeMismatches, r.SupplyResidual)
	}
	if d.proto == BLNT {
		checkFeeCarry(t, s, r, from, i1)
	} else if r.ProtocolFee.Sign() != 0 || r.FeeCreditResidual.Sign() != 0 {
		t.Errorf("%s: Blend row with a protocol fee %s", s.Name, r.ProtocolFee)
	}
}

// checkFeeCarry is lending-interest-plan §3a.3: Pf.carry + rate·Σgross − 1e7·(Ef.credit − Pf.credit
// − Σfee_credit_residual) == Ef.carry, and the hour's fee is the credit the entry gained.
func checkFeeCarry(t *testing.T, s blendtest.Slice, r HourRow, from, i1 int) {
	t.Helper()
	pf, ef := s.Rows[from], s.Rows[i1]
	moved := new(big.Int).Sub(dec(t, ef.FeeCredit), dec(t, pf.FeeCredit))
	moved.Sub(moved, r.FeeCreditResidual)
	eqStr(t, s.Name+" protocol fee", r.ProtocolFee, moved)
	lhs := new(big.Int).Mul(r.Gross, big.NewInt(blnt.ProtocolInterestFeeRate))
	lhs.Add(lhs, dec(t, pf.FeeCarry))
	lhs.Sub(lhs, new(big.Int).Mul(moved, big.NewInt(10_000_000)))
	eqStr(t, s.Name+" fee carry", lhs, dec(t, ef.FeeCarry))
}

// A seeded first version pairs with the seed instead of being a baseline.
func TestFold_Seeded(t *testing.T) {
	s := blendtest.BlendV2Supplies
	vs := v2Supplies.versions(t)
	seed := v2Supplies.seed()
	seed.Reserves = map[ReserveKey][]blend.ReserveData{{s.Pool, s.Asset}: {vs[0].Reserve}}
	rows := fold(t, seed, vs[1:], v2Supplies.opts(0))
	gross, baselines, accrued := new(big.Int), uint32(0), uint32(0)
	for _, r := range rows {
		gross.Add(gross, r.Gross)
		baselines += r.Baselines
		accrued += r.AccruedVersions
	}
	eqStr(t, "gross", gross, telescoped(t, s.Rows, blend.PoolV2))
	if baselines != 0 || accrued != uint32(len(vs)-1) { //nolint:gosec // short slice
		t.Errorf("baselines %d accrued %d", baselines, accrued)
	}
}

// snapshot renders rows and the carried state for comparison.
func snapshot(rows []HourRow, carry Seeds) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%+v\n", r)
	}
	keys := make([]ReserveKey, 0, len(carry.Reserves))
	for k := range carry.Reserves {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Pool+keys[i].Asset < keys[j].Pool+keys[j].Asset })
	for _, k := range keys {
		fmt.Fprintf(&b, "%v %+v\n", k, carry.Reserves[k])
	}
	for _, m := range []any{carry.Configs, carry.Fees} {
		fmt.Fprintf(&b, "%v\n", m)
	}
	return b.String()
}

func ledgerCuts(vs []Version) []int {
	var cuts []int
	for i := 1; i < len(vs); i++ {
		if vs[i].Ledger != vs[i-1].Ledger {
			cuts = append(cuts, i)
		}
	}
	return cuts
}

// foldWindows folds vs in windows cut at the given indexes, as the derive does with windows [A,B],
// [B+1,C]. A window commits the hours before its last version's hour, or with whole set, before the
// next version's hour (the window ended on a later ledger that touched no key).
// The windows' rows are sorted as one call sorts them.
func foldWindows(t testing.TB, seed Seeds, vs []Version, cuts []int, whole bool, final time.Time, opts Options) (string, error) {
	t.Helper()
	var rows []HourRow
	lo := 0
	for _, hi := range append(append([]int(nil), cuts...), len(vs)) {
		commit := final
		switch {
		case hi < len(vs) && whole:
			commit = hourOf(vs[hi])
		case hi < len(vs):
			commit = hourOf(vs[hi-1])
		}
		w := opts
		w.Events = windowEvents(opts.Events, vs, lo, hi)
		r, carry, err := Fold(seed, vs[lo:hi], commit, w)
		if err != nil {
			return "", fmt.Errorf("window [%d,%d): %w", lo, hi, err)
		}
		rows, seed, lo = append(rows, r...), carry, hi
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Pool != b.Pool {
			return a.Pool < b.Pool
		}
		if a.Asset != b.Asset {
			return a.Asset < b.Asset
		}
		return a.Hour.Before(b.Hour)
	})
	return snapshot(rows, seed), nil
}

// windowEvents are the events a derive window [vs[lo], vs[hi]) reads: those of its ledgers.
func windowEvents(es []Event, vs []Version, lo, hi int) []Event {
	var out []Event
	for _, e := range es {
		if (lo == 0 || e.Ledger >= vs[lo].Ledger) && (hi == len(vs) || e.Ledger < vs[hi].Ledger) {
			out = append(out, e)
		}
	}
	return out
}

// Folding in windows through the carry equals one fold, at every ledger cut and every pair of cuts.
func TestFold_SplitInvariance(t *testing.T) {
	sets := append([]dataset(nil), golden...)
	for _, d := range sets {
		t.Run(d.name, func(t *testing.T) { checkSplits(t, d.seed(), d.versions(t), d.opts(100)) })
	}
	t.Run("unordered_65641374", func(t *testing.T) {
		checkSplits(t, v2Supplies.seed(), unorderedAt(v2Supplies.versions(t), 65641374), v2Supplies.opts(100))
	})
	// The unordered ledger ends its hour, so its candidates travel in the carry.
	t.Run("unordered_hour_end", func(t *testing.T) {
		vs := unorderedAt(v2Supplies.versions(t), 65641374)
		vs[len(vs)-1].CloseTime = vs[len(vs)-1].CloseTime.Add(time.Hour)
		checkSplits(t, v2Supplies.seed(), vs, v2Supplies.opts(100))
	})
}

func checkSplits(t *testing.T, seed Seeds, vs []Version, opts Options) {
	t.Helper()
	final := lastHour(vs)
	one, err := foldWindows(t, seed, vs, nil, false, final, opts)
	if err != nil {
		t.Fatal(err)
	}
	cuts := ledgerCuts(vs)
	for _, whole := range []bool{false, true} {
		for i, a := range cuts {
			if got, err := foldWindows(t, seed, vs, []int{a}, whole, final, opts); err != nil || got != one {
				t.Fatalf("cut before ledger %d (whole hours %v): %v\n%s\nwant\n%s", vs[a].Ledger, whole, err, got, one)
			}
			for _, b := range cuts[i+1:] {
				if got, err := foldWindows(t, seed, vs, []int{a, b}, whole, final, opts); err != nil || got != one {
					t.Fatalf("cuts before ledgers %d and %d (whole hours %v) differ from one fold: %v", vs[a].Ledger, vs[b].Ledger, whole, err)
				}
			}
		}
	}
}

func unorderedAt(vs []Version, ledger uint32) []Version {
	out := append([]Version(nil), vs...)
	for i := range out {
		if out[i].Ledger == ledger {
			out[i].IntraLedgerSeq = 0
		}
	}
	return out
}

// Input order does not matter: Fold orders by (ledger, intra, tx, op, change).
func TestFold_InputOrderIrrelevant(t *testing.T) {
	vs := blntCACSQ.versions(t)
	rev := append([]Version(nil), vs...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	a := snapshot(fold(t, blntCACSQ.seed(), vs, blntCACSQ.opts(0)), Seeds{})
	b := snapshot(fold(t, blntCACSQ.seed(), rev, blntCACSQ.opts(0)), Seeds{})
	if a != b {
		t.Fatalf("reversed input folds differently:\n%s\n%s", b, a)
	}
}

func direct(t testing.TB, prev, next blend.ReserveData, bstop uint32) blend.Accrual {
	t.Helper()
	a, err := blend.AccrueBetween(prev, next, bstop)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sumRows(rows []HourRow, f func(HourRow) *big.Int) *big.Int {
	n := new(big.Int)
	for _, r := range rows {
		n.Add(n, f(r))
	}
	return n
}

func counts(rows []HourRow) (versions, accrued, errs, unordered uint32) {
	for _, r := range rows {
		versions += r.Versions
		accrued += r.AccruedVersions
		errs += r.AccrualErrors
		unordered += r.UnorderedVersions
	}
	return versions, accrued, errs, unordered
}

func gross(r HourRow) *big.Int    { return r.Gross }
func backstop(r HourRow) *big.Int { return r.Backstop }

// A Config written by next's own transaction was not in force when it ran; the next transaction's pair uses it.
func TestFold_SameTxConfigExcluded(t *testing.T) {
	vs := v2Supplies.versions(t)[:3]
	cfg := vs[1]
	cfg.Kind, cfg.Asset, cfg.Reserve, cfg.BstopRate, cfg.ChangeIndex = KindConfig, "", blend.ReserveData{}, 5_000_000, 1
	in := append(append([]Version(nil), vs...), cfg)
	rows := fold(t, v2Supplies.seed(), in, v2Supplies.opts(0))
	first := direct(t, vs[0].Reserve, vs[1].Reserve, 2_000_000)
	second := direct(t, vs[1].Reserve, vs[2].Reserve, 5_000_000)
	eqStr(t, "backstop", sumRows(rows, backstop), new(big.Int).Add(first.Backstop, second.Backstop))
	eqStr(t, "residual", sumRows(rows, func(r HourRow) *big.Int { return r.BackstopResidual }), second.BackstopResidual)
	if first.BackstopResidual.Sign() != 0 || second.BackstopResidual.Sign() == 0 {
		t.Fatalf("fixture: residuals %s %s", first.BackstopResidual, second.BackstopResidual)
	}
}

// No Config before the pair is an accrual error, never a zero rate; the pair still advances.
func TestFold_MissingConfigIsAnError(t *testing.T) {
	vs := v2Supplies.versions(t)[:3]
	cfg := vs[1]
	cfg.Kind, cfg.Asset, cfg.Reserve, cfg.BstopRate, cfg.ChangeIndex = KindConfig, "", blend.ReserveData{}, 2_000_000, 1
	rows := fold(t, Seeds{}, append(append([]Version(nil), vs...), cfg), v2Supplies.opts(0))
	_, accrued, errs, _ := counts(rows)
	if accrued != 1 || errs != 1 || !strings.Contains(rows[0].ErrorSamples[0], "65641173") {
		t.Fatalf("accrued %d errors %d samples %q", accrued, errs, rows[0].ErrorSamples)
	}
	eqStr(t, "gross", sumRows(rows, gross), direct(t, vs[1].Reserve, vs[2].Reserve, 2_000_000).Gross)
	eqStr(t, "excluded delta", rows[0].ExcludedCreditDelta,
		new(big.Int).Sub(vs[1].Reserve.BackstopCredit, vs[0].Reserve.BackstopCredit))
}

// A pair AccrueBetween refuses is counted and sampled (at most five), adds nothing, and advances.
func TestFold_RefusedPairs(t *testing.T) {
	base := v2Supplies.versions(t)[0]
	var vs []Version
	for i := range 8 {
		v := base
		v.Ledger, v.TxHash = base.Ledger+uint32(i), fmt.Sprintf("tx%d", i) //nolint:gosec // i < 8
		v.Reserve.DRate = new(big.Int).Sub(base.Reserve.DRate, big.NewInt(int64(i)))
		vs = append(vs, v)
	}
	last := v2Supplies.versions(t)[1]
	rows := fold(t, v2Supplies.seed(), append(vs, last), v2Supplies.opts(0))
	_, accrued, errs, _ := counts(rows)
	if accrued != 1 || errs != 7 || len(rows[0].ErrorSamples) != maxErrorSamples {
		t.Fatalf("accrued %d errors %d samples %d", accrued, errs, len(rows[0].ErrorSamples))
	}
	eqStr(t, "gross", sumRows(rows, gross), direct(t, vs[7].Reserve, last.Reserve, 2_000_000).Gross)
}

// v2_supplies' two versions at 65641374 lose their order: they agree on what the first pair reads,
// so it is exact, the second is a zero-interest pair in either order, and they disagree on b_supply,
// so the pair to 65641393 is unordered.
func TestFold_UnorderedDisagreeingPredecessor(t *testing.T) {
	vs := unorderedAt(v2Supplies.versions(t), 65641374)
	rows := fold(t, v2Supplies.seed(), vs, v2Supplies.opts(1))
	_, accrued, _, unordered := counts(rows)
	if accrued != 12 || unordered != 1 {
		t.Fatalf("accrued %d unordered %d", accrued, unordered)
	}
	n := len(vs)
	want := new(big.Int).Sub(telescoped(t, blendtest.BlendV2Supplies.Rows, blend.PoolV2),
		direct(t, vs[n-2].Reserve, vs[n-1].Reserve, 2_000_000).Gross)
	eqStr(t, "gross", sumRows(rows, gross), want)
	credit := new(big.Int).Sub(vs[n-1].Reserve.BackstopCredit, vs[0].Reserve.BackstopCredit)
	got := sumRows(rows, func(r HourRow) *big.Int {
		return new(big.Int).Add(new(big.Int).Add(r.Backstop, r.BackstopResidual), r.ExcludedCreditDelta)
	})
	eqStr(t, "backstop identity", got, credit)
}

// When the unordered ledger's candidates agree on every field AccrueBetween reads, the next pair is exact.
func TestFold_UnorderedAgreeingPredecessor(t *testing.T) {
	vs := unorderedAt(v2Supplies.versions(t), 65641374)
	n := len(vs)
	vs[n-2].Reserve.BSupply = vs[n-3].Reserve.BSupply
	rows := fold(t, v2Supplies.seed(), vs, v2Supplies.opts(0))
	_, accrued, _, unordered := counts(rows)
	if accrued != 13 || unordered != 0 {
		t.Fatalf("accrued %d unordered %d", accrued, unordered)
	}
	eqStr(t, "gross includes the resolved pair", sumRows(rows, gross), telescoped(t, blendtest.BlendV2Supplies.Rows, blend.PoolV2))
}

// A later write in an unordered ledger that agrees on last_time, d_rate, b_rate and backstop_credit
// is a zero-interest pair in every apply order, so the ledger folds exactly as when ordered. On BLNT
// a fee entry that moved in the ledger makes it order-dependent, and it stays unordered.
func TestFold_UnorderedZeroGapWrite(t *testing.T) {
	vs := v2Supplies.versions(t)
	n := len(vs)
	vs[n-2].Reserve.BSupply = vs[n-3].Reserve.BSupply
	want := snapshot(fold(t, v2Supplies.seed(), vs, v2Supplies.opts(0)), Seeds{})
	if got := snapshot(fold(t, v2Supplies.seed(), unorderedAt(vs, 65641374), v2Supplies.opts(0)), Seeds{}); got != want {
		t.Fatalf("unordered ledger:\n%s\nwant\n%s", got, want)
	}

	prev, next := blntPair(t)
	other := next
	other.TxHash, other.Reserve.BSupply = "t2", big.NewInt(1)
	in := unorderedAt([]Version{prev, next, other, feeVersion(next, 2, blnt.ProtocolFees{"A": feeOf(t, "2476", "1360000")})}, 11)
	seed := Seeds{Configs: map[string][]uint32{"P": {2_000_000}}, Fees: map[string][]blnt.ProtocolFees{"P": {{"A": feeOf(t, "7", "0")}}}}
	if _, accrued, _, unordered := counts(fold(t, seed, in, Options{Protocols: map[string]Protocol{"P": BLNT}, MaxUnordered: 2})); accrued != 0 || unordered != 2 {
		t.Fatalf("BLNT fee moved: accrued %d unordered %d", accrued, unordered)
	}
}

// The MaxUnordered verdict is per emitted row, so it cannot depend on where the derive cuts its calls.
func TestFold_MaxUnorderedIsCutIndependent(t *testing.T) {
	h0 := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	mk := func(ledger uint32, tx string, at time.Time, dRate, credit int64) Version {
		return Version{Pool: "P", Kind: KindReserve, Asset: "A", Ledger: ledger, TxHash: tx, CloseTime: at, Reserve: blend.ReserveData{
			DRate: big.NewInt(dRate), BRate: big.NewInt(1e12), IRMod: big.NewInt(1), BSupply: big.NewInt(1e12), DSupply: big.NewInt(5e11),
			BackstopCredit: big.NewInt(credit), LastTime: uint64(at.Unix()), Version: blend.PoolV2, //nolint:gosec // 2026
		}}
	}
	h1 := h0.Add(70 * time.Minute)
	vs := []Version{mk(10, "a", h0, 1e12, 0), mk(10, "b", h0, 1e12, 5), mk(20, "c", h1, 1e12+100, 7), mk(20, "d", h1, 1e12+100, 9)}
	seed := Seeds{Configs: map[string][]uint32{"P": {2_000_000}}}
	for limit, wantErr := range map[int]bool{1: true, 2: false} {
		opts := Options{Protocols: map[string]Protocol{"P": BlendV2}, MaxUnordered: limit}
		_, oneErr := foldWindows(t, seed, vs, nil, false, lastHour(vs), opts)
		_, splitErr := foldWindows(t, seed, vs, []int{2}, true, lastHour(vs), opts)
		if (oneErr != nil) != wantErr || (splitErr != nil) != wantErr {
			t.Errorf("MaxUnordered %d: one fold %v, split at 01:00 %v", limit, oneErr, splitErr)
		}
	}
}

// The lake can hold one change twice: an exact re-read, or a re-derive's orphan at another
// change_index or apply position. Each folds as the single change.
func TestFold_LakeDuplicates(t *testing.T) {
	vs := v2Supplies.versions(t)
	want := snapshot(fold(t, v2Supplies.seed(), vs, v2Supplies.opts(0)), Seeds{})
	n := len(vs)
	renumbered := vs[1]
	renumbered.ChangeIndex = 9
	moved := vs[n-3] // 65641374's first transaction; the other is at intra_ledger_seq 2
	moved.ChangeIndex, moved.IntraLedgerSeq = 9, 5
	for name, extra := range map[string]Version{"exact": vs[1], "renumbered": renumbered, "moved_past_another_tx": moved} {
		got := snapshot(fold(t, v2Supplies.seed(), append(append([]Version(nil), vs...), extra), v2Supplies.opts(0)), Seeds{})
		if got != want {
			t.Errorf("%s:\n%s\nwant\n%s", name, got, want)
		}
	}
}

// Two values of one key in one transaction: in change order (P23 restored, then updated) they pair in
// that order; at the same position the order is unknown, so both are accrual errors and nothing is summed.
func TestFold_OneTxTwoValues(t *testing.T) {
	vs := v2Supplies.versions(t)
	restored, updated := vs[1], vs[1]
	restored.Reserve, updated.ChangeIndex = vs[0].Reserve, 1
	in := append([]Version{vs[0], restored, updated}, vs[2:]...)
	rows := fold(t, v2Supplies.seed(), in, v2Supplies.opts(0))
	if versions, accrued, errs, unordered := counts(rows); versions != uint32(len(in)) || accrued != versions-1 || errs+unordered != 0 { //nolint:gosec // short
		t.Errorf("restored: versions %d accrued %d errors %d unordered %d", versions, accrued, errs, unordered)
	}
	eqStr(t, "restored gross", sumRows(rows, gross), telescoped(t, blendtest.BlendV2Supplies.Rows, blend.PoolV2))

	conflict := vs[1]
	conflict.Reserve.DRate = new(big.Int).Add(vs[1].Reserve.DRate, big.NewInt(1))
	rows = fold(t, v2Supplies.seed(), append(append([]Version(nil), vs...), conflict), v2Supplies.opts(1))
	_, _, errs, unordered := counts(rows)
	if errs != 2 || unordered != 1 || !strings.Contains(rows[0].ErrorSamples[0], "65641173") {
		t.Fatalf("conflict: errors %d unordered %d samples %q", errs, unordered, rows[0].ErrorSamples)
	}
}

// An eviction row (no transaction, no apply order) beside ordered transactions changes no value, so
// it does not make the ledger unordered.
func TestFold_RemovedDoesNotUnorder(t *testing.T) {
	vs := v2Supplies.versions(t)
	want := snapshot(fold(t, v2Supplies.seed(), vs, v2Supplies.opts(0)), Seeds{})
	gone := vs[len(vs)-3]
	gone.Asset, gone.TxHash, gone.IntraLedgerSeq, gone.Removed, gone.Reserve = "X", "", 0, true, blend.ReserveData{}
	if got := snapshot(fold(t, v2Supplies.seed(), append(vs, gone), v2Supplies.opts(0)), Seeds{}); got != want {
		t.Fatalf("with an eviction row:\n%s\nwant\n%s", got, want)
	}
}

// The carry owns its values: changing Fold's inputs afterwards does not change it.
func TestFold_CarryOwnsItsValues(t *testing.T) {
	vs := blntCACSQ.versions(t)
	seed := blntCACSQ.seed()
	_, carry, err := Fold(seed, vs, hourOf(vs[len(vs)-1]), blntCACSQ.opts(0))
	if err != nil || len(carry.Pending) == 0 {
		t.Fatalf("err %v, %d pending", err, len(carry.Pending))
	}
	render := func() string { return snapshot(nil, carry) + fmt.Sprintf("%+v", carry.Pending) }
	before := render()
	for _, v := range vs {
		for _, n := range []*big.Int{v.Reserve.DRate, v.Reserve.BSupply, v.Reserve.BackstopCredit} {
			if n != nil {
				n.SetInt64(-1)
			}
		}
		for _, f := range v.Fees {
			f.Credit.SetInt64(-1)
		}
	}
	for _, c := range seed.Configs {
		c[0] = 9
	}
	if after := render(); after != before {
		t.Fatalf("carry changed with its inputs:\n%s\nwas\n%s", after, before)
	}
}

// Candidates for an unordered ledger's first version that disagree on b_rate leave the pair unordered.
func TestFold_UnorderedDisagreeingFirst(t *testing.T) {
	vs := unorderedAt(v2Supplies.versions(t), 65641374)
	n := len(vs)
	vs[n-2].Reserve.BRate = new(big.Int).Add(vs[n-2].Reserve.BRate, big.NewInt(1))
	rows := fold(t, v2Supplies.seed(), vs, v2Supplies.opts(3))
	if _, accrued, _, unordered := counts(rows); accrued != 10 || unordered != 3 {
		t.Fatalf("accrued %d unordered %d", accrued, unordered)
	}
}

// Another transaction in an unordered ledger writing Config: an equal rate is harmless, a different one leaves the pair unordered.
func TestFold_UnorderedConfigWrite(t *testing.T) {
	for _, c := range []struct {
		rate      uint32
		unordered uint32
	}{{2_000_000, 0}, {5_000_000, 1}} {
		vs := v2Supplies.versions(t)[:2]
		cfg := vs[1]
		cfg.Kind, cfg.Asset, cfg.Reserve, cfg.BstopRate, cfg.TxHash = KindConfig, "", blend.ReserveData{}, c.rate, "other"
		in := unorderedAt(append(append([]Version(nil), vs...), cfg), vs[1].Ledger)
		rows := fold(t, v2Supplies.seed(), in, v2Supplies.opts(1))
		if _, accrued, _, unordered := counts(rows); unordered != c.unordered || accrued != 1-c.unordered {
			t.Errorf("rate %d: accrued %d unordered %d", c.rate, accrued, unordered)
		}
	}
}

func TestFold_MaxUnordered(t *testing.T) {
	vs := unorderedAt(v2Supplies.versions(t), 65641374)
	for limit, wantErr := range map[int]bool{0: true, 1: false} {
		_, _, err := Fold(v2Supplies.seed(), vs, lastHour(vs), v2Supplies.opts(limit))
		if (err != nil) != wantErr {
			t.Errorf("MaxUnordered %d: err %v", limit, err)
		}
	}
}

// An evicted reserve pairs across the gap: with its pre-eviction value, restored or not.
func TestFold_EvictionGap(t *testing.T) {
	vs := v2Supplies.versions(t)[:3]
	gone := vs[1]
	gone.Ledger, gone.TxHash, gone.Removed, gone.Reserve = vs[1].Ledger+5, "evict", true, blend.ReserveData{}
	restored := vs[1]
	restored.Ledger, restored.TxHash = vs[1].Ledger+10, "restore"
	want := new(big.Int).Add(direct(t, vs[0].Reserve, vs[1].Reserve, 2_000_000).Gross,
		direct(t, vs[1].Reserve, vs[2].Reserve, 2_000_000).Gross)
	for name, in := range map[string][]Version{
		"restored":  {vs[0], vs[1], gone, restored, vs[2]},
		"rewritten": {vs[0], vs[1], gone, vs[2]},
	} {
		rows := fold(t, v2Supplies.seed(), in, v2Supplies.opts(0))
		versions, accrued, errs, unordered := counts(rows)
		if versions != uint32(len(in)-1) || accrued != versions-1 || errs+unordered != 0 { //nolint:gosec // short
			t.Errorf("%s: versions %d accrued %d errors %d unordered %d", name, versions, accrued, errs, unordered)
		}
		eqStr(t, name+" gross", sumRows(rows, gross), want)
		if rows[0].UnreconciledVersions != 0 {
			t.Errorf("%s: %d unreconciled", name, rows[0].UnreconciledVersions)
		}
	}
}

// blnt.TestAccrueBetween_Vectors' pair: gross 1,234,568, fee 2469 at carry 0.
func blntPair(t testing.TB) (prev, next Version) {
	t.Helper()
	mk := func(ledger uint32, tx string, r blendtest.Row) Version {
		return Version{
			Pool: "P", Kind: KindReserve, Asset: "A", Ledger: ledger, IntraLedgerSeq: 1, TxHash: tx,
			CloseTime: unix(r.LastTime), Reserve: reserve(t, r, blend.PoolV2),
		}
	}
	prev = mk(10, "t0", blendtest.Row{DRate: "1100000000000", BRate: "1050000000000", BSupply: "1300000000013", DSupply: "987654321987", BackstopCredit: "500", LastTime: 100})
	next = mk(11, "t1", blendtest.Row{DRate: "1100001250000", BRate: "1050000758214", BSupply: "1300000000013", DSupply: "987654321987", BackstopCredit: "246919", LastTime: 160})
	next.ChangeIndex = 1
	return prev, next
}

func feeVersion(at Version, change uint32, fees blnt.ProtocolFees) Version {
	v := at
	v.Kind, v.Asset, v.Reserve, v.Fees, v.ChangeIndex = KindFees, "", blend.ReserveData{}, fees, change
	return v
}

func feeOf(t testing.TB, credit, carry string) blnt.ProtocolFee {
	t.Helper()
	return blnt.ProtocolFee{Credit: dec(t, credit), Carry: dec(t, carry)}
}

// nextFee is the ProtocolFees of next's own transaction wherever it sits in it, never a later
// transaction's; an asset absent from the map is a zero entry; no ProtocolFees state is an error.
func TestFold_BLNTFeePairing(t *testing.T) {
	prev, next := blntPair(t)
	other := next
	other.IntraLedgerSeq, other.TxHash = 2, "t2"
	seed7 := map[string][]blnt.ProtocolFees{"P": {{"A": feeOf(t, "7", "0")}}}
	after := blnt.ProtocolFees{"A": feeOf(t, "2476", "1360000")}
	cases := []struct {
		name      string
		fees      map[string][]blnt.ProtocolFees
		in        []Version
		fee, res  string
		mismatch  uint32
		accrualEr uint32
	}{
		{"same_tx_after", seed7, []Version{prev, next, feeVersion(next, 2, after)}, "2469", "0", 0, 0},
		{"same_tx_before", seed7, []Version{prev, next, feeVersion(next, 0, after)}, "2469", "0", 0, 0},
		{"later_tx_not_next", seed7, []Version{prev, next, feeVersion(other, 0, after)}, "2469", "-2469", 1, 0},
		{"missing_asset_is_zero", map[string][]blnt.ProtocolFees{"P": {{}}}, []Version{
			prev, next,
			feeVersion(next, 2, blnt.ProtocolFees{"A": feeOf(t, "2469", "1360000")}),
		}, "2469", "0", 0, 0},
		{"no_fee_state", nil, []Version{prev, next}, "0", "0", 0, 1},
		// A delete stores no fees: the empty map replaces the seeded 7, so next's prevFee is zero.
		{"deleted_fees_are_empty", seed7, []Version{
			prev, feeVersion(prev, 2, blnt.ProtocolFees{}), next,
			feeVersion(next, 2, blnt.ProtocolFees{"A": feeOf(t, "2469", "1360000")}),
		}, "2469", "0", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seed := Seeds{Configs: map[string][]uint32{"P": {2_000_000}}, Fees: c.fees}
			rows := fold(t, seed, c.in, Options{Protocols: map[string]Protocol{"P": BLNT}})
			r := rows[0]
			eqStr(t, "protocol fee", r.ProtocolFee, dec(t, c.fee))
			eqStr(t, "fee credit residual", r.FeeCreditResidual, dec(t, c.res))
			if r.FeeMismatches != c.mismatch || r.AccrualErrors != c.accrualEr {
				t.Errorf("fee mismatches %d errors %d %q", r.FeeMismatches, r.AccrualErrors, r.ErrorSamples)
			}
		})
	}
}

func TestFold_RefusesInput(t *testing.T) {
	vs := v2Supplies.versions(t)
	v1 := vs[1]
	v1.Reserve.Version = blend.PoolV1
	nilField := vs[1]
	nilField.Reserve.BackstopCredit = nil
	cases := map[string]func() error{
		"no protocol": func() error { _, _, err := Fold(Seeds{}, vs, lastHour(vs), Options{}); return err },
		"scale mismatch": func() error {
			_, _, err := Fold(Seeds{}, []Version{vs[0], v1}, lastHour(vs), v2Supplies.opts(0))
			return err
		},
		"nil field": func() error {
			_, _, err := Fold(Seeds{}, []Version{vs[0], nilField}, lastHour(vs), v2Supplies.opts(0))
			return err
		},
		"commit not an hour": func() error {
			_, _, err := Fold(Seeds{}, vs, lastHour(vs).Add(time.Minute), v2Supplies.opts(0))
			return err
		},
		"already committed": func() error {
			_, _, err := Fold(Seeds{CommittedBefore: lastHour(vs)}, vs, lastHour(vs), v2Supplies.opts(0))
			return err
		},
		"fees on a blend pool": func() error {
			_, _, err := Fold(Seeds{}, []Version{feeVersion(vs[0], 1, blnt.ProtocolFees{})}, lastHour(vs), v2Supplies.opts(0))
			return err
		},
		// A removal of either is a delete, not an eviction gap that keeps the old value in force.
		"removed fees": func() error {
			prev, next := blntPair(t)
			gone := feeVersion(prev, 2, nil)
			gone.Removed = true
			_, _, err := Fold(Seeds{Configs: map[string][]uint32{"P": {2_000_000}}}, []Version{prev, gone, next}, lastHour([]Version{next}),
				Options{Protocols: map[string]Protocol{"P": BLNT}})
			return err
		},
		"removed instance": func() error {
			gone := vs[0]
			gone.Kind, gone.Asset, gone.Reserve, gone.Removed, gone.ChangeIndex = KindConfig, "", blend.ReserveData{}, true, 9
			_, _, err := Fold(v2Supplies.seed(), append([]Version{gone}, vs...), lastHour(vs), v2Supplies.opts(0))
			return err
		},
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
