package blend

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Reference arithmetic for the fuzz properties below. It is written
// independently of interest.go (different rounding idiom: (n+d-1)/d
// rather than QuoRem+carry) so a rounding-direction regression in the
// production helpers cannot also hide in the oracle.

func i128FromParts(hi int64, lo uint64) *big.Int {
	n := new(big.Int).Lsh(big.NewInt(hi), 64)
	return n.Add(n, new(big.Int).SetUint64(lo))
}

func u128FromParts(hi, lo uint64) *big.Int {
	n := new(big.Int).Lsh(new(big.Int).SetUint64(hi), 64)
	return n.Add(n, new(big.Int).SetUint64(lo))
}

func refFloor(n, d *big.Int) *big.Int { return new(big.Int).Div(n, d) }

func refCeil(n, d *big.Int) *big.Int {
	num := new(big.Int).Add(n, d)
	num.Sub(num, big.NewInt(1))
	return num.Div(num, d)
}

func mul(a, b *big.Int) *big.Int { return new(big.Int).Mul(a, b) }

func bi(n int64) *big.Int { return big.NewInt(n) }

// refBorrowRate transcribes interest.rs::calc_accrual's cur_ir branch.
func refBorrowRate(rc ReserveConfig, u, irMod *big.Int) *big.Int {
	s7 := bi(10_000_000)
	t := bi(int64(rc.Util))
	r0, r1, r2, r3 := bi(int64(rc.RBase)), bi(int64(rc.ROne)), bi(int64(rc.RTwo)), bi(int64(rc.RThree))
	u95, u05 := bi(9_500_000), bi(500_000)
	switch {
	case u.Cmp(t) <= 0:
		us := bi(0)
		if t.Sign() > 0 {
			us = refCeil(mul(u, s7), t)
		}
		base := new(big.Int).Add(refCeil(mul(us, r1), s7), r0)
		return refCeil(mul(base, irMod), s7)
	case u.Cmp(u95) <= 0:
		us := refCeil(mul(new(big.Int).Sub(u, t), s7), new(big.Int).Sub(u95, t))
		base := new(big.Int).Add(refCeil(mul(us, r2), s7), r1)
		base.Add(base, r0)
		return refCeil(mul(base, irMod), s7)
	default:
		us := refCeil(mul(new(big.Int).Sub(u, u95), s7), u05)
		sum := new(big.Int).Add(r2, r1)
		sum.Add(sum, r0)
		return new(big.Int).Add(refCeil(mul(us, r3), s7), refCeil(mul(irMod, sum), s7))
	}
}

// FuzzFixedPointRounding pins the soroban-fixed-point-math rounding
// directions (floor vs ceil) against an independent big.Int oracle, on
// operands wider than 64 bits, and that no helper mutates its inputs.
func FuzzFixedPointRounding(f *testing.F) {
	f.Add(uint64(0), uint64(7), uint64(3), uint8(0))
	f.Add(uint64(0), uint64(10_000_000), uint64(10_000_000), uint8(0))
	f.Add(uint64(1), uint64(0), uint64(1_000_000_000_001), uint8(1))
	f.Add(^uint64(0), ^uint64(0), ^uint64(0), uint8(1))
	f.Add(uint64(0), uint64(0), uint64(1), uint8(0))
	f.Fuzz(func(t *testing.T, aHi, aLo, bRaw uint64, sel uint8) {
		a := u128FromParts(aHi, aLo)
		b := new(big.Int).SetUint64(bRaw)
		if b.Sign() == 0 {
			b.SetInt64(1)
		}
		scalar := scalar7
		if sel%2 == 1 {
			scalar = scalar12
		}
		aCopy, bCopy, sCopy := new(big.Int).Set(a), new(big.Int).Set(b), new(big.Int).Set(scalar)

		floor := fixedMulFloor(a, b, scalar)
		ceil := fixedMulCeil(a, b, scalar)
		divCeil := fixedDivCeil(a, b, scalar)

		if want := refFloor(mul(a, b), scalar); floor.Cmp(want) != 0 {
			t.Fatalf("fixedMulFloor(%s,%s,%s)=%s want %s", a, b, scalar, floor, want)
		}
		if want := refCeil(mul(a, b), scalar); ceil.Cmp(want) != 0 {
			t.Fatalf("fixedMulCeil(%s,%s,%s)=%s want %s", a, b, scalar, ceil, want)
		}
		if want := refCeil(mul(a, scalar), b); divCeil.Cmp(want) != 0 {
			t.Fatalf("fixedDivCeil(%s,%s,%s)=%s want %s", a, b, scalar, divCeil, want)
		}
		exact := new(big.Int).Rem(mul(a, b), scalar).Sign() == 0
		gap := new(big.Int).Sub(ceil, floor)
		if exact && gap.Sign() != 0 || !exact && gap.Cmp(bi(1)) != 0 {
			t.Fatalf("ceil-floor=%s for exact=%v", gap, exact)
		}
		if a.Cmp(aCopy) != 0 || b.Cmp(bCopy) != 0 || scalar.Cmp(sCopy) != 0 {
			t.Fatalf("fixed-point helper mutated an input: a=%s b=%s scalar=%s", a, b, scalar)
		}
	})
}

