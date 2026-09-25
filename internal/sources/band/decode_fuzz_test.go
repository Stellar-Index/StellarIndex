package band

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Generative runs (seed corpora also run under plain `go test`):
//
//	go test -run=NONE -fuzz=FuzzDecodeRelayArgs$   -fuzztime=60s -parallel=2 ./internal/sources/band/
//	go test -run=NONE -fuzz=FuzzDecodeRelayArgsRaw -fuzztime=60s -parallel=2 ./internal/sources/band/

const fuzzClose = int64(1_780_000_000) // 2026-05-28T20:26:40Z

func b64ScVal(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// rateScVal encodes rate in one of several numeric ScVal shapes. Only
// kind 0 (u64) is Band's wire type; the rest are foreign shapes the
// decoder must refuse rather than coerce.
func rateScVal(kind uint8, rate uint64) xdr.ScVal {
	switch kind % 5 {
	case 1:
		p := xdr.Int128Parts{Lo: xdr.Uint64(rate)}
		return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
	case 2:
		p := xdr.UInt128Parts{Lo: xdr.Uint64(rate)}
		return xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &p}
	case 3:
		v := xdr.Int64(rate)
		return xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &v}
	case 4:
		v := xdr.Uint32(rate)
		return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
	default:
		v := xdr.Uint64(rate)
		return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}
	}
}

