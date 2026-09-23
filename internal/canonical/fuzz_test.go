// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical_test

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Every target checks a property against an independent reference
// (math/big, a regexp, or the documented rule restated), never just
// "did not panic". Seeds double as unit tests under plain `go test`.

const (
	fzIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	fzTxHash = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
)

var (
	two64  = new(big.Int).Lsh(big.NewInt(1), 64)
	two127 = new(big.Int).Lsh(big.NewInt(1), 127)
	two128 = new(big.Int).Lsh(big.NewInt(1), 128)
	two256 = new(big.Int).Lsh(big.NewInt(1), 256)
)

// i128Ref decodes hi/lo as a 16-byte big-endian two's-complement
// integer — a byte-level reference independent of FromInt128Parts'
// shift-and-add composition.
func i128Ref(hi int64, lo uint64) *big.Int {
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(uint64(hi) >> (56 - 8*i))
		b[8+i] = byte(lo >> (56 - 8*i))
	}
	v := new(big.Int).SetBytes(b[:])
	if b[0]&0x80 != 0 {
		v.Sub(v, two128)
	}
	return v
}

// assertAmountRoundTrips pins that every wire form of a (string, JSON,
// SQL string, SQL []byte) reproduces want exactly.
func assertAmountRoundTrips(t *testing.T, a c.Amount, want *big.Int) {
	t.Helper()
	if a.String() != want.String() {
		t.Fatalf("String() = %s, want %s", a.String(), want)
	}
	if a.BigInt().Cmp(want) != 0 {
		t.Fatalf("BigInt() = %s, want %s", a.BigInt(), want)
	}
	back, err := c.FromString(a.String())
	if err != nil || !back.Equal(a) {
		t.Fatalf("FromString(String()) = %s, %v; want %s", back, err, a)
	}
	j, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if len(j) == 0 || j[0] != '"' {
		t.Fatalf("MarshalJSON = %s; must be a JSON string, never a number (ADR-0003)", j)
	}
	var fromJSON c.Amount
	if err := json.Unmarshal(j, &fromJSON); err != nil || !fromJSON.Equal(a) {
		t.Fatalf("JSON round trip = %s, %v; want %s", fromJSON, err, a)
	}
	v, err := a.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	var fromSQL, fromBytes c.Amount
	if err := fromSQL.Scan(v); err != nil || !fromSQL.Equal(a) {
		t.Fatalf("Scan(Value()) = %s, %v; want %s", fromSQL, err, a)
	}
	if err := fromBytes.Scan([]byte(v.(string))); err != nil || !fromBytes.Equal(a) {
		t.Fatalf("Scan([]byte) = %s, %v; want %s", fromBytes, err, a)
	}
}

func FuzzFromInt128Parts(f *testing.F) {
	f.Add(int64(2), uint64(3106517825480896768)) // KALIEN incident, ADR-0003
	f.Add(int64(0), uint64(0))
	f.Add(int64(-1), uint64(math.MaxUint64)) // -1
	f.Add(int64(math.MaxInt64), uint64(math.MaxUint64))
	f.Add(int64(math.MinInt64), uint64(0))
	f.Add(int64(0), uint64(math.MaxUint64))
	f.Add(int64(-1), uint64(0))
	f.Fuzz(func(t *testing.T, hi int64, lo uint64) {
		want := i128Ref(hi, lo)
		a := c.FromInt128Parts(hi, lo)
		assertAmountRoundTrips(t, a, want)
		if want.Cmp(new(big.Int).Neg(two127)) < 0 || want.Cmp(two127) >= 0 {
			t.Fatalf("%s outside i128 range", want)
		}
		// Inverse: re-split the value into two's-complement words.
		m := new(big.Int).Mod(a.BigInt(), two128)
		gotLo := new(big.Int).Mod(m, two64).Uint64()
		gotHi := int64(new(big.Int).Rsh(m, 64).Uint64())
		if gotHi != hi || gotLo != lo {
			t.Fatalf("re-split (%d, %d), want (%d, %d)", gotHi, gotLo, hi, lo)
		}
		wantSign := 0
		switch {
		case hi < 0:
			wantSign = -1
		case hi > 0 || lo > 0:
			wantSign = 1
		}
		if a.Sign() != wantSign || a.IsZero() != (wantSign == 0) {
			t.Fatalf("Sign() = %d IsZero() = %v, want sign %d", a.Sign(), a.IsZero(), wantSign)
		}
	})
}

