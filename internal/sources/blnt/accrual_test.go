package blnt

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
)

func dec(t testing.TB, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad decimal %q", s)
	}
	return n
}

type rdIn struct {
	dRate, bRate, bSupply, dSupply, bc string
	lt                                 uint64
}

func (r rdIn) data(t testing.TB) ReserveData {
	t.Helper()
	return ReserveData{
		DRate: dec(t, r.dRate), BRate: dec(t, r.bRate), IRMod: big.NewInt(1),
		BSupply: dec(t, r.bSupply), DSupply: dec(t, r.dSupply),
		BackstopCredit: dec(t, r.bc), LastTime: r.lt, Version: blend.PoolV2,
	}
}

func fee(t testing.TB, credit, carry string) ProtocolFee {
	t.Helper()
	return ProtocolFee{Credit: dec(t, credit), Carry: dec(t, carry)}
}

// A ProtocolFees entry as stored by tx 7a04ad29…94dd (ledger 65648345) on pool CACSQWCM…MCY7.
const mainnetProtocolFeesXDR = "A+m22QAAAAYAAAAAAAAAAQUoWExRudnQ4obXc7g/wQiT8tv/sng+59IzuZONScuWAAAAEAAAAAEAAAABAAAADwAAAAxQcm90b2NvbEZlZXMAAAABAAAAEQAAAAEAAAADAAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dwAAABEAAAABAAAAAgAAAA8AAAAFY2FycnkAAAAAAAAKAAAAAAAAAAAAAAAAAAUwIAAAAA8AAAAGY3JlZGl0AAAAAAAKAAAAAAAAAAAAAAAAAA4elwAAABIAAAABre/OWa7lKWj3YGHUlMJSW3Vln6QpamX0me8p5WR35JYAAAARAAAAAQAAAAIAAAAPAAAABWNhcnJ5AAAAAAAACgAAAAAAAAAAAAAAAAA982AAAAAPAAAABmNyZWRpdAAAAAAACgAAAAAAAAAAAAAAAACOd+wAAAASAAAAAean2et1IwBqRpqnSDrREHJHRDwNguYnY95nCEjE6XyQAAAAEQAAAAEAAAACAAAADwAAAAVjYXJyeQAAAAAAAAoAAAAAAAAAAAAAAAAAERcAAAAADwAAAAZjcmVkaXQAAAAAAAoAAAAAAAAAAAAAAAAAtZoaAAAAAA==" // gitleaks:allow public ledger entry

func TestDecodeProtocolFees_Mainnet(t *testing.T) {
	got, err := DecodeProtocolFees(contractDataVal(t, mainnetProtocolFeesXDR))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA": {"925335", "340000"},
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": {"9336812", "4060000"},
		"CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV": {"11901466", "1120000"},
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d assets, want %d", len(got), len(want))
	}
	for asset, w := range want {
		f, ok := got[asset]
		if !ok || f.Credit.String() != w[0] || f.Carry.String() != w[1] {
			t.Errorf("%s = %+v, want credit %s carry %s", asset, f, w[0], w[1])
		}
	}
}

func i128n(n int64) xdr.ScVal {
	parts := xdr.Int128Parts{Lo: xdr.Uint64(uint64(n))} //nolint:gosec // two's-complement split of a test constant
	if n < 0 {
		parts.Hi = -1
	}
	return i128(parts)
}

func mapVal(entries ...xdr.ScMapEntry) xdr.ScVal {
	m := xdr.ScMap(entries)
	pm := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
}

