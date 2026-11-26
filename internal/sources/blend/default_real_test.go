package blend

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
)

func readDefaultTSV(t *testing.T, name string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "defaulted_debt", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out [][]string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<16), 1<<20)
	for first := true; s.Scan(); first = false {
		if !first {
			out = append(out, strings.Split(s.Text(), "\t"))
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// decodeResData returns a ResData entry's pool, asset, last-modified ledger and reserve.
func decodeResData(t *testing.T, entryB64 string) (pool, asset string, ledger uint32, rd ReserveData) {
	t.Helper()
	var e xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryB64, &e); err != nil {
		t.Fatal(err)
	}
	cd, ok := e.Data.GetContractData()
	key, okKey := cd.Key.GetVec()
	if !ok || !okKey || key == nil || len(*key) != 2 {
		t.Fatal("not a ResData entry")
	}
	addr, ok := (*key)[1].GetAddress()
	asset, okA := scval.ContractIDFromScAddress(addr)
	pool, okP := scval.ContractIDFromScAddress(cd.Contract)
	if !ok || !okA || !okP {
		t.Fatal("ResData key without contract addresses")
	}
	rd, err := DecodeReserveData(cd.Val, PoolV2)
	if err != nil {
		t.Fatal(err)
	}
	return pool, asset, uint32(e.LastModifiedLedgerSeq), rd
}

func rowOf(rd ReserveData, ledger uint32, tx string) blendtest.Row {
	return blendtest.Row{
		Ledger: ledger, Tx: tx, DRate: rd.DRate.String(), BRate: rd.BRate.String(), BSupply: rd.BSupply.String(),
		DSupply: rd.DSupply.String(), BackstopCredit: rd.BackstopCredit.String(), LastTime: rd.LastTime,
	}
}

// The Go golden data is the lake's XDR decoded: each pair is the transaction's state pre-image and updated version,
// and each event's asset topic and i128 body are the version's.
func TestDefaulted_GoldenMatchesXDR(t *testing.T) {
	rows := readDefaultTSV(t, "reserves.tsv")
	if len(rows) != 2*len(blendtest.DefaultedDebt) {
		t.Fatalf("%d entries for %d versions", len(rows), len(blendtest.DefaultedDebt))
	}
	for i, d := range blendtest.DefaultedDebt {
		s, u := rows[2*i], rows[2*i+1]
		pool, asset, prevLedger, prev := decodeResData(t, s[4])
		pool2, asset2, _, next := decodeResData(t, u[4])
		got := blendtest.Default{
			Pool: pool, Asset: asset, Ledger: d.Ledger, Tx: u[1], Prev: rowOf(prev, prevLedger, ""), Next: rowOf(next, d.Ledger, u[1]),
		}
		want := d
		want.BstopRate, want.EventIndex, want.DTokens, want.SupplyResidual = 0, 0, "", ""
		if s[3] != "state" || u[3] != "updated" || s[1] != u[1] || u[0] != strconv.Itoa(int(d.Ledger)) ||
			pool2 != pool || asset2 != asset || got != want {
			t.Errorf("version %d: XDR %+v, golden %+v", i, got, want)
		}
	}
	n := 0
	for _, ev := range readDefaultTSV(t, "events.tsv") {
		var topic, body xdr.ScVal
		if err := xdr.SafeUnmarshalBase64(ev[3], &topic); err != nil {
			t.Fatal(err)
		}
		if err := xdr.SafeUnmarshalBase64(ev[4], &body); err != nil {
			t.Fatal(err)
		}
		addr, _ := topic.GetAddress()
		asset, _ := scval.ContractIDFromScAddress(addr)
		amt, err := scval.AsAmountFromI128(body)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range blendtest.DefaultedDebt {
			if d.Tx == ev[1] && d.Asset == asset && strconv.Itoa(int(d.EventIndex)) == ev[2] && d.DTokens == amt.BigInt().String() {
				n++
			}
		}
	}
	if n != len(blendtest.DefaultedDebt) {
		t.Errorf("%d events match a golden version, want %d", n, len(blendtest.DefaultedDebt))
	}
}

func goldenReserve(t *testing.T, r blendtest.Row) ReserveData {
	t.Helper()
	return rdIn{r.DRate, r.BRate, r.BSupply, r.DSupply, r.BackstopCredit, r.LastTime}.data(t, PoolV2)
}

// The 8 mainnet versions the full-history interest proof left unexplained: accrual, then the transaction's
// defaulted_debt for the asset, gives the stored b_rate exactly, with no backstop residual at the pool's rate.
func TestDefaultedBRate_ReproducesMainnet(t *testing.T) {
	clamped := 0
	for _, d := range blendtest.DefaultedDebt {
		prev, next := goldenReserve(t, d.Prev), goldenReserve(t, d.Next)
		a, err := AccrueBetween(prev, next, d.BstopRate)
		if err != nil {
			t.Fatal(err)
		}
		if a.BackstopResidual.Sign() != 0 || a.BRateMatches {
			t.Errorf("%d %s: backstop residual %s, b_rate matches %v", d.Ledger, d.Asset, a.BackstopResidual, a.BRateMatches)
		}
		got, err := DefaultedBRate(ReserveData{DRate: next.DRate, BRate: a.ExpectedBRate, BSupply: prev.BSupply, Version: PoolV2},
			mustBig(t, d.DTokens))
		if err != nil {
			t.Fatal(err)
		}
		if got.Cmp(next.BRate) != 0 {
			t.Errorf("%d %s: b_rate %s, stored %s (accrual expected %s)", d.Ledger, d.Asset, got, next.BRate, a.ExpectedBRate)
		}
		if next.BRate.Sign() == 0 {
			clamped++
		}
	}
	if clamped != 1 {
		t.Errorf("%d versions clamp b_rate to 0, want 1", clamped)
	}
	// The rate is pinned by backstop_credit alone: one unit either side leaves a residual.
	d := blendtest.DefaultedDebt[7]
	for _, bstop := range []uint32{d.BstopRate - 1, d.BstopRate + 1} {
		if a, err := AccrueBetween(goldenReserve(t, d.Prev), goldenReserve(t, d.Next), bstop); err != nil || a.BackstopResidual.Sign() == 0 {
			t.Errorf("bstop_rate %d: residual %v (%v)", bstop, a.BackstopResidual, err)
		}
	}
}
