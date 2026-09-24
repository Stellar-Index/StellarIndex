package reflector

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Generative runs (seed corpora also run under plain `go test`):
//
//	go test -run=^$ -fuzz=^FuzzDecodeUpdate$          -fuzztime=60s -parallel=2 ./internal/sources/reflector/
//	go test -run=^$ -fuzz=^FuzzSdkDecodeUpdateBodyRaw$ -fuzztime=60s -parallel=2 ./internal/sources/reflector/

// ref128 is the reference i128 value of (hi, lo): hi·2^64 + lo in exact
// big.Int arithmetic, independent of canonical.FromInt128Parts.
func ref128(hi int64, lo uint64) *big.Int {
	v := new(big.Int).Lsh(big.NewInt(hi), 64)
	return v.Add(v, new(big.Int).SetUint64(lo))
}

func fuzzSymbolAsset(sym string) (xdr.ScVal, bool) {
	if sym == "" || len(sym) > 32 {
		return xdr.ScVal{}, false
	}
	for _, c := range sym {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return xdr.ScVal{}, false
		}
	}
	s := xdr.ScSymbol(sym)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &s}, true
}

// FuzzDecodeUpdate drives the full decodeUpdate path with a
// well-formed single-slot event whose price, asset, timestamp and
// indices are fuzzed, and checks the emitted row against an
// independently computed expectation.
func FuzzDecodeUpdate(f *testing.F) {
	const close2026 = int64(1_776_000_000) // 2026-04-12, seconds
	f.Add(int64(0), uint64(20_000_000_000_000), "BTC", false, uint64(1_776_000_000_000), uint8(0), uint8(0), uint8(1))
	f.Add(int64(1), uint64(0), "", true, uint64(0), uint8(99), uint8(63), uint8(2))                      // 2^64: above int64
	f.Add(int64(-1), uint64(0), "USD", false, uint64(1<<63), uint8(3), uint8(1), uint8(3))               // negative
	f.Add(int64(0), uint64(0), "EUR", false, uint64(1_776_000_000_000), uint8(0), uint8(0), uint8(3))    // zero
	f.Add(int64(0x7fffffffffffffff), ^uint64(0), "XAU", false, ^uint64(0), uint8(1), uint8(2), uint8(3)) // i128 max
	f.Add(int64(0), uint64(1), "NOT_A_TICKER_ZZ", false, uint64(1_776_086_400_001), uint8(0), uint8(0), uint8(2))

	f.Fuzz(func(t *testing.T, hi int64, lo uint64, sym string, useAddr bool, tsMs uint64, op, ev, variantByte uint8) {
		var assetSv xdr.ScVal
		var wantAsset canonical.Asset
		if useAddr {
			assetSv = xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: ptrScAddr(zeroContractAddress(t))}
			a, err := canonical.NewSorobanAsset(zeroContractStrkey(t))
			if err != nil {
				t.Fatalf("reference soroban asset: %v", err)
			}
			wantAsset = a
		} else {
			sv, ok := fuzzSymbolAsset(sym)
			if !ok {
				t.Skip("not a valid ScSymbol")
			}
			a, err := canonical.MapOracleSymbol(sym)
			if err != nil {
				t.Skip("symbol unrepresentable as an asset")
			}
			assetSv, wantAsset = sv, a
		}
		opIdx := int(op % 100) // Stellar caps ops/tx at 100
		evIdx := int(ev % eventFanoutStride)
		variant := Variant(variantByte%3 + 1)
		closedAt := time.Unix(close2026, 0).UTC()
		wantPrice := ref128(hi, lo)

		e := &events.Event{
			ContractID:     adapterContract,
			Ledger:         62_000_000,
			TxHash:         "fuzz",
			OperationIndex: opIdx,
			EventIndex:     evIdx,
			Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, encodeTimestampTopic(t, tsMs)},
			Value:          encodeUpdateBody(t, []xdr.ScVal{assetSv}, []*big.Int{wantPrice}),
		}
		updates, err := decodeUpdate(e, variant, DefaultDecimals, "", closedAt)

		if wantPrice.Sign() <= 0 {
			if !errors.Is(err, ErrEmptyPrices) || updates != nil {
				t.Fatalf("non-positive price %s: got (%v, %v), want ErrEmptyPrices", wantPrice, updates, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("well-formed event rejected: %v", err)
		}
		if len(updates) != 1 {
			t.Fatalf("got %d updates for one slot", len(updates))
		}
		u := updates[0]
		if u.Price.BigInt().Cmp(wantPrice) != 0 {
			t.Fatalf("price = %s, want the exact i128 %s (ADR-0003: no truncation)", u.Price, wantPrice)
		}
		if !u.Asset.Equal(wantAsset) {
			t.Fatalf("asset = %s, want %s", u.Asset, wantAsset)
		}
		if u.Quote.String() != "fiat:USD" || u.Decimals != DefaultDecimals || u.Source != variant.SourceName() {
			t.Fatalf("row metadata = (%s, %d, %s)", u.Quote, u.Decimals, u.Source)
		}
		wantOp := uint64(opIdx*eventFanoutStride+evIdx) * opIndexFanoutStride
		if uint64(u.OpIndex) != wantOp {
			t.Fatalf("OpIndex = %d, want %d for (op %d, event %d, slot 0)", u.OpIndex, wantOp, opIdx, evIdx)
		}
		// Timestamp is the topic's MILLISECONDS when it is a plausible
		// time at or before close+24h, else the ledger close — never
		// seconds-scaled, never wrapped.
		ceilMs := uint64(closedAt.Add(canonical.SafeUnixFutureWindow).UnixMilli())
		if tsMs >= 1_000_000_000_000 && tsMs <= ceilMs {
			if want := time.UnixMilli(int64(tsMs)).UTC(); !u.Timestamp.Equal(want) {
				t.Fatalf("ts = %s, want topic millis %s", u.Timestamp, want)
			}
		} else if !u.Timestamp.Equal(closedAt) {
			t.Fatalf("ts = %s for out-of-window topic %d, want the close %s", u.Timestamp, tsMs, closedAt)
		}
	})
}