func FuzzFromUInt128Parts(f *testing.F) {
	f.Add(uint64(2), uint64(3106517825480896768))
	f.Add(uint64(0), uint64(0))
	f.Add(uint64(math.MaxUint64), uint64(math.MaxUint64))
	f.Add(uint64(1)<<63, uint64(0))
	f.Fuzz(func(t *testing.T, hi, lo uint64) {
		var b [16]byte
		for i := 0; i < 8; i++ {
			b[i] = byte(hi >> (56 - 8*i))
			b[8+i] = byte(lo >> (56 - 8*i))
		}
		want := new(big.Int).SetBytes(b[:])
		a := c.FromUInt128Parts(hi, lo)
		assertAmountRoundTrips(t, a, want)
		if a.Sign() < 0 || a.BigInt().Cmp(two128) >= 0 {
			t.Fatalf("%s outside u128 range", a)
		}
		// Unsigned and signed decodes agree exactly when hi's top bit is clear.
		if hi < 1<<63 && !a.Equal(c.FromInt128Parts(int64(hi), lo)) {
			t.Fatalf("u128 %s != i128 %s for non-negative hi", a, c.FromInt128Parts(int64(hi), lo))
		}
	})
}

func FuzzFromUInt256Parts(f *testing.F) {
	f.Add(uint64(0), uint64(0), uint64(0), uint64(0))
	f.Add(uint64(1), uint64(2), uint64(3), uint64(4))
	f.Add(uint64(math.MaxUint64), uint64(math.MaxUint64), uint64(math.MaxUint64), uint64(math.MaxUint64))
	f.Fuzz(func(t *testing.T, hh, hl, lh, ll uint64) {
		var b [32]byte
		for w, v := range []uint64{hh, hl, lh, ll} {
			for i := 0; i < 8; i++ {
				b[8*w+i] = byte(v >> (56 - 8*i))
			}
		}
		want := new(big.Int).SetBytes(b[:])
		a := c.FromUInt256Parts(hh, hl, lh, ll)
		assertAmountRoundTrips(t, a, want)
		if a.Sign() < 0 || a.BigInt().Cmp(two256) >= 0 {
			t.Fatalf("%s outside u256 range", a)
		}
		var got [32]byte
		a.BigInt().FillBytes(got[:])
		if got != b {
			t.Fatalf("FillBytes = %x, want %x", got, b)
		}
	})
}

func FuzzAmountFromString(f *testing.F) {
	for _, s := range []string{
		"", "0", "-0", "+7", "-", "+", "40000005972900000000",
		"170141183460469231731687303715884105727", "-170141183460469231731687303715884105728",
		"1.5", "1e3", " 1", "0x10", "1_000", "00012", strings.Repeat("9", c.MaxAmountStringLen),
		strings.Repeat("9", c.MaxAmountStringLen+1),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := c.FromString(s)
		if s == "" {
			if err != nil || !a.IsZero() {
				t.Fatalf(`FromString("") = %s, %v; want zero, nil`, a, err)
			}
			return
		}
		ref, refOK := new(big.Int).SetString(s, 10)
		wantOK := refOK && len(s) <= c.MaxAmountStringLen
		if (err == nil) != wantOK {
			t.Fatalf("FromString(%q) err = %v, reference accept = %v", s, err, wantOK)
		}
		if err != nil {
			return
		}
		assertAmountRoundTrips(t, a, ref)
	})
}

func FuzzAmountUnmarshalJSON(f *testing.F) {
	for _, s := range []string{
		`"123"`, `123`, ` null`, "null\n", `-5`, `"-5"`, `null`, `"null"`, `""`, `1.5`, `1e3`, `true`, `{}`, `[]`,
		`"40000005972900000000"`, `40000005972900000000`, `"1.0"`, `"+1"`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		sentinel := c.FromInt128Parts(0, 424242)
		a := sentinel
		err := a.UnmarshalJSON(b)
		if err != nil {
			if !a.Equal(sentinel) {
				t.Fatalf("failed UnmarshalJSON(%q) mutated receiver to %s", b, a)
			}
			return
		}
		// Reference: the JSON value must be a string or a number whose
		// text is a base-10 integer (empty string means zero).
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var v any
		if derr := dec.Decode(&v); derr != nil {
			t.Fatalf("UnmarshalJSON accepted %q that is not JSON: %v", b, derr)
		}
		var text string
		switch x := v.(type) {
		case string:
			text = x
		case json.Number:
			text = x.String()
		default:
			t.Fatalf("UnmarshalJSON accepted %q of JSON type %T", b, v)
		}
		want := new(big.Int)
		if text != "" {
			var ok bool
			if want, ok = new(big.Int).SetString(text, 10); !ok {
				t.Fatalf("UnmarshalJSON accepted non-integer %q", b)
			}
		}
		assertAmountRoundTrips(t, a, want)
	})
}