// FuzzReserveMetrics checks supplied/borrowed/utilization/borrow-rate/
// supply-rate against the contract-transcribed oracle, plus two
// oracle-free properties: the borrow rate is monotone non-decreasing in
// utilization, and matches its closed form at the curve's kinks.
func FuzzReserveMetrics(f *testing.F) {
	// Reference config from interest.rs, non-exact rounding everywhere.
	f.Add(uint64(2_000_000_0000001), uint64(1_000_000_0000003), uint64(1_000_000_000_007), uint64(1_000_000_000_011),
		uint32(1_0000003), uint32(7_500_000), uint32(100_000), uint32(500_000), uint32(5_000_000), uint32(15_000_000),
		uint32(1_000_000), uint32(123_457))
	// No supply and no debt: utilization is 0, not the >= cap.
	f.Add(uint64(0), uint64(0), uint64(1_000_000_000_000), uint64(1_000_000_000_000),
		uint32(1_0000000), uint32(7_500_000), uint32(100_000), uint32(500_000), uint32(5_000_000), uint32(15_000_000),
		uint32(1_000_000), uint32(0))
	// Backstop take above 100%: supply rate clamps at 0, never negative.
	f.Add(uint64(3_000_000_000), uint64(2_000_000_001), uint64(1_100_000_000_003), uint64(1_200_000_000_007),
		uint32(1_2345671), uint32(6_000_001), uint32(300_001), uint32(700_003), uint32(4_000_007), uint32(9_000_011),
		uint32(12_000_000), uint32(999))
	// Over-95% regime.
	f.Add(uint64(1_000_000), uint64(990_000), uint64(1_000_000_000_000), uint64(1_000_000_000_000),
		uint32(1_0000000), uint32(5_000_000), uint32(1), uint32(2), uint32(3), uint32(4),
		uint32(0), uint32(4_999_999))
	f.Fuzz(func(t *testing.T, bSupply, dSupply, bRate, dRate uint64, irMod, target, r0, r1, r2, r3, bstop, probe uint32) {
		rd := ReserveData{
			BSupply: new(big.Int).SetUint64(bSupply), BRate: new(big.Int).SetUint64(bRate),
			DSupply: new(big.Int).SetUint64(dSupply), DRate: new(big.Int).SetUint64(dRate),
			IRMod: bi(int64(irMod)),
		}
		// The contract enforces 0 < util < 0.95 at reserve setup.
		rc := ReserveConfig{Util: 1 + target%9_499_999, RBase: r0, ROne: r1, RTwo: r2, RThree: r3}
		s7, s12 := bi(10_000_000), bi(1_000_000_000_000)

		sup := refFloor(mul(rd.BSupply, rd.BRate), s12)
		bor := refCeil(mul(rd.DSupply, rd.DRate), s12)
		if got := rd.SuppliedUnderlying(); got.Cmp(sup) != 0 {
			t.Fatalf("SuppliedUnderlying=%s want floor %s", got, sup)
		}
		if got := rd.BorrowedUnderlying(); got.Cmp(bor) != 0 {
			t.Fatalf("BorrowedUnderlying=%s want ceil %s", got, bor)
		}
		var wantUtil *big.Int
		switch {
		case bor.Sign() == 0:
			wantUtil = bi(0)
		case bor.Cmp(sup) >= 0:
			wantUtil = new(big.Int).Set(s7)
		default:
			wantUtil = refCeil(mul(bor, s7), sup)
		}
		util := rd.Utilization()
		if util.Cmp(wantUtil) != 0 {
			t.Fatalf("Utilization=%s want %s (sup=%s bor=%s)", util, wantUtil, sup, bor)
		}
		if util.Sign() < 0 || util.Cmp(s7) > 0 {
			t.Fatalf("Utilization=%s outside [0, 1e7]", util)
		}

		br := rc.BorrowRate(util, rd.IRMod)
		if want := refBorrowRate(rc, util, rd.IRMod); br.Cmp(want) != 0 {
			t.Fatalf("BorrowRate(util=%s)=%s want %s", util, br, want)
		}

		// Kinks: the rate at 0, at target and at 95% has a closed form.
		sumTo := func(xs ...uint32) *big.Int {
			s := bi(0)
			for _, x := range xs {
				s.Add(s, bi(int64(x)))
			}
			return refCeil(mul(s, rd.IRMod), s7)
		}
		for _, k := range []struct {
			u    *big.Int
			want *big.Int
		}{
			{bi(0), sumTo(r0)},
			{bi(int64(rc.Util)), sumTo(r0, r1)},
			{bi(9_500_000), sumTo(r0, r1, r2)},
		} {
			if got := rc.BorrowRate(k.u, rd.IRMod); got.Cmp(k.want) != 0 {
				t.Fatalf("BorrowRate at kink util=%s = %s want %s", k.u, got, k.want)
			}
		}

		// Monotone non-decreasing in utilization across the whole curve.
		lo := bi(int64(probe % 10_000_001))
		hi := new(big.Int).Add(lo, bi(int64(probe%977+1)))
		if hi.Cmp(s7) > 0 {
			hi.Set(s7)
		}
		if a, b := rc.BorrowRate(lo, rd.IRMod), rc.BorrowRate(hi, rd.IRMod); a.Cmp(b) > 0 {
			t.Fatalf("BorrowRate not monotone: rate(%s)=%s > rate(%s)=%s", lo, a, hi, b)
		}

		sr := SupplyRate(br, util, bstop)
		keep := new(big.Int).Sub(s7, bi(int64(bstop)))
		if keep.Sign() < 0 {
			keep.SetInt64(0)
		}
		wantSR := refFloor(mul(refFloor(mul(br, util), s7), keep), s7)
		if sr.Cmp(wantSR) != 0 {
			t.Fatalf("SupplyRate(br=%s util=%s bstop=%d)=%s want %s", br, util, bstop, sr, wantSR)
		}
		if sr.Sign() < 0 || sr.Cmp(br) > 0 {
			t.Fatalf("SupplyRate=%s outside [0, borrow rate %s]", sr, br)
		}

		m := Metrics(rd, rc, bstop)
		if !m.HasAPR || m.SuppliedUnderlying.Cmp(sup) != 0 || m.BorrowedUnderlying.Cmp(bor) != 0 {
			t.Fatalf("Metrics disagrees with its parts: %+v", m)
		}
		if m.UtilizationPct < 0 || m.UtilizationPct > 100 || m.SupplyAPR < 0 {
			t.Fatalf("Metrics out of range: util%%=%v supplyAPR=%v", m.UtilizationPct, m.SupplyAPR)
		}
	})
}

