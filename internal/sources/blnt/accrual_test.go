package blnt

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
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
	v1 := prev
	v1.Version = blend.PoolV1
	cases := map[string]func() (Accrual, error){
		"nil carry": func() (Accrual, error) {
			return AccrueBetween(prev, next, ProtocolFee{Credit: big.NewInt(1)}, ok, 2_000_000)
		},
		"carry of one unit": func() (Accrual, error) { return AccrueBetween(prev, next, fee(t, "7", "10000000"), ok, 2_000_000) },
		"negative credit":   func() (Accrual, error) { return AccrueBetween(prev, next, ok, fee(t, "-1", "0"), 2_000_000) },
		"mixed versions":    func() (Accrual, error) { return AccrueBetween(v1, next, ok, ok, 2_000_000) },
		"bstop_rate 100%":   func() (Accrual, error) { return AccrueBetween(prev, next, ok, ok, 10_000_000) },
		"d_rate decreasing": func() (Accrual, error) { return AccrueBetween(next, prev, ok, ok, 2_000_000) },
	}
	for name, call := range cases {
		if a, err := call(); err == nil {
			t.Errorf("%s: got %+v, want an error", name, a)
		}
	}
}

// Fourteen consecutive stored versions of reserve CCW67TSZ…MI75 (USDC) on BLNT pool
// CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7, each with the asset's ProtocolFees
// entry as stored after that transaction. Config bstop_rate is 0_2000000. Across ledgers
// 65638200–65658199 all 741 version pairs of the pool's three reserves reproduce b_rate and the fee
// entry exactly, and 738 reproduce backstop_credit; the other three are this slice's 65648345.
type mainnetVersion struct {
	ledger        uint32
	tx            string
	rd            rdIn
	credit, carry string
}

var mainnetUSDC = []mainnetVersion{
	{65648088, "5ca3ddd0baf558c743aa37d5d5b86644c0f3f326c54f2c290d201e37534d972e", rdIn{"1003719138822", "1002201432987", "1809367655146", "1558191411881", "139934814", 1795526492}, "9328091", "5280000"},
	{65648103, "99091a67f61fdcbbc4176ebf60abd15bc2ad4fb181efb48679ad7b70b06bda94", rdIn{"1003719304969", "1002201547223", "1809367655146", "1558191411881", "139986488", 1795526567}, "9328609", "3040000"},
	{65648134, "2e46a2260149c93f4ec7b9227c098d13a4234bb3d0acad53785d0b7ccb5d6892", rdIn{"1003719648345", "1002201783316", "1812410951930", "1558191411881", "140093283", 1795526722}, "9329679", "3960000"},
	{65648156, "dfb403f2512492f1c6930f45e8c3cc7554d51ac4633e4a8bae404c10182cc6a7", rdIn{"1003719889687", "1002201948975", "1809367655147", "1558191411881", "140168344", 1795526832}, "9330431", "5100000"},
	{65648199, "79923026882176c94156955fedcb5b09bee79d9cb7e88ff37e8128c91ff39c43", rdIn{"1003720366011", "1002202276479", "1829482546516", "1558191411881", "140316488", 1795527047}, "9331915", "9180000"},
	{65648215, "0f9378a357d152c2305d4852658f725d530263f20ae8c0e277b8d45bea0d4ae3", rdIn{"1003720532050", "1002202389385", "1832525644005", "1558191411881", "140368128", 1795527127}, "9332433", "3580000"},
	{65648235, "858b923674ebef306dd360b981c879b33d58694192edbedd21725f0f7b89c18c", rdIn{"1003720737509", "1002202528866", "1829482546513", "1558191411881", "140432029", 1795527227}, "9333073", "6480000"},
	{65648262, "16a9a780664edfda8accfcb4c46ea77dd09884320a59f19726f77208fc9d7d1e", rdIn{"1003721017709", "1002202719403", "1809367655144", "1558191411881", "140519175", 1795527362}, "9333946", "8580000"},
	{65648345, "7a04ad29325aee05fef312688bfe2c5c07f4556e330cf0136ba7afafc68794dd", rdIn{"1003721937220", "1002203351627", "1809367655144", "1558191411881", "28292474", 1795527777}, "9336812", "4060000"},
	{65648532, "4b425fc7248ac6d8425e3b51dc78b80b650a2db1004168ebeef2730fc2ab1ed1", rdIn{"1003724009078", "1002204776166", "1812410745386", "1558191411881", "28936853", 1795528712}, "9343269", "1100000"},
	{65648547, "7e6210fbafb59e9eab968abd8dbdc35fae554e768713dc902df58765320b4e21", rdIn{"1003724173700", "1002204889164", "1832531694830", "1558191411881", "28988052", 1795528787}, "9343782", "1340000"},
	{65648553, "15f0f3cc0386add0528d0d0e0b8de53aead3c2149b6ac3e955f71d1dac66963b", rdIn{"1003724235359", "1002204931022", "1829488604588", "1558191411881", "29007229", 1795528817}, "9343974", "2880000"},
	{65648572, "cff11c2c9475b5f1652951efbae629fbf64c0950bd9d5b4b9d544cb386af40e3", rdIn{"1003724432601", "1002205065147", "1809367655144", "1558191411881", "29068574", 1795528912}, "9344588", "9700000"},
	{65648942, "9997124177b1ee49428bb39174b28a9941d2edc8929b71b3887c774011f6f09a", rdIn{"1003728533011", "1002207884449", "1812412944553", "1558191411881", "30343862", 1795530762}, "9357367", "4160000"},
}