func FuzzAmountCmp(f *testing.F) {
	f.Add("1", "2")
	f.Add("-1", "1")
	f.Add("0", "-0")
	f.Add("18446744073709551616", "18446744073709551615")
	f.Add("-170141183460469231731687303715884105728", "170141183460469231731687303715884105727")
	f.Fuzz(func(t *testing.T, x, y string) {
		a, errA := c.FromString(x)
		b, errB := c.FromString(y)
		if errA != nil || errB != nil {
			return
		}
		want := a.BigInt().Cmp(b.BigInt())
		if got := a.Cmp(b); got != want {
			t.Fatalf("Cmp(%s, %s) = %d, want %d", a, b, got, want)
		}
		if got := b.Cmp(a); got != -want {
			t.Fatalf("Cmp not antisymmetric: Cmp(%s, %s) = %d, want %d", b, a, got, -want)
		}
		if a.Equal(b) != (want == 0) {
			t.Fatalf("Equal(%s, %s) = %v, want %v", a, b, a.Equal(b), want == 0)
		}
		if a.Sign() != a.BigInt().Sign() || a.IsZero() != (a.BigInt().Sign() == 0) {
			t.Fatalf("Sign/IsZero disagree with big.Int for %s", a)
		}
	})
}

func FuzzAmountScanInt64(f *testing.F) {
	for _, v := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v int64) {
		var a c.Amount
		if err := a.Scan(v); err != nil {
			t.Fatalf("Scan(int64 %d): %v", v, err)
		}
		assertAmountRoundTrips(t, a, big.NewInt(v))
	})
}

func FuzzParseAsset(f *testing.F) {
	for _, s := range []string{
		"native", "XLM", "xlm", "NATIVE", "fiat:USD", "fiat:usd", "crypto:BTC", "rwa:BENJI", "raw:SolvBTC.BBN_FUNDAMENTAL/USD",
		"USDC-" + fzIssuer, "USDC:" + fzIssuer, "ABCDEFGHIJKL-" + fzIssuer, "ABCDEFGHIJKLM-" + fzIssuer,
		"-" + fzIssuer, "USDC-", "US DC-" + fzIssuer, c.XLMSacContractID, "raw:", "raw: x", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := c.ParseAsset(s)
		if err != nil {
			return
		}
		if verr := a.Validate(); verr != nil {
			t.Fatalf("ParseAsset(%q) = %+v, which fails Validate: %v", s, a, verr)
		}
		again, err := c.ParseAsset(a.String())
		if err != nil || !again.Equal(a) {
			t.Fatalf("ParseAsset(String()) = %+v, %v; want %+v", again, err, a)
		}
		j, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("MarshalJSON: %v", err)
		}
		var fromJSON c.Asset
		if err := json.Unmarshal(j, &fromJSON); err != nil || !fromJSON.Equal(a) {
			t.Fatalf("JSON round trip = %+v, %v; want %+v", fromJSON, err, a)
		}
		v, err := a.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		var fromSQL c.Asset
		if err := fromSQL.Scan(v); err != nil || !fromSQL.Equal(a) {
			t.Fatalf("Scan(Value()) = %+v, %v; want %+v", fromSQL, err, a)
		}
		if a.Type == c.AssetClassic {
			// Identity is (code, issuer): the issuer must be a real
			// G-strkey and the code 1–12 ASCII alphanumerics.
			if !c.IsAccountID(a.Issuer) || !classicCodeRe.MatchString(a.Code) {
				t.Fatalf("classic asset %+v escaped code/issuer validation", a)
			}
		}
	})
}

var classicCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{1,12}$`)

func FuzzParsePair(f *testing.F) {
	for _, s := range []string{
		"native/fiat:USD", "fiat:USD/native", "USDC-" + fzIssuer + "/native", "native/native",
		"raw:BTC/fiat:USD", "raw:A/B/fiat:USD", "/fiat:USD", "native/", c.XLMSacContractID + "/crypto:BTC",
		"crypto:BTC/fiat:USD",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := c.ParsePair(s)
		if err != nil {
			return
		}
		if verr := p.Validate(); verr != nil {
			t.Fatalf("ParsePair(%q) = %s, which fails Validate: %v", s, p, verr)
		}
		if !p.Base.IsMapped() || !p.Quote.IsMapped() {
			t.Fatalf("ParsePair(%q) admitted a raw leg: %s", s, p)
		}
		if p.Base.Equal(p.Quote) {
			t.Fatalf("ParsePair(%q) admitted a self-pair", s)
		}
		again, err := c.ParsePair(p.String())
		if err != nil || !again.Equal(p) {
			t.Fatalf("ParsePair(String()) = %s, %v; want %s", again, err, p)
		}
		if !p.Flip().Flip().Equal(p) || p.Flip().Equal(p) || !p.EqualEitherWay(p.Flip()) {
			t.Fatalf("Flip laws broken for %s", p)
		}
		cp, flipped := p.Canonical()
		cf, flippedF := p.Flip().Canonical()
		if !cp.Equal(cf) || flipped == flippedF {
			t.Fatalf("Canonical not orientation-invariant: %s(%v) vs %s(%v)", cp, flipped, cf, flippedF)
		}
		if flipped != !cp.Equal(p) || (flipped && !cp.Equal(p.Flip())) {
			t.Fatalf("Canonical(%s) = %s, flipped=%v inconsistent", p, cp, flipped)
		}
	})
}

var rawSymbolRe = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)

// FuzzMapOracleSymbol pins that an oracle symbol is either mapped onto
// its allow-list asset or kept verbatim as a raw asset — never dropped,
// never rewritten.
func FuzzMapOracleSymbol(f *testing.F) {
	for _, s := range []string{"USD", "BTC", "BENJI", "SolvBTC.BBN_FUNDAMENTAL/USD", "usd", "", "A B", strings.Repeat("X", 65)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sym string) {
		a, err := c.MapOracleSymbol(sym)
		known := c.IsKnownFiat(sym) || c.IsKnownCrypto(sym) || c.IsKnownRWA(sym)
		if (err == nil) != (known || rawSymbolRe.MatchString(sym)) {
			t.Fatalf("MapOracleSymbol(%q) err = %v", sym, err)
		}
		if err != nil {
			return
		}
		if a.Code != sym || a.IsMapped() != known || a.Validate() != nil {
			t.Fatalf("MapOracleSymbol(%q) = %+v, want code kept verbatim, mapped = %v", sym, a, known)
		}
		again, err := c.ParseAsset(a.String())
		if err != nil || !again.Equal(a) {
			t.Fatalf("ParseAsset(%q) = %+v, %v; want %+v", a.String(), again, err, a)
		}
	})
}

// quoteRankRef restates the documented orientation rule: fiat (4) >
// stablecoin code (3) > XLM (2) > anything else (1).
func quoteRankRef(id string) int {
	if strings.HasPrefix(id, "fiat:") {
		return 4
	}
	code := id
	if i := strings.IndexByte(code, '-'); i > 0 {
		code = code[:i]
	}
	if c.StablecoinCodes[strings.TrimPrefix(code, "crypto:")] {
		return 3
	}
	if id == "native" || id == c.XLMSacContractID {
		return 2
	}
	return 1
}

func FuzzOrient(f *testing.F) {
	for _, p := range [][2]string{
		{"native", "USDC-" + fzIssuer},
		{"AQUA-" + fzIssuer, "native"},
		{"fiat:USD", "crypto:USDT"},
		{"AQUA-" + fzIssuer, "yBTC-" + fzIssuer},
		{"native", c.XLMSacContractID},
		{"x", "x"},
		{"crypto:BTC", "crypto:ETH"},
	} {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		base, quote, flipped := c.Orient(a, b)
		if a == b {
			if base != a || quote != b {
				t.Fatalf("Orient(%q, %q) = %q/%q", a, b, base, quote)
			}
			return
		}
		ra, rb := quoteRankRef(a), quoteRankRef(b)
		wantQuote := b
		if ra > rb || (ra == rb && a > b) {
			wantQuote = a
		}
		if quote != wantQuote || flipped != (wantQuote == a) {
			t.Fatalf("Orient(%q, %q) = %q/%q flipped=%v, want quote %q", a, b, base, quote, flipped, wantQuote)
		}
		if (base != a || quote != b) && (base != b || quote != a) {
			t.Fatalf("Orient(%q, %q) = %q/%q is not a permutation", a, b, base, quote)
		}
		base2, quote2, flipped2 := c.Orient(b, a)
		if base2 != base || quote2 != quote || flipped2 == flipped {
			t.Fatalf("Orient not symmetric: (%q,%q,%v) vs (%q,%q,%v)", base, quote, flipped, base2, quote2, flipped2)
		}
	})
}

// fuzzCloseBound keeps closedAt ±~3000 years around the epoch so the
// reference int64 arithmetic below cannot overflow.
const fuzzCloseBound = int64(1e11)

func FuzzSafeUnixSeconds(f *testing.F) {
	const closeRef = int64(1_745_000_000)
	for _, raw := range []uint64{0, 999_999_999, 1_000_000_000, uint64(closeRef), uint64(closeRef + 86400), uint64(closeRef + 86401), math.MaxUint64, 1 << 63} {
		f.Add(raw, closeRef)
	}
	f.Add(uint64(1_000_000_000), int64(-86400))
	f.Add(uint64(1<<63), int64(-100_000))
	f.Fuzz(func(t *testing.T, raw uint64, closeSec int64) {
		if closeSec > fuzzCloseBound || closeSec < -fuzzCloseBound {
			return
		}
		closedAt := time.Unix(closeSec, 0).In(time.FixedZone("X", 3600))
		got := c.SafeUnixSeconds(raw, closedAt)
		ceil := closeSec + 86400
		want := closedAt.UTC()
		if ceil >= 0 && raw >= 1_000_000_000 && raw <= uint64(ceil) {
			want = time.Unix(int64(raw), 0).UTC()
		}
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Fatalf("SafeUnixSeconds(%d, %d) = %v, want %v", raw, closeSec, got, want)
		}
	})
}

func FuzzSafeUnixMillis(f *testing.F) {
	const closeRef = int64(1_745_000_000_000)
	for _, raw := range []uint64{0, 999_999_999_999, 1_000_000_000_000, uint64(closeRef), uint64(closeRef + 86_400_000), uint64(closeRef + 86_400_001), math.MaxUint64, 1 << 63} {
		f.Add(raw, closeRef)
	}
	f.Add(uint64(1_000_000_000_000), int64(-86_400_000))
	f.Fuzz(func(t *testing.T, raw uint64, closeMs int64) {
		if closeMs > fuzzCloseBound*1000 || closeMs < -fuzzCloseBound*1000 {
			return
		}
		closedAt := time.UnixMilli(closeMs)
		got := c.SafeUnixMillis(raw, closedAt)
		ceil := closeMs + 86_400_000
		want := closedAt.UTC()
		if ceil >= 0 && raw >= 1_000_000_000_000 && raw <= uint64(ceil) {
			want = time.UnixMilli(int64(raw)).UTC()
		}
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Fatalf("SafeUnixMillis(%d, %d) = %v, want %v", raw, closeMs, got, want)
		}
	})
}

func FuzzUnboundedUnixSeconds(f *testing.F) {
	for _, raw := range []uint64{0, 1, math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw uint64) {
		got, ok := c.UnboundedUnixSeconds(raw)
		if ok != (raw <= math.MaxInt64) {
			t.Fatalf("UnboundedUnixSeconds(%d) ok = %v", raw, ok)
		}
		if ok && (got.Unix() != int64(raw) || got.Location() != time.UTC) {
			t.Fatalf("UnboundedUnixSeconds(%d) = %v", raw, got)
		}
		if !ok && !got.IsZero() {
			t.Fatalf("UnboundedUnixSeconds(%d) rejected but returned %v", raw, got)
		}
	})
}

func FuzzFanoutOpIndex(f *testing.F) {
	for _, p := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {0xFFFF, 0xFFFF}, {0x10000, 0}, {0, 0x10000}, {-1, 0}, {0, -1}} {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, op, ev int) {
		inRange := op >= 0 && op <= 0xFFFF && ev >= 0 && ev <= 0xFFFF
		defer func() {
			if r := recover(); (r != nil) == inRange {
				t.Fatalf("FanoutOpIndex(%d, %d) panic = %v, in range = %v", op, ev, r, inRange)
			}
		}()
		got := c.FanoutOpIndex(op, ev)
		// Injective: the packed index decodes back to both inputs.
		if int(got>>16) != op || int(got&0xFFFF) != ev {
			t.Fatalf("FanoutOpIndex(%d, %d) = %#x does not decode back", op, ev, got)
		}
	})
}

var txHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func FuzzTradeValidateTxHash(f *testing.F) {
	for _, h := range []string{fzTxHash, strings.ToUpper(fzTxHash), fzTxHash + "0", fzTxHash[:63], fzTxHash[:63] + "g", ""} {
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, h string) {
		tr := c.Trade{
			Source:      "sdex",
			TxHash:      h,
			Timestamp:   time.Unix(1_745_000_000, 0).UTC(),
			Pair:        c.Pair{Base: c.NativeAsset(), Quote: c.Asset{Type: c.AssetFiat, Code: "USD"}},
			BaseAmount:  c.FromInt128Parts(0, 1),
			QuoteAmount: c.FromInt128Parts(0, 1),
		}
		if (tr.Validate() == nil) != txHashRe.MatchString(h) {
			t.Fatalf("Trade.Validate tx_hash %q: err = %v, want accept = %v", h, tr.Validate(), txHashRe.MatchString(h))
		}
	})
}

func FuzzOraclePriceFloat(f *testing.F) {
	f.Add("123456789", uint8(0))
	f.Add("123456789", uint8(7))
	f.Add("40000005972900000000", uint8(14))
	f.Add("1", uint8(38))
	f.Add("-5", uint8(3))
	f.Fuzz(func(t *testing.T, s string, dec uint8) {
		price, err := c.FromString(s)
		if err != nil || price.IsZero() {
			return
		}
		got, _ := c.OracleUpdate{Price: price, Decimals: dec}.PriceFloat().Rat(nil)
		want := new(big.Rat).SetFrac(price.BigInt(), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil))
		// |got - want| <= |want| * 2^-60: correct scale direction and
		// exponent, with no worse than big.Float's 64-bit mantissa.
		diff := new(big.Rat).Sub(got, want)
		diff.Abs(diff)
		tol := new(big.Rat).Abs(want)
		tol.Mul(tol, new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Lsh(big.NewInt(1), 60)))
		if diff.Cmp(tol) > 0 {
			t.Fatalf("PriceFloat(%s, %d) = %s, want ~%s", s, dec, got.FloatString(40), want.FloatString(40))
		}
	})
}

// FuzzAssetFromXDR pins that an XDR classic asset is either rejected or
// decoded to exactly its own (code, issuer) identity: only trailing
// null padding is stripped, never an interior or control byte.
func FuzzAssetFromXDR(f *testing.F) {
	issuer := bytes.Repeat([]byte{7}, 32)
	f.Add(false, []byte("USDC"), issuer)
	f.Add(true, []byte("AQUA\x00\x00\x00\x00\x00\x00\x00\x00"), issuer)
	f.Add(false, []byte("A\x00B\x00"), issuer)
	f.Add(false, []byte("AB\x01\x00"), issuer)
	f.Add(false, []byte("\x00\x00\x00\x00"), issuer)
	f.Add(true, []byte("ABCDEFGHIJKL"), issuer)
	f.Fuzz(func(t *testing.T, twelve bool, code, key []byte) {
		if len(key) != 32 {
			return
		}
		var ed xdr.Uint256
		copy(ed[:], key)
		acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &ed}
		var x xdr.Asset
		var raw []byte
		if twelve {
			var c12 xdr.AssetCode12
			copy(c12[:], code)
			raw = c12[:]
			x = xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: &xdr.AlphaNum12{AssetCode: c12, Issuer: acct}}
		} else {
			var c4 xdr.AssetCode4
			copy(c4[:], code)
			raw = c4[:]
			x = xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{AssetCode: c4, Issuer: acct}}
		}
		wantCode := string(bytes.TrimRight(raw, "\x00"))
		a, err := c.AssetFromXDR(x)
		if (err == nil) != classicCodeRe.MatchString(wantCode) {
			t.Fatalf("AssetFromXDR(code %q) err = %v, want accept = %v", raw, err, classicCodeRe.MatchString(wantCode))
		}
		if err != nil {
			return
		}
		if a.Type != c.AssetClassic || a.Code != wantCode {
			t.Fatalf("AssetFromXDR(code %q) = %+v, want classic code %q", raw, a, wantCode)
		}
		gotKey, err := strkey.Decode(strkey.VersionByteAccountID, a.Issuer)
		if err != nil || !bytes.Equal(gotKey, key) {
			t.Fatalf("issuer %s does not decode to the XDR key: %v", a.Issuer, err)
		}
	})
}