// reserveConfigEntries builds a ReserveConfig ScMap in soroban-sdk's
// sorted-key order.
func reserveConfigEntries(t *testing.T, u [11]uint32, supplyCap *big.Int, enabled bool) []xdr.ScMapEntry {
	t.Helper()
	return []xdr.ScMapEntry{
		{Key: symbolScVal("c_factor"), Val: u32ScVal(u[2])},
		{Key: symbolScVal("decimals"), Val: u32ScVal(u[1])},
		{Key: symbolScVal("enabled"), Val: boolScVal(enabled)},
		{Key: symbolScVal("index"), Val: u32ScVal(u[0])},
		{Key: symbolScVal("l_factor"), Val: u32ScVal(u[3])},
		{Key: symbolScVal("max_util"), Val: u32ScVal(u[5])},
		{Key: symbolScVal("r_base"), Val: u32ScVal(u[6])},
		{Key: symbolScVal("r_one"), Val: u32ScVal(u[7])},
		{Key: symbolScVal("r_three"), Val: u32ScVal(u[9])},
		{Key: symbolScVal("r_two"), Val: u32ScVal(u[8])},
		{Key: symbolScVal("reactivity"), Val: u32ScVal(u[10])},
		{Key: symbolScVal("supply_cap"), Val: i128ScVal(t, supplyCap)},
		{Key: symbolScVal("util"), Val: u32ScVal(u[4])},
	}
}