func validSymbol(sym string) bool {
	if sym == "" || len(sym) > 32 {
		return false
	}
	for _, c := range sym {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// FuzzDecodeRelayArgs builds a well-formed relay / force_relay call
// with a fuzzed symbol, rate, resolve_time and indices, and checks the
// emitted row against an independently computed expectation:
//   - the price is the exact u64 rate (no int64 wrap above 2^63);
//   - USD and zero rates are never emitted;
//   - inside Band's own acceptance window (floor ≤ resolve_time <
//     close + 3600, strict, as relay() applies it) the timestamp is the
//     declared resolve_time; outside it, relay() is dropped entirely
//     (ErrEmptyRates — the contract would silently no-op it) and
//     force_relay clamps to the ledger close (unconditional admin path);
//   - a rate in any numeric shape other than u64 is refused.
func FuzzDecodeRelayArgs(f *testing.F) {
	f.Add("BTC", uint64(50_000_000_000_000), uint64(fuzzClose), false, uint8(0), uint8(0), true)
	f.Add("EUR", uint64(1<<63), uint64(fuzzClose-60), true, uint8(3), uint8(0), false)
	f.Add("XLM", ^uint64(0), uint64(fuzzClose+3599), false, uint8(99), uint8(0), true)
	f.Add("ETH", uint64(1), uint64(fuzzClose+3600), false, uint8(0), uint8(0), true) // window edge: rejected on-chain
	f.Add("ETH", uint64(1), uint64(fuzzClose+3601), true, uint8(0), uint8(0), true)
	f.Add("USD", uint64(1_000_000_000), uint64(fuzzClose), false, uint8(0), uint8(0), true)
	f.Add("BTC", uint64(0), uint64(fuzzClose), false, uint8(0), uint8(0), true)
	f.Add("BTC", uint64(7), uint64(fuzzClose), false, uint8(0), uint8(1), true)
	f.Add("NOT_LISTED_Q", uint64(7), uint64(999_999_999), false, uint8(0), uint8(2), false)

	f.Fuzz(func(t *testing.T, sym string, rate, resolve uint64, force bool, op, rateKind uint8, opSrc bool) {
		if !validSymbol(sym) {
			t.Skip("not a valid ScSymbol")
		}
		wantAsset, aerr := canonical.MapOracleSymbol(sym)
		closedAt := time.Unix(fuzzClose, 0).UTC()
		s := xdr.ScSymbol(sym)
		pair := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &s}, rateScVal(rateKind, rate)}
		pp := &pair
		outer := xdr.ScVec{{Type: xdr.ScValTypeScvVec, Vec: &pp}}
		po := &outer
		ratesArg := b64ScVal(t, xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &po})

		fn := FnRelay
		args := []string{encodeAddressArg(t, relayerG), ratesArg, encodeU64Arg(t, resolve), encodeU64Arg(t, 1)}
		wantObserver := relayerG
		opSource, txSource := "", "GTXSOURCE"
		if opSrc {
			opSource = "GOPSOURCE"
		}
		if force {
			fn, args = FnForceRelay, args[1:]
			wantObserver = txSource
			if opSrc {
				wantObserver = opSource
			}
		}
		opIdx := int(op % 100)

		updates, err := decodeRelayArgs(fn, args, adapterC, 52_000_000, "fuzz", opIdx, opSource, txSource, closedAt)

		switch {
		case rateKind%5 != 0:
			if !errors.Is(err, ErrMalformedArgs) || updates != nil {
				t.Fatalf("rate shape %d: got (%v, %v), want ErrMalformedArgs", rateKind%5, updates, err)
			}
			return
		case aerr != nil:
			if !errors.Is(err, ErrMalformedArgs) {
				t.Fatalf("unrepresentable symbol %q: got %v, want ErrMalformedArgs", sym, err)
			}
			return
		case sym == "USD" || rate == 0:
			if !errors.Is(err, ErrEmptyRates) || updates != nil {
				t.Fatalf("USD/zero slot: got (%v, %v), want ErrEmptyRates", updates, err)
			}
			return
		case !force && !(resolve >= 1_000_000_000 && resolve < uint64(fuzzClose)+3600):
			// relay() would silently no-op outside its own acceptance
			// window — the decoder must drop it, not clamp-and-write.
			if !errors.Is(err, ErrEmptyRates) || updates != nil {
				t.Fatalf("out-of-window relay resolve_time %d: got (%v, %v), want ErrEmptyRates", resolve, updates, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("well-formed %s rejected: %v", fn, err)
		}
		if len(updates) != 1 {
			t.Fatalf("got %d updates for one slot", len(updates))
		}
		u := updates[0]
		if want := new(big.Int).SetUint64(rate); u.Price.BigInt().Cmp(want) != 0 {
			t.Fatalf("price = %s, want the exact u64 %s (ADR-0003)", u.Price, want)
		}
		if !u.Asset.Equal(wantAsset) || u.Quote.String() != "fiat:USD" || u.Decimals != DefaultDecimals {
			t.Fatalf("row = (%s, %s, %d), want (%s, fiat:USD, %d)", u.Asset, u.Quote, u.Decimals, wantAsset, DefaultDecimals)
		}
		if u.Observer != wantObserver {
			t.Fatalf("observer = %q, want %q", u.Observer, wantObserver)
		}
		if want := uint32(opIdx) * opIndexFanoutStride; u.OpIndex != want {
			t.Fatalf("OpIndex = %d, want %d", u.OpIndex, want)
		}
		wantTs := closedAt
		if resolve >= 1_000_000_000 && resolve < uint64(fuzzClose)+3600 {
			wantTs = time.Unix(int64(resolve), 0).UTC()
		}
		if !u.Timestamp.Equal(wantTs) {
			t.Fatalf("resolve_time %d (close %d): ts = %s, want %s", resolve, fuzzClose, u.Timestamp, wantTs)
		}
	})
}

// FuzzDecodeRelayArgsRaw hands decodeRelayArgs arbitrary XDR in every
// argument slot. It must never panic, and a success must be justified
// by the bytes: every emitted row is a positive price equal to the u64
// in a non-USD symbol_rates slot at the row's own vector position.
func FuzzDecodeRelayArgsRaw(f *testing.F) {
	good := func(sv xdr.ScVal) []byte {
		b, err := sv.MarshalBinary()
		if err != nil {
			f.Fatalf("marshal: %v", err)
		}
		return b
	}
	u := xdr.Uint64(uint64(fuzzClose))
	s := xdr.ScSymbol("BTC")
	r := xdr.Uint64(50_000_000_000_000)
	pair := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &s}, {Type: xdr.ScValTypeScvU64, U64: &r}}
	pp := &pair
	outer := xdr.ScVec{{Type: xdr.ScValTypeScvVec, Vec: &pp}}
	po := &outer
	rates := good(xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &po})
	ts := good(xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u})
	f.Add(false, rates, ts, ts)
	f.Add(true, rates, ts, ts)
	f.Add(false, []byte{}, ts, []byte{0})

	f.Fuzz(func(t *testing.T, force bool, ratesRaw, tsRaw, reqRaw []byte) {
		enc := base64.StdEncoding.EncodeToString
		fn := FnRelay
		args := []string{encodeAddressArg(t, relayerG), enc(ratesRaw), enc(tsRaw), enc(reqRaw)}
		if force {
			fn, args = FnForceRelay, args[1:]
		}
		updates, err := decodeRelayArgs(fn, args, adapterC, 52_000_000, "fuzz", 0, "", "", time.Unix(fuzzClose, 0))
		if err != nil {
			if updates != nil {
				t.Fatalf("error %v returned alongside %d updates", err, len(updates))
			}
			return
		}
		sv, perr := scval.Parse(enc(ratesRaw))
		if perr != nil {
			t.Fatalf("accepted symbol_rates scval.Parse rejects: %v", perr)
		}
		vec, verr := scval.AsVec(sv)
		if verr != nil {
			t.Fatalf("accepted a non-Vec symbol_rates (%s)", sv.Type)
		}
		for _, up := range updates {
			i := int(up.OpIndex % opIndexFanoutStride)
			if i >= len(vec) {
				t.Fatalf("row slot %d beyond %d symbol_rates", i, len(vec))
			}
			elts, _ := scval.AsTupleN(vec[i], 2)
			if len(elts) != 2 || elts[0].Type != xdr.ScValTypeScvSymbol || elts[1].Type != xdr.ScValTypeScvU64 {
				t.Fatalf("slot %d accepted in a non-(Symbol, u64) shape", i)
			}
			if string(*elts[0].Sym) == "USD" {
				t.Fatalf("slot %d emitted a USD row", i)
			}
			want := new(big.Int).SetUint64(uint64(*elts[1].U64))
			if want.Sign() <= 0 || up.Price.BigInt().Cmp(want) != 0 {
				t.Fatalf("slot %d price = %s, want positive %s", i, up.Price, want)
			}
		}
	})
}