// FuzzSdkDecodeUpdateBodyRaw feeds arbitrary XDR to the body decoder.
// Beyond not panicking, a success must mean the body really was the
// #[contractevent] Map carrying update_data, with exactly one entry per
// update_data slot (DAT-03 slot stability) — a foreign shape must be
// refused, not mis-decoded.
func FuzzSdkDecodeUpdateBodyRaw(f *testing.F) {
	fixRoot := filepath.Join("..", "..", "..", "test", "fixtures", "reflector")
	dirs, _ := os.ReadDir(fixRoot)
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(fixRoot, d.Name(), "*.json"))
		for _, p := range files {
			raw, err := os.ReadFile(p)
			if err != nil {
				f.Fatalf("read %s: %v", p, err)
			}
			var fx struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(raw, &fx); err != nil {
				f.Fatalf("unmarshal %s: %v", p, err)
			}
			b, err := base64.StdEncoding.DecodeString(fx.Value)
			if err != nil {
				f.Fatalf("fixture value %s: %v", p, err)
			}
			f.Add(b)
		}
	}
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 1})

	f.Fuzz(func(t *testing.T, body []byte) {
		b64 := base64.StdEncoding.EncodeToString(body)
		entries, err := sdkDecodeUpdateBody(b64)
		if err != nil {
			if entries != nil {
				t.Fatalf("error %v returned alongside %d entries", err, len(entries))
			}
			return
		}
		sv, perr := scval.Parse(b64)
		if perr != nil {
			t.Fatalf("decoder accepted bytes scval.Parse rejects: %v", perr)
		}
		m, merr := scval.AsMap(sv)
		if merr != nil {
			t.Fatalf("decoder accepted a non-Map body (%s)", sv.Type)
		}
		ud, uerr := scval.MustMapField(m, "update_data")
		if uerr != nil {
			t.Fatal("decoder accepted a Map with no update_data field")
		}
		vec, verr := scval.AsVec(ud)
		if verr != nil {
			t.Fatalf("decoder accepted a non-Vec update_data (%s)", ud.Type)
		}
		if len(entries) != len(vec) {
			t.Fatalf("%d entries for %d update_data slots; slot count must be stable", len(entries), len(vec))
		}
		for i, pair := range vec {
			elts, _ := scval.AsTupleN(pair, 2)
			if len(elts) != 2 || elts[1].Type != xdr.ScValTypeScvI128 {
				t.Fatalf("slot %d accepted with a non-i128 price", i)
			}
			p := *elts[1].I128
			if want := ref128(int64(p.Hi), uint64(p.Lo)); entries[i].Price.BigInt().Cmp(want) != 0 {
				t.Fatalf("slot %d price = %s, want %s", i, entries[i].Price, want)
			}
		}
	})
}