func equalReserveConfig(a, b ReserveConfig) bool {
	if a.SupplyCap == nil || b.SupplyCap == nil || a.SupplyCap.Cmp(b.SupplyCap) != 0 {
		return false
	}
	a.SupplyCap, b.SupplyCap = nil, nil
	return a == b
}

// FuzzReserveConfigRoundTrip closes the writer→reader loop the APY
// path depends on: a queue_set_reserve ReserveConfig decoded by the
// EVENT decoder, persisted as blend_admin.attributes->'metadata' JSON
// and re-read by ParseReserveConfigMetadata must equal the STORAGE
// decoder's view of the same ScVal — every field, the i128 supply cap
// without truncation, regardless of map key order.
func FuzzReserveConfigRoundTrip(f *testing.F) {
	f.Add(uint32(3), uint32(7), uint32(8_500_000), uint32(9_000_000), uint32(8_000_000), uint32(9_500_000),
		uint32(100_000), uint32(500_000), uint32(1_000_000), uint32(2_000_000), uint32(50_000),
		int64(0), uint64(1_000_000_000_000), true, false)
	f.Add(uint32(0), uint32(18), uint32(1), uint32(2), uint32(3), uint32(4), uint32(5), uint32(6), uint32(7), uint32(8),
		uint32(9), int64(1<<40), uint64(0xdeadbeefcafebabe), false, true)
	f.Add(^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), uint32(11), uint32(12),
		^uint32(0), int64(-1<<63), uint64(0), true, true)
	f.Fuzz(func(t *testing.T, idx, dec, cf, lf, util, maxUtil, r0, r1, r2, r3, react uint32,
		capHi int64, capLo uint64, enabled, reverse bool,
	) {
		fields := [11]uint32{idx, dec, cf, lf, util, maxUtil, r0, r1, r2, r3, react}
		supplyCap := i128FromParts(capHi, capLo)
		entries := reserveConfigEntries(t, fields, supplyCap, enabled)
		if reverse {
			slices.Reverse(entries)
		}
		sv := mapScVal(entries)

		want := ReserveConfig{
			Index: idx, Decimals: dec, CFactor: cf, LFactor: lf, Util: util, MaxUtil: maxUtil,
			RBase: r0, ROne: r1, RTwo: r2, RThree: r3, Reactivity: react,
			SupplyCap: supplyCap, Enabled: enabled,
		}
		fromStorage, err := DecodeReserveConfig(sv)
		if err != nil {
			t.Fatalf("DecodeReserveConfig: %v", err)
		}
		if !equalReserveConfig(fromStorage, want) {
			t.Fatalf("DecodeReserveConfig=%+v want %+v", fromStorage, want)
		}

		meta, err := decodeReserveConfig(sv)
		if err != nil {
			t.Fatalf("decodeReserveConfig: %v", err)
		}
		// Mirror blend_admin's writer: attrs["metadata"] = cfg, stored
		// as jsonb, read back via attributes->'metadata'.
		attrs, err := json.Marshal(map[string]any{"metadata": meta})
		if err != nil {
			t.Fatalf("marshal attrs: %v", err)
		}
		var stored map[string]json.RawMessage
		if err := json.Unmarshal(attrs, &stored); err != nil {
			t.Fatalf("unmarshal attrs: %v", err)
		}
		fromEvent, err := ParseReserveConfigMetadata(stored["metadata"])
		if err != nil {
			t.Fatalf("ParseReserveConfigMetadata: %v", err)
		}
		if !equalReserveConfig(fromEvent, want) {
			t.Fatalf("event-path ReserveConfig=%+v want %+v (json %s)", fromEvent, want, stored["metadata"])
		}
	})
}

