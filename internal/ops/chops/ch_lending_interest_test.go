package chops

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	liPool  = "CPOOL"
	liAsset = "CASSET"
	liV2    = "a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e"
	liPerHr = 7 // ledgers per hour in the fake lake, so a partition boundary falls mid-hour
)

var liT0 = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)

func liClose(seq uint32) time.Time {
	// Modulo keeps the Duration in range; no test spans a wrap.
	return liT0.Add(time.Duration(seq/liPerHr%1_000_000)*time.Hour + time.Duration(seq%liPerHr)*time.Minute)
}

// fakeInterestLake is the interest pass's storage: ledgers liPerHr to an hour through tip, the versions, and
// lending_state_hourly derived from them as the state pass would.
type fakeInterestLake struct {
	wm, stateWM, floor, tip uint32
	pools                   []clickhouse.LendingContract
	versions                []lendinginterest.Version
	code                    map[string][]clickhouse.ContractCodeVersion
	tamper                  func([]clickhouse.LendingInterestStateRow)

	calls    []string
	reads    [][2]uint32
	seeds    int
	inserted []lendinginterest.HourRow
	marks    [][2]uint32
}

func (f *fakeInterestLake) hour(seq uint32) clickhouse.LedgerHour {
	first := seq / liPerHr * liPerHr
	return clickhouse.LedgerHour{Hour: liClose(first), First: first, Last: first + liPerHr - 1, Closed: first+liPerHr <= f.tip}
}

// state is each reserve's last version per hour in [from, to), and its last before from.
func (f *fakeInterestLake) state(from, to time.Time) []clickhouse.LendingInterestStateRow {
	var out []clickhouse.LendingInterestStateRow
	byHour := map[time.Time]int{}
	var before *clickhouse.LendingInterestStateRow
	for _, v := range f.versions {
		if v.Kind != lendinginterest.KindReserve {
			continue
		}
		h := v.CloseTime.Truncate(time.Hour)
		r := clickhouse.LendingInterestStateRow{
			Key:  clickhouse.LendingInterestKey{Pool: v.Pool, Asset: v.Asset, Kind: lendinginterest.KindReserve},
			Hour: h, Ledger: v.Ledger, Value: v,
		}
		switch {
		case h.Before(from):
			r.Before = true
			before = &r
		case h.Before(to):
			if i, ok := byHour[h]; ok {
				out[i] = r
			} else {
				byHour[h] = len(out)
				out = append(out, r)
			}
		}
	}
	if before != nil {
		out = append(out, *before)
	}
	if f.tamper != nil {
		f.tamper(out)
	}
	return out
}

