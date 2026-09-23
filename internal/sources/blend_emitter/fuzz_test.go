package blend_emitter

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

func i128SV(n *big.Int) xdr.ScVal {
	u := new(big.Int).Set(n)
	if u.Sign() < 0 {
		u.Add(u, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	lo := new(big.Int).And(u, new(big.Int).SetUint64(^uint64(0))).Uint64()
	hi := new(big.Int).Rsh(u, 64).Uint64()
	p := xdr.Int128Parts{Hi: xdr.Int64(int64(hi)), Lo: xdr.Uint64(lo)} //nolint:gosec // two's-complement reinterpretation
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func vecSV(vals ...xdr.ScVal) xdr.ScVal {
	v := xdr.ScVec(vals)
	pv := &v
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
}

func contractAddrSV(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, strk)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func b64SV(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// i128sFromBytes carves n signed 128-bit values out of raw (zero-padded).
func i128sFromBytes(raw []byte, n int) []*big.Int {
	buf := make([]byte, 16*n)
	copy(buf, raw)
	out := make([]*big.Int, n)
	for i := range out {
		v := new(big.Int).SetBytes(buf[16*i : 16*i+16])
		if buf[16*i]&0x80 != 0 {
			v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 128))
		}
		out[i] = v
	}
	return out
}

func emitterEvent(t *testing.T, topic0 string, body xdr.ScVal) events.Event {
	t.Helper()
	return events.Event{
		ContractID:     MainnetEmitter,
		Topic:          []string{topic0},
		Value:          b64SV(t, body),
		LedgerClosedAt: dropFixtureClosedAt,
	}
}

// FuzzDecodeDrop: every recipient's amount survives at full i128 width,
// in wire order; a zero or negative amount anywhere rejects the whole
// event (ErrNonPositiveAmount); an inner element that is not exactly a
// (recipient, amount) pair is malformed rather than truncated.
func FuzzDecodeDrop(f *testing.F) {
	f.Add([]byte{}, uint8(1), false) // a zero amount
	pos := make([]byte, 48)
	for i := range 3 {
		binary.BigEndian.PutUint64(pos[16*i+8:], uint64(10_000_000_000_000*(i+1)))
	}
	f.Add(pos, uint8(3), false)
	f.Add(pos, uint8(2), true) // trailing extra element on each pair
	f.Add([]byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, uint8(1), false)
	f.Add([]byte{0x80}, uint8(1), false) // i128 min
	f.Fuzz(func(t *testing.T, raw []byte, n uint8, extra bool) {
		count := 1 + int(n%5)
		amts := i128sFromBytes(raw, count)
		pairs := make([]xdr.ScVal, count)
		recips := make([]string, count)
		for i := range pairs {
			recips[i] = contractStrkeyFromSeed(t, byte(0x60+i))
			elems := []xdr.ScVal{contractAddrSV(t, recips[i]), i128SV(amts[i])}
			if extra {
				elems = append(elems, i128SV(big.NewInt(1)))
			}
			pairs[i] = vecSV(elems...)
		}
		ev := emitterEvent(t, dropFixtureTopic0, vecSV(pairs...))
		got, err := decodeDrop(&ev)
		if extra {
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("3-element recipient pair: err=%v want ErrMalformedPayload", err)
			}
			return
		}
		allPositive := true
		for _, a := range amts {
			allPositive = allPositive && a.Sign() > 0
		}
		if !allPositive {
			if !errors.Is(err, ErrNonPositiveAmount) {
				t.Fatalf("amounts %v: err=%v want ErrNonPositiveAmount", amts, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("decodeDrop: %v", err)
		}
		if len(got) != count {
			t.Fatalf("%d recipients want %d", len(got), count)
		}
		for i, r := range got {
			if r.Recipient != recips[i] || r.Amount.BigInt().Cmp(amts[i]) != 0 {
				t.Fatalf("recipient[%d]=(%s,%s) want (%s,%s)", i, r.Recipient, r.Amount, recips[i], amts[i])
			}
		}
	})
}

// FuzzDecodeDistribute: the amount is carried at full i128 width, a
// non-positive amount is rejected, and a body that is not exactly
// (backstop, amount) is malformed.
func FuzzDecodeDistribute(f *testing.F) {
	f.Add(int64(0), uint64(1_492_660_000_000), false)
	f.Add(int64(0), uint64(0), false)
	f.Add(int64(-1), ^uint64(0), false)
	f.Add(int64(1<<62), uint64(3), false)
	f.Add(int64(0), uint64(5), true)
	f.Fuzz(func(t *testing.T, hi int64, lo uint64, extra bool) {
		amt := new(big.Int).Lsh(big.NewInt(hi), 64)
		amt.Add(amt, new(big.Int).SetUint64(lo))
		backstop := contractStrkeyFromSeed(t, 0x70)
		elems := []xdr.ScVal{contractAddrSV(t, backstop), i128SV(amt)}
		if extra {
			elems = append(elems, i128SV(big.NewInt(1)))
		}
		ev := emitterEvent(t, distributeFixtureTopic0, vecSV(elems...))
		got, err := decodeDistribute(&ev)
		switch {
		case extra:
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("3-element body: err=%v want ErrMalformedPayload", err)
			}
		case amt.Sign() <= 0:
			if !errors.Is(err, ErrNonPositiveAmount) {
				t.Fatalf("amount %s: err=%v want ErrNonPositiveAmount", amt, err)
			}
		default:
			if err != nil {
				t.Fatalf("decodeDistribute: %v", err)
			}
			if got.BackstopID != backstop || got.Amount.BigInt().Cmp(amt) != 0 {
				t.Fatalf("got (%s,%s) want (%s,%s)", got.BackstopID, got.Amount, backstop, amt)
			}
		}
	})
}

// FuzzEmitterDecodeBody feeds arbitrary body bytes under each Emitter
// topic. The decoder must never panic and every rejection must carry
// one of the package's sentinels.
func FuzzEmitterDecodeBody(f *testing.F) {
	for i, body := range []string{distributeFixtureBody, dropFixtureBody, qSwapFixtureBody, swapFixtureBody} {
		raw, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			f.Fatalf("fixture %d: %v", i, err)
		}
		f.Add(uint8(i), raw)
	}
	topics := []string{distributeFixtureTopic0, dropFixtureTopic0, qSwapFixtureTopic0, swapFixtureTopic0}
	f.Fuzz(func(t *testing.T, sel uint8, body []byte) {
		ev := events.Event{
			ContractID:     MainnetEmitter,
			Topic:          []string{topics[int(sel)%len(topics)]},
			Value:          base64.StdEncoding.EncodeToString(body),
			LedgerClosedAt: dropFixtureClosedAt,
		}
		out, err := NewDecoder().Decode(ev)
		if err != nil {
			if !errors.Is(err, ErrMalformedPayload) && !errors.Is(err, ErrNonPositiveAmount) && !errors.Is(err, ErrEmptyRecipients) {
				t.Fatalf("unclassified decode error: %v", err)
			}
			return
		}
		if len(out) != 1 {
			t.Fatalf("Decode returned %d events, want 1", len(out))
		}
	})
}