// i128sFromBytes carves n signed 128-bit values out of raw (zero-padded).
func i128sFromBytes(raw []byte, n int) []*big.Int {
	buf := make([]byte, 16*n)
	copy(buf, raw)
	out := make([]*big.Int, n)
	for i := range out {
		hi := int64(binary.BigEndian.Uint64(buf[16*i:])) //nolint:gosec // deliberate two's-complement reinterpretation
		lo := binary.BigEndian.Uint64(buf[16*i+8:])
		out[i] = i128FromParts(hi, lo)
	}
	return out
}

// FuzzDecodeReserveData: every i128 field decodes to exactly its wire
// value (full 128 bits, sign included), decoding is by field NAME
// (order-independent), and a missing field is an error, not a zero.
func FuzzDecodeReserveData(f *testing.F) {
	seed := make([]byte, 96)
	for i := range seed {
		seed[i] = byte(i*37 + 11)
	}
	f.Add(seed, uint64(1_700_000_000), false, uint8(0))
	f.Add([]byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, uint64(0), true, uint8(3))
	f.Add([]byte{}, ^uint64(0), false, uint8(6))
	f.Fuzz(func(t *testing.T, raw []byte, lastTime uint64, reverse bool, drop uint8) {
		v := i128sFromBytes(raw, 6)
		names := []string{"b_rate", "b_supply", "backstop_credit", "d_rate", "d_supply", "ir_mod"}
		entries := make([]xdr.ScMapEntry, 0, 7)
		for i, n := range names {
			entries = append(entries, xdr.ScMapEntry{Key: symbolScVal(n), Val: i128ScVal(t, v[i])})
		}
		entries = append(entries, xdr.ScMapEntry{Key: symbolScVal("last_time"), Val: u64ScVal(lastTime)})
		if reverse {
			slices.Reverse(entries)
		}
		rd, err := DecodeReserveData(mapScVal(entries))
		if err != nil {
			t.Fatalf("DecodeReserveData: %v", err)
		}
		got := []*big.Int{rd.BRate, rd.BSupply, rd.BackstopCredit, rd.DRate, rd.DSupply, rd.IRMod}
		for i := range got {
			if got[i].Cmp(v[i]) != 0 {
				t.Fatalf("%s=%s want %s", names[i], got[i], v[i])
			}
		}
		if rd.LastTime != lastTime {
			t.Fatalf("last_time=%d want %d", rd.LastTime, lastTime)
		}

		missing := slices.Delete(slices.Clone(entries), int(drop)%len(entries), int(drop)%len(entries)+1)
		if _, err := DecodeReserveData(mapScVal(missing)); err == nil {
			t.Fatalf("DecodeReserveData accepted a map missing a field (dropped index %d)", int(drop)%len(entries))
		}
	})
}