func (f *fakeInterestLake) install(t *testing.T) {
	t.Helper()
	saved := []func(){}
	set := func(restore func()) { saved = append(saved, restore) }
	{
		a, b, c, d, e := lendingInterestWatermark, lendingInterestFloor, lendingInterestStateWM, lendingInterestHour, lendingInterestKeys
		set(func() {
			lendingInterestWatermark, lendingInterestFloor, lendingInterestStateWM, lendingInterestHour, lendingInterestKeys = a, b, c, d, e
		})
		g, h, i, j, k := lendingInterestSeeds, lendingInterestVersions, lendingInterestState, lendingInterestUncovered, insertLendingInterest
		set(func() {
			lendingInterestSeeds, lendingInterestVersions, lendingInterestState, lendingInterestUncovered, insertLendingInterest = g, h, i, j, k
		})
		l, m, n, o, p, q := deleteSupersededInterest, setLendingInterestWM, setLendingInterestPoolWM, lendingInterestServerNow, lendingInterestRegistry, openLendingCodeHistory
		set(func() {
			deleteSupersededInterest, setLendingInterestWM, setLendingInterestPoolWM, lendingInterestServerNow, lendingInterestRegistry, openLendingCodeHistory = l, m, n, o, p, q
		})
	}
	t.Cleanup(func() {
		for _, r := range saved {
			r()
		}
	})
	lendingInterestWatermark = func(context.Context, string) (uint32, error) { return f.wm, nil }
	lendingInterestFloor = func(context.Context, string) (uint32, error) { return f.floor, nil }
	lendingInterestStateWM = func(context.Context, string) (uint32, error) { return f.stateWM, nil }
	lendingInterestHour = func(_ context.Context, _ string, seq uint32) (clickhouse.LedgerHour, error) { return f.hour(seq), nil }
	lendingInterestKeys = func(_ context.Context, _ string, _ []clickhouse.LendingContract, hi uint32) ([]clickhouse.LendingInterestKey, error) {
		if hi > f.stateWM {
			return nil, clickhouse.ErrLendingInterestAheadOfState
		}
		return []clickhouse.LendingInterestKey{{Key: "res", Pool: liPool, Kind: lendinginterest.KindReserve, Asset: liAsset}}, nil
	}
	lendingInterestSeeds = func(_ context.Context, _ string, _ []clickhouse.LendingContract, _ []clickhouse.LendingInterestKey, h time.Time) (lendinginterest.Seeds, error) {
		f.seeds++
		s := lendinginterest.Seeds{CommittedBefore: h, Configs: map[string][]uint32{liPool: {2_000_000}}}
		for _, v := range f.versions {
			if v.Kind == lendinginterest.KindReserve && v.CloseTime.Before(h) {
				s.Reserves = map[lendinginterest.ReserveKey][]blend.ReserveData{{Pool: v.Pool, Asset: v.Asset}: {v.Reserve}}
			}
		}
		return s, nil
	}
	lendingInterestVersions = func(_ context.Context, _ string, lo, hi uint32, _ []clickhouse.LendingInterestKey, fn func(lendinginterest.Version) error) error {
		f.reads = append(f.reads, [2]uint32{lo, hi})
		for _, v := range f.versions {
			if v.Ledger >= lo && v.Ledger <= hi {
				if err := fn(v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	lendingInterestState = func(_ context.Context, _ string, _ []clickhouse.LendingContract, _ []clickhouse.LendingInterestKey, from, to time.Time) ([]clickhouse.LendingInterestStateRow, error) {
		return f.state(from, to), nil
	}
	lendingInterestUncovered = func(context.Context, string, []clickhouse.LendingContract) ([]clickhouse.LendingInterestPoolGap, error) {
		return nil, nil
	}
	insertLendingInterest = func(_ context.Context, _ string, rows []lendinginterest.HourRow, rev uint16) error {
		if rev != lendingInterestRev {
			return fmt.Errorf("rev %d", rev)
		}
		f.calls = append(f.calls, "insert")
		f.inserted = append(f.inserted, rows...)
		return nil
	}
	deleteSupersededInterest = func(_ context.Context, _ string, _ []clickhouse.LendingContract, from, to, _ time.Time) error {
		f.calls = append(f.calls, "delete "+from.Format("15")+"-"+to.Format("15"))
		return nil
	}
	setLendingInterestWM = func(_ context.Context, _ string, from, thru uint32, _ []clickhouse.LendingContract) error {
		f.calls = append(f.calls, "mark")
		f.marks = append(f.marks, [2]uint32{from, thru})
		return nil
	}
	setLendingInterestPoolWM = func(_ context.Context, _ string, _ clickhouse.LendingContract, from, thru uint32) error {
		f.calls = append(f.calls, "pool-mark")
		f.marks = append(f.marks, [2]uint32{from, thru})
		return nil
	}
	lendingInterestServerNow = func(context.Context, string) (time.Time, error) {
		f.calls = append(f.calls, "now")
		return liT0, nil
	}
	lendingInterestRegistry = func(context.Context, string, uint32) ([]clickhouse.LendingContract, error) { return f.pools, nil }
	openLendingCodeHistory = func(context.Context, string) (lendingCodeHistory, func(), error) {
		return fakeCodeHistory(f.code), func() {}, nil
	}
}

type fakeCodeHistory map[string][]clickhouse.ContractCodeVersion

func (h fakeCodeHistory) ReplayCodeHistory(_ context.Context, id string) ([]clickhouse.ContractCodeVersion, error) {
	return h[id], nil
}

func liReserve(seq uint32, step int64) lendinginterest.Version {
	n := func(v int64) *big.Int { return big.NewInt(v) }
	return lendinginterest.Version{
		Pool: liPool, Kind: lendinginterest.KindReserve, Asset: liAsset, Ledger: seq, IntraLedgerSeq: 1,
		TxHash: fmt.Sprintf("tx%d", seq), CloseTime: liClose(seq),
		Reserve: blend.ReserveData{
			DRate: n(1_000_000_000_000 + step*1_000_000), BRate: n(1_000_000_000_000 + step*500_000), IRMod: n(1),
			BSupply: n(3_000_000_000_000), DSupply: n(2_000_000_000_000), BackstopCredit: n(step * 77),
			LastTime: uint64(liClose(seq).Unix()), Version: blend.PoolV2, //nolint:gosec // positive
		},
	}
}

// newFakeInterestLake is one blend_v2 pool whose reserve changes across a partition boundary that falls mid-hour.
func newFakeInterestLake() *fakeInterestLake {
	f := &fakeInterestLake{
		wm: 199_989, stateWM: 200_017, tip: 200_030,
		pools: []clickhouse.LendingContract{{ID: liPool, Protocol: clickhouse.LendingBlendV2, Deployed: 1}},
		code:  map[string][]clickhouse.ContractCodeVersion{liPool: {{Ledger: 1, WasmHash: liV2}}},
	}
	for i, seq := range []uint32{199_991, 199_994, 199_998, 200_001, 200_005, 200_012} {
		f.versions = append(f.versions, liReserve(seq, int64(i+1)))
	}
	return f
}

func runInterest(t *testing.T, r lendingRun) (string, error) {
	t.Helper()
	var log bytes.Buffer
	r.chAddr = "fake"
	err := r.interestPass(context.Background(), &log)
	return log.String(), err
}

// The hourly timer's run never starts a backfill: with no watermark it logs one line and reads and writes nothing.
func TestInterestPass_TimerWithoutWatermarkWritesNothing(t *testing.T) {
	f := newFakeInterestLake()
	f.wm = 0
	f.install(t)
	out, err := runInterest(t, lendingRun{})
	if err != nil || !strings.Contains(out, "lending interest: not initialised (run -interest-init)") {
		t.Fatalf("timer run: %v %q, want nil and the not-initialised line", err, out)
	}
	if len(f.reads)+len(f.calls)+f.seeds != 0 {
		t.Fatalf("timer run touched the lake: reads %v calls %v seeds %d", f.reads, f.calls, f.seeds)
	}
	// An explicit interest run on an uninitialised host is an operator error, not a silent no-op.
	if _, err := runInterest(t, lendingRun{interest: interestFlags{only: true}}); err == nil {
		t.Fatal("-interest-only before -interest-init succeeded")
	}
	f.wm = 5
	if _, err := runInterest(t, lendingRun{interest: interestFlags{init: true}}); err == nil {
		t.Fatal("-interest-init over an existing watermark succeeded")
	}
}

// A first run starts at the first ledger of the hour holding max(-floor-ledger, the factory genesis).
func TestInterestPass_InitStartsAtTheGenesisHour(t *testing.T) {
	f := newFakeInterestLake()
	g := blend.FactoryGenesisLedger
	f.wm, f.stateWM, f.tip, f.versions = 0, g+20, g+40, nil
	f.install(t)
	if _, err := runInterest(t, lendingRun{floor: 1, interest: interestFlags{init: true, only: true}}); err != nil {
		t.Fatal(err)
	}
	if want := g / liPerHr * liPerHr; len(f.reads) == 0 || f.reads[0][0] != want || len(f.marks) == 0 || f.marks[0][0] != want {
		t.Fatalf("reads %v marks %v, want both from %d", f.reads, f.marks, want)
	}
	f.wm, f.reads, f.marks = 0, nil, nil
	if _, err := runInterest(t, lendingRun{floor: g + 9, interest: interestFlags{init: true}}); err != nil {
		t.Fatal(err)
	}
	if want := (g + 9) / liPerHr * liPerHr; f.reads[0][0] != want {
		t.Fatalf("reads %v, want from %d (the floor's hour)", f.reads, want)
	}
}

// Two windows split an hour: the open hour rides in the carry, so each ledger is read once, the seeds are read once,
// the rows equal one fold over everything, and each mark ends on an hour's last ledger.
func TestInterestPass_CarryAcrossWindows(t *testing.T) {
	f := newFakeInterestLake()
	f.install(t)
	out, err := runInterest(t, lendingRun{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := [][2]uint32{{199_990, 199_999}, {200_000, 200_017}}; !slices.Equal(f.reads, want) {
		t.Fatalf("reads = %v, want %v (no ledger twice)", f.reads, want)
	}
	if want := [][2]uint32{{199_990, 199_996}, {199_997, 200_017}}; f.seeds != 1 || !slices.Equal(f.marks, want) {
		t.Fatalf("seeds %d marks %v, want 1 and %v", f.seeds, f.marks, want)
	}
	seed := lendinginterest.Seeds{CommittedBefore: liClose(199_990), Configs: map[string][]uint32{liPool: {2_000_000}}}
	whole, _, err := lendinginterest.Fold(seed, f.versions, liClose(200_018), lendinginterest.Options{
		Protocols: map[string]lendinginterest.Protocol{liPool: lendinginterest.BlendV2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 4 || len(f.inserted) != len(whole) {
		t.Fatalf("inserted %d rows, one fold gives %d (want 4)", len(f.inserted), len(whole))
	}
	for i := range whole {
		w, g := whole[i], f.inserted[i]
		if !w.Hour.Equal(g.Hour) || w.Gross.Cmp(g.Gross) != 0 || w.Versions != g.Versions || w.AccruedVersions != g.AccruedVersions {
			t.Errorf("row %d = %s gross %s versions %d/%d, want %s gross %s versions %d/%d", i, g.Hour, g.Gross, g.Versions,
				g.AccruedVersions, w.Hour, w.Gross, w.Versions, w.AccruedVersions)
		}
	}
	if whole[1].Versions != 2 || whole[1].Gross.Sign() <= 0 {
		t.Fatalf("degenerate fixture: the split hour holds %d versions, gross %s", whole[1].Versions, whole[1].Gross)
	}
	if slices.Contains(f.calls, "now") || slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "delete") }) {
		t.Fatalf("a resume deleted or read the clock: %v", f.calls)
	}
}

// An identity that fails refuses the window before anything is written, naming pool, asset, hour and both sides.
func TestInterestPass_IdentityFailureRefusesBeforeInsert(t *testing.T) {
	f := newFakeInterestLake()
	f.tamper = func(rows []clickhouse.LendingInterestStateRow) {
		for i := range rows {
			if !rows[i].Before && rows[i].Ledger == 199_994 {
				r := rows[i].Value.Reserve
				r.BackstopCredit = new(big.Int).Add(r.BackstopCredit, big.NewInt(1))
				rows[i].Value.Reserve = r
			}
		}
	}
	f.install(t)
	_, err := runInterest(t, lendingRun{})
	if err == nil || !strings.Contains(err.Error(), liPool) || !strings.Contains(err.Error(), liAsset) ||
		!strings.Contains(err.Error(), liClose(199_990).Format(time.RFC3339)) || !strings.Contains(err.Error(), "backstop_credit moved") {
		t.Fatalf("err = %v, want the backstop identity naming pool, asset and hour", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls after a failed identity: %v", f.calls)
	}
	// An hour lending_state holds that the fold does not is refused too.
	f2 := newFakeInterestLake()
	f2.tamper = func(rows []clickhouse.LendingInterestStateRow) {
		for i := range rows {
			if !rows[i].Before && rows[i].Ledger == 199_994 {
				rows[i].Ledger = 199_995
			}
		}
	}
	f2.install(t)
	if _, err := runInterest(t, lendingRun{}); err == nil || !strings.Contains(err.Error(), "last_ledger 199994") || len(f2.calls) != 0 {
		t.Fatalf("err = %v calls %v, want a last_ledger refusal and nothing written", err, f2.calls)
	}
}

// Without -write the pass reads, folds and checks, prints the totals, and writes nothing.
func TestInterestPass_DryRunWritesNothing(t *testing.T) {
	f := newFakeInterestLake()
	f.install(t)
	out, err := runInterest(t, lendingRun{dryRun: true, from: 199_992, interest: interestFlags{only: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || len(f.reads) != 2 || !strings.Contains(out, "would write") || !strings.Contains(out, "identities hold") {
		t.Fatalf("dry run: calls %v reads %v log %q", f.calls, f.reads, out)
	}
}

// -interest-max-windows bounds one run; the next resumes from the watermark.
func TestInterestPass_MaxWindows(t *testing.T) {
	f := newFakeInterestLake()
	f.install(t)
	out, err := runInterest(t, lendingRun{interest: interestFlags{maxWindows: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.reads) != 1 || !slices.Equal(f.marks, [][2]uint32{{199_990, 199_996}}) || !strings.Contains(out, "stopping after 1 windows at 199996") {
		t.Fatalf("reads %v marks %v log %q", f.reads, f.marks, out)
	}
}

// A re-derive snaps -from down and -to up to whole hours, reads the clock before the insert, and deletes the hours'
// older rows after it, before the mark.
func TestInterestPass_RederiveSnapsAndDeletesSuperseded(t *testing.T) {
	f := newFakeInterestLake()
	f.wm = 200_017
	f.install(t)
	if _, err := runInterest(t, lendingRun{from: 199_992, to: 200_002, interest: interestFlags{only: true}}); err != nil {
		t.Fatal(err)
	}
	if want := [][2]uint32{{199_990, 199_999}, {200_000, 200_003}}; !slices.Equal(f.reads, want) {
		t.Fatalf("reads = %v, want %v", f.reads, want)
	}
	h := func(seq uint32) string { return liClose(seq).Format("15") }
	want := []string{"now", "insert", "delete " + h(199_990) + "-" + h(199_990), "mark", "now", "insert", "delete " + h(199_997) + "-" + h(199_997), "mark"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	f.wm = 199_996
	if _, err := runInterest(t, lendingRun{from: 200_012, interest: interestFlags{only: true}}); err == nil {
		t.Fatal("-from above the watermark+1 accepted")
	}
}

// A pool running unaudited code refuses the window and leaves the watermark where it was.
func TestInterestPass_UnlistedWASMRefuses(t *testing.T) {
	f := newFakeInterestLake()
	f.code[liPool] = append(f.code[liPool], clickhouse.ContractCodeVersion{Ledger: 200_004, WasmHash: strings.Repeat("ab", 32)})
	f.install(t)
	_, err := runInterest(t, lendingRun{})
	if err == nil || !strings.Contains(err.Error(), strings.Repeat("ab", 32)) || !strings.Contains(err.Error(), "watermark holds at 200003") {
		t.Fatalf("err = %v, want the hash named and the watermark held", err)
	}
	// The window was split at the upgrade: everything before it is derived and marked.
	if want := [][2]uint32{{199_990, 199_996}, {199_997, 200_003}}; !slices.Equal(f.marks, want) {
		t.Fatalf("marks = %v, want %v", f.marks, want)
	}
}

func TestGateLendingWASM(t *testing.T) {
	v1 := "baf978f10efdbcd85747868bef8832845ea6809f7643b67a4ac0cd669327fc2c"
	other := strings.Repeat("cd", 32)
	pool := func(proto string, deployed uint32) []clickhouse.LendingContract {
		return []clickhouse.LendingContract{{ID: "P", Protocol: proto, Deployed: deployed}}
	}
	code := func(vs ...clickhouse.ContractCodeVersion) map[string][]clickhouse.ContractCodeVersion {
		return map[string][]clickhouse.ContractCodeVersion{"P": vs}
	}
	for _, c := range []struct {
		name    string
		pools   []clickhouse.LendingContract
		code    map[string][]clickhouse.ContractCodeVersion
		wantHi  uint32
		wantErr string
	}{
		{"listed", pool(clickhouse.LendingBlendV2, 10), code(clickhouse.ContractCodeVersion{Ledger: 10, WasmHash: liV2}), 200, ""},
		{"unlisted", pool(clickhouse.LendingBlendV2, 10), code(clickhouse.ContractCodeVersion{Ledger: 10, WasmHash: other}), 0, other},
		{"mismatch", pool(clickhouse.LendingBlendV2, 10), code(clickhouse.ContractCodeVersion{Ledger: 10, WasmHash: v1}), 0, "audited as blend_v1"},
		{"no history", pool(clickhouse.LendingBlendV2, 10), code(), 0, "no WASM recorded at ledger 100"},
		{"deployed later", pool(clickhouse.LendingBlendV2, 300), code(), 200, ""},
		{
			"change mid-window splits", pool(clickhouse.LendingBlendV2, 10),
			code(clickhouse.ContractCodeVersion{Ledger: 10, WasmHash: liV2}, clickhouse.ContractCodeVersion{Ledger: 150, WasmHash: other}), 149, "",
		},
		{
			"deploy inside the window is not a change", pool(clickhouse.LendingBlendV2, 120),
			code(clickhouse.ContractCodeVersion{Ledger: 120, WasmHash: liV2}), 200, "",
		},
	} {
		hi, err := gateLendingWASM(c.code, c.pools, 100, 200)
		switch {
		case c.wantErr == "" && (err != nil || hi != c.wantHi):
			t.Errorf("%s: hi %d err %v, want %d", c.name, hi, err, c.wantHi)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err %v, want %q", c.name, err, c.wantErr)
		}
	}
	// After the split, the window starting at the change sees the new hash, and refuses it.
	if _, err := gateLendingWASM(code(clickhouse.ContractCodeVersion{Ledger: 10, WasmHash: liV2},
		clickhouse.ContractCodeVersion{Ledger: 150, WasmHash: other}), pool(clickhouse.LendingBlendV2, 10), 150, 200); err == nil {
		t.Fatal("window from the upgrade accepted the unlisted hash")
	}
}

// The BLNT fee identity: carry plus rate·gross, less the units credited, is the next carry.
func TestCheckFeeCarry(t *testing.T) {
	n := big.NewInt
	h := liT0
	row := lendinginterest.HourRow{Pool: "P", Asset: "A", Hour: h, Gross: n(1_000), FeeCreditResidual: n(0)}
	st := &interestState{
		before: &clickhouse.LendingInterestStateRow{Before: true},
		in:     []clickhouse.LendingInterestStateRow{{Hour: h}},
	}
	st.before.Value.Fees = blnt.ProtocolFees{"A": {Credit: n(5), Carry: n(9_000_000)}}
	// 9e6 + 1000·20000 = 2.9e7: two units credited, carry 9e6.
	st.in[0].Value.Fees = blnt.ProtocolFees{"A": {Credit: n(7), Carry: n(9_000_000)}}
	if err := checkFeeCarry(row, st); err != nil {
		t.Fatal(err)
	}
	st.in[0].Value.Fees = blnt.ProtocolFees{"A": {Credit: n(7), Carry: n(9_000_001)}}
	if err := checkFeeCarry(row, st); err == nil || !strings.Contains(err.Error(), "carry 9000001") {
		t.Fatalf("err = %v, want the carry named", err)
	}
	// An hour whose entry did not change holds Ef = Pf: no gross, no movement.
	if err := checkFeeCarry(lendinginterest.HourRow{Pool: "P", Asset: "A", Hour: h.Add(time.Hour), Gross: n(0), FeeCreditResidual: n(0)}, st); err != nil {
		t.Fatal(err)
	}
	// A deleted entry (the empty map) reads as zero; the dropped credit is the residual.
	st.before.Value.Fees = blnt.ProtocolFees{"A": {Credit: n(5), Carry: n(0)}}
	st.in[0].Value.Fees = blnt.ProtocolFees{}
	row.Gross, row.FeeCreditResidual = n(0), n(-5)
	if err := checkFeeCarry(row, st); err != nil {
		t.Fatal(err)
	}
	// Pairs left out of the sums leave the fee movement unrecorded: not checked.
	row.FeeCreditResidual, row.AccrualErrors = n(0), 1
	if err := checkFeeCarry(row, st); err != nil {
		t.Fatal(err)
	}
}