// At 65648345 the transaction also drew backstop_credit on all three reserves by amounts unrelated
// to accrual, which only an interest-auction fill does; b_rate and the fee entry still reconcile.
var mainnetReported = map[uint32]string{65648345: "-112512682"}

func TestAccrueBetween_MainnetGolden(t *testing.T) {
	rows := mainnetUSDC
	gross, fees, backstop, bcResidual := new(big.Int), new(big.Int), new(big.Int), new(big.Int)
	for i := 1; i < len(rows); i++ {
		p, n := rows[i-1], rows[i]
		a, err := AccrueBetween(p.rd.data(t), n.rd.data(t), fee(t, p.credit, p.carry), fee(t, n.credit, n.carry), 2_000_000)
		if err != nil {
			t.Fatalf("ledger %d tx %s: %v", n.ledger, n.tx, err)
		}
		wantResidual := "0"
		if r, ok := mainnetReported[n.ledger]; ok {
			wantResidual = r
		}
		if !a.BRateMatches || !a.FeesMatch || a.BackstopResidual.String() != wantResidual {
			t.Errorf("ledger %d tx %s: b_rate %s recomputed %s; fees %s/%s expected %s/%s; residual %s want %s",
				n.ledger, n.tx, n.rd.bRate, a.ExpectedBRate, n.credit, n.carry, a.ExpectedFees.Credit,
				a.ExpectedFees.Carry, a.BackstopResidual, wantResidual)
		}
		if a.Gross.Sign() <= 0 || a.ProtocolFee.Sign() <= 0 {
			t.Errorf("ledger %d tx %s: gross %s fee %s, want both positive", n.ledger, n.tx, a.Gross, a.ProtocolFee)
		}
		gross.Add(gross, a.Gross)
		fees.Add(fees, a.ProtocolFee)
		backstop.Add(backstop, a.Backstop)
		bcResidual.Add(bcResidual, a.BackstopResidual)
	}

	// Totals from the first and last chain values alone. d_supply is constant across the slice, so
	// gross telescopes to the difference of the two ceil-rounded liabilities.
	first, last := rows[0], rows[len(rows)-1]
	ceilLiab := func(r rdIn) *big.Int {
		n := new(big.Int).Mul(dec(t, r.dSupply), dec(t, r.dRate))
		n.Add(n, big.NewInt(999_999_999_999))
		return n.Quo(n, big.NewInt(1_000_000_000_000))
	}
	for _, r := range rows {
		if r.rd.dSupply != first.rd.dSupply {
			t.Fatalf("ledger %d: d_supply moved; the telescoping below assumes it does not", r.ledger)
		}
	}
	if want := new(big.Int).Sub(ceilLiab(last.rd), ceilLiab(first.rd)); gross.Cmp(want) != 0 {
		t.Errorf("Σ gross = %s, telescoped %s", gross, want)
	}
	creditDelta := new(big.Int).Sub(dec(t, last.credit), dec(t, first.credit))
	if fees.Cmp(creditDelta) != 0 {
		t.Errorf("Σ fee = %s, stored credit moved %s", fees, creditDelta)
	}
	// carry₀ + Σgross·rate = 1e7·Δcredit + carryₙ: the fee is exactly 0.2% of gross, with no loss.
	lhs := new(big.Int).Mul(gross, big.NewInt(ProtocolInterestFeeRate))
	lhs.Add(lhs, dec(t, first.carry))
	rhs := new(big.Int).Mul(creditDelta, big.NewInt(10_000_000))
	rhs.Add(rhs, dec(t, last.carry))
	if lhs.Cmp(rhs) != 0 {
		t.Errorf("carry₀ + Σgross·rate = %s, 1e7·Δcredit + carryₙ = %s", lhs, rhs)
	}
	bcDelta := new(big.Int).Sub(dec(t, last.rd.bc), dec(t, first.rd.bc))
	if sum := new(big.Int).Add(backstop, bcResidual); sum.Cmp(bcDelta) != 0 {
		t.Errorf("Σ backstop + Σ residual = %s, stored backstop_credit moved %s", sum, bcDelta)
	}
	if gross.String() != "14637944" || fees.String() != "29276" || bcResidual.String() != "-112512682" {
		t.Errorf("totals gross %s fee %s residual %s", gross, fees, bcResidual)
	}
}