// FuzzDecodeAuctionData: bid/lot amounts survive at full i128 width and
// in wire order, bid and lot are not crossed, and a non-contract
// (account) asset key is rejected rather than mis-keyed.
func FuzzDecodeAuctionData(f *testing.F) {
	big1 := make([]byte, 64)
	for i := range big1 {
		big1[i] = byte(0xA5 ^ i)
	}
	f.Add(big1, uint8(2), uint8(2), uint32(51_000_000), false)
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 1}, uint8(1), uint8(0), uint32(0), true)
	f.Add([]byte{}, uint8(0), uint8(3), ^uint32(0), false)
	f.Fuzz(func(t *testing.T, raw []byte, nBid, nLot uint8, block uint32, accountKey bool) {
		nb, nl := int(nBid%4), int(nLot%4)
		amts := i128sFromBytes(raw, nb+nl)
		build := func(off, n int, tagBase byte) (xdr.ScVal, []AssetAmount) {
			entries := make([]xdr.ScMapEntry, n)
			want := make([]AssetAmount, n)
			for i := range n {
				asset := contractStrkeyFromSeed(t, tagBase+byte(i))
				entries[i] = xdr.ScMapEntry{Key: addressScVal(t, asset), Val: i128ScVal(t, amts[off+i])}
				a, err := canonical.NewSorobanAsset(asset)
				if err != nil {
					t.Fatalf("NewSorobanAsset: %v", err)
				}
				want[i] = AssetAmount{Asset: a, Amount: amts[off+i]}
			}
			return mapScVal(entries), want
		}
		bidSV, wantBid := build(0, nb, 0x10)
		lotSV, wantLot := build(nb, nl, 0x40)

		got, err := decodeAuctionData(auctionDataScVal(bidSV, lotSV, block))
		if err != nil {
			t.Fatalf("decodeAuctionData: %v", err)
		}
		check := func(side string, got, want []AssetAmount) {
			if len(got) != len(want) {
				t.Fatalf("%s: %d entries want %d", side, len(got), len(want))
			}
			for i := range got {
				if got[i].Asset.String() != want[i].Asset.String() || got[i].Amount.Cmp(want[i].Amount) != 0 {
					t.Fatalf("%s[%d]=(%s,%s) want (%s,%s)", side, i, got[i].Asset, got[i].Amount, want[i].Asset, want[i].Amount)
				}
			}
		}
		check("bid", got.Bid, wantBid)
		check("lot", got.Lot, wantLot)
		if got.Block != block {
			t.Fatalf("block=%d want %d", got.Block, block)
		}

		if accountKey {
			bad := mapScVal([]xdr.ScMapEntry{{
				Key: addressScVal(t, accountStrkeyFromSeed(t, 0x33)),
				Val: i128ScVal(t, bi(1)),
			}})
			if _, err := decodeAuctionData(auctionDataScVal(bad, lotSV, block)); err == nil {
				t.Fatal("decodeAuctionData accepted a G-address asset key")
			}
		}
	})
}

// FuzzDecodePositionEvent: token and b/d-token amounts are carried at
// full i128 width in the right slots, and topic arity / body arity
// drift is rejected as ErrMalformedPayload rather than mis-decoded.
func FuzzDecodePositionEvent(f *testing.F) {
	f.Add(uint8(0), int64(0), uint64(1_000_000_000), int64(0), uint64(990_000_000))
	f.Add(uint8(6), int64(1<<62), uint64(7), int64(-1), uint64(1))
	f.Add(uint8(4), int64(-1<<63), uint64(0), int64(1<<62-1), ^uint64(0))
	kinds := []struct{ topic, kind string }{
		{TopicSymbolSupply, EventSupply},
		{TopicSymbolWithdraw, EventWithdraw},
		{TopicSymbolSupplyCollateral, EventSupplyCollateral},
		{TopicSymbolWithdrawCollateral, EventWithdrawCollateral},
		{TopicSymbolBorrow, EventBorrow},
		{TopicSymbolRepay, EventRepay},
		{TopicSymbolFlashLoan, EventFlashLoan},
	}
	f.Fuzz(func(t *testing.T, sel uint8, tHi int64, tLo uint64, bHi int64, bLo uint64) {
		k := kinds[int(sel)%len(kinds)]
		tokenAmt, bdAmt := i128FromParts(tHi, tLo), i128FromParts(bHi, bLo)
		topics := []string{
			k.topic,
			encodeScVal(t, addressScVal(t, contractStrkeyFromSeed(t, 0x20))),
			encodeScVal(t, addressScVal(t, accountStrkeyFromSeed(t, 0x21))),
		}
		counterparty := contractStrkeyFromSeed(t, 0x22)
		if k.kind == EventFlashLoan {
			topics = append(topics, encodeScVal(t, addressScVal(t, counterparty)))
		}
		ev := &events.Event{
			ContractID: contractStrkeyFromSeed(t, 0x10),
			Topic:      topics,
			Value:      encodeScVal(t, vecScVal(i128ScVal(t, tokenAmt), i128ScVal(t, bdAmt))),
		}
		out, err := decodePositionEvent(ev, k.kind, time.Unix(0, 0))
		if err != nil {
			t.Fatalf("decodePositionEvent(%s): %v", k.kind, err)
		}
		if out.TokenAmount.Cmp(tokenAmt) != 0 || out.BOrDAmount.Cmp(bdAmt) != 0 {
			t.Fatalf("%s amounts=(%s,%s) want (%s,%s)", k.kind, out.TokenAmount, out.BOrDAmount, tokenAmt, bdAmt)
		}
		if (k.kind == EventFlashLoan) != (out.Counterparty == counterparty) {
			t.Fatalf("%s counterparty=%q", k.kind, out.Counterparty)
		}

		// Body arity drift: a third element is a new WASM, not a supply.
		long := *ev
		long.Value = encodeScVal(t, vecScVal(i128ScVal(t, tokenAmt), i128ScVal(t, bdAmt), i128ScVal(t, bi(1))))
		if _, err := decodePositionEvent(&long, k.kind, time.Unix(0, 0)); !errors.Is(err, ErrMalformedPayload) {
			t.Fatalf("%s 3-element body: err=%v want ErrMalformedPayload", k.kind, err)
		}
		// Topic arity drift: drop the last topic.
		short := *ev
		short.Topic = topics[:len(topics)-1]
		if _, err := decodePositionEvent(&short, k.kind, time.Unix(0, 0)); !errors.Is(err, ErrMalformedPayload) {
			t.Fatalf("%s with %d topics: err=%v want ErrMalformedPayload", k.kind, len(short.Topic), err)
		}
	})
}