func TestDecodeProtocolFees_Refuses(t *testing.T) {
	stored := contractDataVal(t, mainnetProtocolFeesXDR)
	first := (*stored.MustMap())[0]
	inner := func(entries ...xdr.ScMapEntry) xdr.ScVal {
		return mapVal(xdr.ScMapEntry{Key: first.Key, Val: mapVal(entries...)})
	}
	carry := func(n int64) xdr.ScMapEntry { return xdr.ScMapEntry{Key: sym("carry"), Val: i128n(n)} }
	credit := func(n int64) xdr.ScMapEntry { return xdr.ScMapEntry{Key: sym("credit"), Val: i128n(n)} }
	cases := map[string]xdr.ScVal{
		"not a map":         i128n(1),
		"symbol key":        mapVal(xdr.ScMapEntry{Key: sym("XLM"), Val: first.Val}),
		"duplicate asset":   mapVal(first, first),
		"extra field":       inner(carry(0), credit(1), xdr.ScMapEntry{Key: sym("rate"), Val: i128n(20_000)}),
		"missing credit":    inner(carry(0), xdr.ScMapEntry{Key: sym("credits"), Val: i128n(1)}),
		"carry is one unit": inner(carry(10_000_000), credit(1)),
		"negative carry":    inner(carry(-1), credit(1)),
		"negative credit":   inner(carry(0), credit(-1)),
		"credit not i128":   inner(carry(0), xdr.ScMapEntry{Key: sym("credit"), Val: sym("x")}),
	}
	for name, v := range cases {
		if got, err := DecodeProtocolFees(v); err == nil {
			t.Errorf("%s: decoded %+v, want an error", name, got)
		}
	}
	if _, err := DecodeProtocolFees(inner(carry(9_999_999), credit(0))); err != nil {
		t.Errorf("carry just below one unit: %v", err)
	}
}

type blntWant struct {
	gross, fee, backstop, intended, realised, bRate, credit, carry, feeResidual, bcResidual string
	bRateMatches, feesMatch                                                                 bool
}