// FuzzDecoderDecodeBody feeds arbitrary body bytes behind well-formed
// topics for the amount-bearing kinds. The decoder must never panic,
// and whatever it rejects must be a classified error.
func FuzzDecoderDecodeBody(f *testing.F) {
	// The fixture helpers only Fatal on invalid constant input, so a
	// bare *testing.T is safe for building the fixed topics and seeds.
	seedT := &testing.T{}
	addrTopic := func(tag byte, contract bool) string {
		t := seedT
		if contract {
			return encodeScVal(t, addressScVal(t, contractStrkeyFromSeed(t, tag)))
		}
		return encodeScVal(t, addressScVal(t, accountStrkeyFromSeed(t, tag)))
	}
	auctionType := func() string {
		b, _ := u32ScVal(AuctionTypeUserLiquidation).MarshalBinary()
		return base64.StdEncoding.EncodeToString(b)
	}()
	topicSets := [][]string{
		{TopicSymbolSupply, addrTopic(0x20, true), addrTopic(0x21, false)},
		{TopicSymbolFlashLoan, addrTopic(0x20, true), addrTopic(0x21, false), addrTopic(0x22, true)},
		{TopicSymbolNewAuction, auctionType, addrTopic(0x21, false)},
		{TopicSymbolFillAuction, auctionType, addrTopic(0x21, false)},
		{TopicSymbolQueueSetReserve, addrTopic(0x23, false)},
		{TopicSymbolClaim, addrTopic(0x21, false)},
		{TopicSymbolGulp, addrTopic(0x20, true)},
	}
	pos, _ := vecScVal(i128ScVal(seedT, bi(5)), i128ScVal(seedT, bi(6))).MarshalBinary()
	f.Add(uint8(0), pos)
	f.Add(uint8(1), pos)
	cfg, _ := vecScVal(addressScVal(seedT, contractStrkeyFromSeed(seedT, 0x20)), reserveConfigScVal(seedT)).MarshalBinary()
	f.Add(uint8(4), cfg)
	amt, _ := i128ScVal(seedT, bi(-7)).MarshalBinary()
	f.Add(uint8(6), amt)
	f.Add(uint8(2), []byte{0, 0, 0, 16, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, sel uint8, body []byte) {
		ev := events.Event{
			ContractID:     contractStrkeyFromSeed(t, 0x10),
			Topic:          topicSets[int(sel)%len(topicSets)],
			Value:          base64.StdEncoding.EncodeToString(body),
			LedgerClosedAt: "2026-05-20T10:00:00Z",
		}
		out, err := NewDecoder().Decode(ev)
		if err != nil {
			if !errors.Is(err, ErrMalformedPayload) && !errors.Is(err, ErrUnknownAuctionType) {
				t.Fatalf("unclassified decode error: %v", err)
			}
			return
		}
		if len(out) != 1 {
			t.Fatalf("Decode returned %d events, want 1", len(out))
		}
	})
}