func assertBLNT(t *testing.T, a Accrual, w blntWant) {
	t.Helper()
	for _, c := range []struct {
		name string
		got  *big.Int
		want string
	}{
		{"Gross", a.Gross, w.gross},
		{"ProtocolFee", a.ProtocolFee, w.fee},
		{"Backstop", a.Backstop, w.backstop},
		{"SupplierIntended", a.SupplierIntended, w.intended},
		{"SupplierRealised", a.SupplierRealised, w.realised},
		{"ExpectedBRate", a.ExpectedBRate, w.bRate},
		{"ExpectedFees.Credit", a.ExpectedFees.Credit, w.credit},
		{"ExpectedFees.Carry", a.ExpectedFees.Carry, w.carry},
		{"FeeCreditResidual", a.FeeCreditResidual, w.feeResidual},
		{"BackstopResidual", a.BackstopResidual, w.bcResidual},
	} {
		if c.got.Cmp(dec(t, c.want)) != 0 {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
	if a.BRateMatches != w.bRateMatches || a.FeesMatch != w.feesMatch {
		t.Errorf("BRateMatches %v FeesMatch %v, want %v %v", a.BRateMatches, a.FeesMatch, w.bRateMatches, w.feesMatch)
	}
}

// Hand-computed (gross 1,234,568). Each rounding is fractional, so each alternative moves a value:
// a ceil fee gives 2470 at carry 0; dropping the carry gives 2469 at carry 9e6; a backstop share
// on gross gives 246913 and a ceil one 246420.
func TestAccrueBetween_Vectors(t *testing.T) {
	prev := rdIn{"1100000000000", "1050000000000", "1300000000013", "987654321987", "500", 100}
	next := rdIn{"1100001250000", "1050000758214", "1300000000013", "987654321987", "246919", 160}
	cases := []struct {
		name            string
		prevFee, nextFe ProtocolFee
		want            blntWant
	}{
		{
			"fee_floor_at_carry_zero", fee(t, "7", "0"), fee(t, "2476", "1360000"),
			blntWant{"1234568", "2469", "246419", "985680", "985678", "1050000758214", "2476", "1360000", "0", "0", true, true},
		},
		{
			"carry_crosses_a_unit", fee(t, "7", "9000000"), fee(t, "2477", "360000"),
			blntWant{"1234568", "2470", "246419", "985679", "985678", "1050000758214", "2477", "360000", "0", "0", true, true},
		},
		{
			// The same chain values read as a fee withdrawal of 2000 units: reported, never coerced.
			"fee_withdrawal_reported", fee(t, "7", "0"), fee(t, "476", "1360000"),
			blntWant{"1234568", "2469", "246419", "985680", "985678", "1050000758214", "2476", "1360000", "-2000", "0", true, false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := AccrueBetween(prev.data(t), next.data(t), c.prevFee, c.nextFe, 2_000_000)
			if err != nil {
				t.Fatal(err)
			}
			assertBLNT(t, a, c.want)
		})
	}
}

// d_supply and b_supply above 2^82 and a fee credit above 2^64.
func TestAccrueBetween_AboveInt64(t *testing.T) {
	prev := rdIn{"1100000000000", "1050000000000", "7000000000000000000000003", "5000000000000000000000011", "100000000000000000000", 0}
	next := rdIn{"1100001250000", "1050000712857", "7000000000000000000000003", "5000000000000000000000011", "101247500000000000000", 5}
	a, err := AccrueBetween(prev.data(t), next.data(t), fee(t, "18446744073709551616", "9990000"),
		fee(t, "18459244073709551616", "9990000"), 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	assertBLNT(t, a, blntWant{
		"6250000000000000000", "12500000000000000", "1247500000000000000", "4990000000000000000",
		"4989999000000000000", "1050000712857", "18459244073709551616", "9990000", "0", "0", true, true,
	})
}

// With no accrual the fee entry must not move and the fee is never computed.
func TestAccrueBetween_NoAccrualKeepsFees(t *testing.T) {
	v := rdIn{"1100000000000", "1050000000000", "1300000000013", "987654321987", "500", 100}
	a, err := AccrueBetween(v.data(t), v.data(t), fee(t, "7", "9000000"), fee(t, "7", "9000000"), 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	assertBLNT(t, a, blntWant{"0", "0", "0", "0", "0", "1050000000000", "7", "9000000", "0", "0", true, true})
}

func TestAccrueBetween_Refuses(t *testing.T) {
	prev := rdIn{"1100000000000", "1050000000000", "1300000000013", "987654321987", "500", 100}.data(t)
	next := rdIn{"1100001250000", "1050000758214", "1300000000013", "987654321987", "246919", 160}.data(t)
	ok := fee(t, "7", "0")
	v1, v1Next := prev, next
	v1.Version, v1Next.Version = blend.PoolV1, blend.PoolV1
	cases := map[string]func() (Accrual, error){
		"nil carry": func() (Accrual, error) {
			return AccrueBetween(prev, next, ProtocolFee{Credit: big.NewInt(1)}, ok, 2_000_000)
		},
		"carry of one unit": func() (Accrual, error) { return AccrueBetween(prev, next, fee(t, "7", "10000000"), ok, 2_000_000) },
		"negative credit":   func() (Accrual, error) { return AccrueBetween(prev, next, ok, fee(t, "-1", "0"), 2_000_000) },
		"mixed versions":    func() (Accrual, error) { return AccrueBetween(v1, next, ok, ok, 2_000_000) },
		"v1 data":           func() (Accrual, error) { return AccrueBetween(v1, v1Next, ok, ok, 2_000_000) },
		"bstop_rate 100%":   func() (Accrual, error) { return AccrueBetween(prev, next, ok, ok, 10_000_000) },
		"d_rate decreasing": func() (Accrual, error) { return AccrueBetween(next, prev, ok, ok, 2_000_000) },
	}
	for name, call := range cases {
		if a, err := call(); err == nil {
			t.Errorf("%s: got %+v, want an error", name, a)
		}
	}
}

// Golden slices: consecutive stored versions of one reserve, each with the asset's ProtocolFees
// entry as stored after that transaction. They sample a larger replay run outside the repo over
// every stored change of BLNT pools CACSQWCM…MCY7 (ledgers 65638200–65658199: 741 version pairs),
// CAUTPSQD…VF4D (65578200–65658199: 65) and CCH3BNZW…C3XD (65578200–65658199: 7). There every pair
// reproduced b_rate and the fee entry, and every pair but CACSQWCM…'s three at 65648345 (below)
// reproduced backstop_credit.
type mainnetVersion struct {
	ledger        uint32
	tx            string
	rd            rdIn
	credit, carry string
}

type mainnetSlice struct {
	name  string
	bstop uint32 // the pool's Config bstop_rate
	rows  []mainnetVersion
	// reported maps a ledger to the BackstopResidual of a version accrual alone cannot explain.
	reported map[uint32]string
}

// mainnetSlices converts blendtest.BLNT, whose pools and reserves are named there.
var mainnetSlices = func() []mainnetSlice {
	out := make([]mainnetSlice, 0, len(blendtest.BLNT))
	for _, s := range blendtest.BLNT {
		rows := make([]mainnetVersion, len(s.Rows))
		for i, r := range s.Rows {
			rows[i] = mainnetVersion{r.Ledger, r.Tx, rdIn{r.DRate, r.BRate, r.BSupply, r.DSupply, r.BackstopCredit, r.LastTime}, r.FeeCredit, r.FeeCarry}
		}
		out = append(out, mainnetSlice{s.Name, s.BstopRate, rows, s.Reported})
	}
	return out
}()

func TestAccrueBetween_MainnetGolden(t *testing.T) {
	if len(mainnetSlices) != 4 || mainnetSlices[0].reported[65648345] != "-112512682" {
		t.Fatalf("golden slices: %d, CACSQ_CCW67 residual %q", len(mainnetSlices), mainnetSlices[0].reported[65648345])
	}
	for _, s := range mainnetSlices {
		t.Run(s.name, func(t *testing.T) {
			rows := s.rows
			gross := new(big.Int)
			for i := 1; i < len(rows); i++ {
				p, n := rows[i-1], rows[i]
				a, err := AccrueBetween(p.rd.data(t), n.rd.data(t), fee(t, p.credit, p.carry), fee(t, n.credit, n.carry), s.bstop)
				if err != nil {
					t.Fatalf("ledger %d tx %s: %v", n.ledger, n.tx, err)
				}
				wantResidual := "0"
				if r, ok := s.reported[n.ledger]; ok {
					wantResidual = r
				}
				if !a.BRateMatches || !a.FeesMatch || a.BackstopResidual.String() != wantResidual {
					t.Errorf("ledger %d tx %s: b_rate %s recomputed %s; fees %s/%s expected %s/%s; residual %s want %s",
						n.ledger, n.tx, n.rd.bRate, a.ExpectedBRate, n.credit, n.carry, a.ExpectedFees.Credit,
						a.ExpectedFees.Carry, a.BackstopResidual, wantResidual)
				}
				if (p.rd.lt == n.rd.lt) != (a.Gross.Sign() == 0) {
					t.Errorf("ledger %d tx %s: gross %s across last_time %d → %d", n.ledger, n.tx, a.Gross, p.rd.lt, n.rd.lt)
				}
				gross.Add(gross, a.Gross)
			}
			want := telescopedGross(t, rows)
			if gross.Cmp(want) != 0 {
				t.Errorf("Σ gross = %s, telescoped from chain values %s", gross, want)
			}
			// Consistency check on chain values only: carry₀ + rate·gross = 1e7·Δcredit + carryₙ, so the
			// stored fee entry moved by exactly 0.2% of the telescoped gross, with nothing lost.
			first, last := rows[0], rows[len(rows)-1]
			lhs := new(big.Int).Mul(want, big.NewInt(ProtocolInterestFeeRate))
			lhs.Add(lhs, dec(t, first.carry))
			rhs := new(big.Int).Sub(dec(t, last.credit), dec(t, first.credit))
			rhs.Mul(rhs, big.NewInt(10_000_000)).Add(rhs, dec(t, last.carry))
			if lhs.Cmp(rhs) != 0 {
				t.Errorf("carry₀ + rate·gross = %s, 1e7·Δcredit + carryₙ = %s", lhs, rhs)
			}
		})
	}
}

// telescopedGross sums ceil(d_supply·d_rate/1e12) differences over each run of versions whose
// d_supply (the accrual operand) is constant, from chain values alone.
func telescopedGross(t *testing.T, rows []mainnetVersion) *big.Int {
	t.Helper()
	liab := func(dSupply, dRate string) *big.Int {
		n := new(big.Int).Mul(dec(t, dSupply), dec(t, dRate))
		n.Add(n, big.NewInt(999_999_999_999))
		return n.Quo(n, big.NewInt(1_000_000_000_000))
	}
	total, j := new(big.Int), 0
	for i := 1; i < len(rows); i++ {
		if i == len(rows)-1 || rows[i].rd.dSupply != rows[j].rd.dSupply {
			total.Add(total, liab(rows[j].rd.dSupply, rows[i].rd.dRate))
			total.Sub(total, liab(rows[j].rd.dSupply, rows[j].rd.dRate))
			j = i
		}
	}
	return total
}
