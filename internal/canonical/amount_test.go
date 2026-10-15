package canonical

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
)

// Canonical regression test for ADR-0003 (i128 no-truncation): KALIEN balance 40,000,005,972,900,000,000 (hi=2,
// lo=3106517825480896768). It exceeds i64 max (~9.22e18); a low-word-only decoder shows 310,651,782,548.0896768. Asserts
// the correct 128-bit reconstruction.
func TestAmount_FromInt128Parts_KALIENIncident(t *testing.T) {
	t.Parallel()

	const (
		hi             int64  = 2
		lo             uint64 = 3106517825480896768
		expectedString        = "40000005972900000000"
	)

	got := FromInt128Parts(hi, lo)
	if got.String() != expectedString {
		t.Fatalf("KALIEN incident regression: got %q, want %q", got.String(), expectedString)
	}

	// Cross-check via BigInt.
	want, ok := new(big.Int).SetString(expectedString, 10)
	if !ok {
		t.Fatal("setup: expected string did not parse as big.Int")
	}
	if got.BigInt().Cmp(want) != 0 {
		t.Fatalf("BigInt compare: got %s, want %s", got.BigInt(), want)
	}
}

// TestAmount_FromInt128Parts covers representative corner cases of
// the hi/lo → big.Int reconstruction.
func TestAmount_FromInt128Parts(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		hi   int64
		lo   uint64
		want string
	}{
		"zero": {
			hi: 0, lo: 0, want: "0",
		},
		"positive_small": {
			hi: 0, lo: 42, want: "42",
		},
		"i64_max_exactly": {
			hi: 0, lo: math.MaxInt64, want: "9223372036854775807",
		},
		"one_above_i64_max": {
			hi: 0, lo: uint64(math.MaxInt64) + 1, want: "9223372036854775808",
		},
		"kalien_incident_amount": {
			hi: 2, lo: 3106517825480896768, want: "40000005972900000000",
		},
		"negative_small": {
			hi: -1, lo: math.MaxUint64 - 0, want: "-1",
		},
		"negative_large": {
			hi: -1, lo: math.MaxUint64 - 41, want: "-42",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := FromInt128Parts(tc.hi, tc.lo).String()
			if got != tc.want {
				t.Fatalf("hi=%d lo=%d: got %q, want %q", tc.hi, tc.lo, got, tc.want)
			}
		})
	}
}

// TestAmount_FromUInt128Parts verifies the unsigned path.
func TestAmount_FromUInt128Parts(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		hi   uint64
		lo   uint64
		want string
	}{
		"zero": {
			hi: 0, lo: 0, want: "0",
		},
		"u64_max_in_low_word": {
			hi: 0, lo: math.MaxUint64, want: "18446744073709551615",
		},
		"one_above_u64_max": {
			hi: 1, lo: 0, want: "18446744073709551616",
		},
		"u128_max": {
			hi: math.MaxUint64, lo: math.MaxUint64,
			want: "340282366920938463463374607431768211455",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := FromUInt128Parts(tc.hi, tc.lo).String()
			if got != tc.want {
				t.Fatalf("hi=%d lo=%d: got %q, want %q", tc.hi, tc.lo, got, tc.want)
			}
		})
	}
}

// TestAmount_JSONRoundTrip asserts that Amount always serialises
// to a JSON string (ADR-0003) and deserialises losslessly —
// including values far beyond JSON-number precision.
func TestAmount_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	values := []string{
		"0",
		"1",
		"9223372036854775807",  // i64 max
		"9223372036854775808",  // i64 max + 1 (traditional overflow boundary)
		"18446744073709551616", // u64 max + 1
		"40000005972900000000", // KALIEN incident
		"340282366920938463463374607431768211455", // u128 max
		"-1",
		"-9223372036854775808", // i64 min
	}

	for _, s := range values {
		t.Run(s, func(t *testing.T) {
			original, err := FromString(s)
			if err != nil {
				t.Fatalf("FromString(%q): %v", s, err)
			}

			// Marshal — must be a JSON string.
			b, err := json.Marshal(original)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.HasPrefix(string(b), `"`) {
				t.Fatalf("Amount marshalled to non-string JSON: %s (ADR-0003 violation)", b)
			}

			// Unmarshal — must round-trip losslessly.
			var round Amount
			if err := json.Unmarshal(b, &round); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if round.Cmp(original) != 0 {
				t.Fatalf("round-trip: got %q, want %q", round, original)
			}
		})
	}
}

// The lax JSON-number form is still accepted, for producers not yet using our string convention. It must round-trip
// integers within JSON's lossless integer range (2^53).
func TestAmount_UnmarshalJSONNumber(t *testing.T) {
	t.Parallel()

	// Only test values that fit in JSON's lossless integer range
	// (i.e. the accepted but-not-recommended path).
	const raw = `123456789`
	var got Amount
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.String() != raw {
		t.Fatalf("Number form: got %q, want %q", got, raw)
	}
}

// TestAmount_SQLRoundTrip verifies the database/sql Valuer + Scanner
// round-trip via both string and []byte paths (Postgres NUMERIC
// drivers use both historically).
func TestAmount_FromStringRejectsOversize(t *testing.T) {
	// DoS guard: a multi-megabyte decimal string must be rejected
	// at parse time rather than triggering a giant big.Int alloc.
	// Any input past MaxAmountStringLen is refused.
	t.Parallel()
	oversize := strings.Repeat("9", MaxAmountStringLen+1)
	_, err := FromString(oversize)
	if err == nil {
		t.Fatal("oversize decimal string must be rejected")
	}
	if !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("err should wrap ErrInvalidAmount, got %v", err)
	}
	// Exactly at the cap still parses.
	atCap := strings.Repeat("9", MaxAmountStringLen)
	if _, err := FromString(atCap); err != nil {
		t.Errorf("at-cap input (len=%d) must parse, got %v", MaxAmountStringLen, err)
	}
}

func TestAmount_UnmarshalJSONRejectsOversize(t *testing.T) {
	// JSON path also enforces the cap (inherited via FromString).
	// Covers untrusted-input entry points like future POST bodies.
	t.Parallel()
	oversize := `"` + strings.Repeat("9", MaxAmountStringLen+1) + `"`
	var a Amount
	if err := a.UnmarshalJSON([]byte(oversize)); err == nil {
		t.Fatal("oversize JSON string must be rejected")
	}
}

// encoding/json leaves a non-pointer string target unmodified on `null`, which would silently become FromString("") ==
// zero. An absent money value must not read back as the real value 0, so null needs an explicit guard.
func TestAmount_UnmarshalJSONRejectsNull(t *testing.T) {
	t.Parallel()
	// Seed with a nonzero value so a silent no-op would be visible too,
	// but the real assertion is on the returned error and the value
	// after a failed Unmarshal must not read back as a valid zero.
	a := Amount{value: big.NewInt(42)}
	err := a.UnmarshalJSON([]byte("null"))
	if err == nil {
		t.Fatal("JSON null must be rejected, not silently treated as zero")
	}
	if !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("err should wrap ErrInvalidAmount, got %v", err)
	}
}

// JSON permits whitespace around a value, and a padded null leaves a string target untouched just like a bare one. The
// null guard must not depend on the exact byte spelling.
func TestAmount_UnmarshalJSONRejectsWhitespacePaddedNull(t *testing.T) {
	t.Parallel()
	for _, in := range []string{" null", "null\n", "\t null \r\n"} {
		a := Amount{value: big.NewInt(42)}
		err := a.UnmarshalJSON([]byte(in))
		if !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("UnmarshalJSON(%q) = %v, value %s; want ErrInvalidAmount", in, err, a)
		}
		if a.String() != "42" {
			t.Errorf("UnmarshalJSON(%q) overwrote the receiver with %s", in, a)
		}
	}
}

func TestAmount_SQLRoundTrip(t *testing.T) {
	t.Parallel()

	original, err := FromString("40000005972900000000")
	if err != nil {
		t.Fatal(err)
	}

	// Valuer must emit a string.
	v, err := original.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("Value returned %T, want string", v)
	}

	// Scanner via string.
	var viaString Amount
	if err := viaString.Scan(s); err != nil {
		t.Fatalf("Scan(string): %v", err)
	}
	if viaString.Cmp(original) != 0 {
		t.Fatalf("Scan(string) round-trip: got %q, want %q", viaString, original)
	}

	// Scanner via []byte (common Postgres driver path).
	var viaBytes Amount
	if err := viaBytes.Scan([]byte(s)); err != nil {
		t.Fatalf("Scan([]byte): %v", err)
	}
	if viaBytes.Cmp(original) != 0 {
		t.Fatalf("Scan([]byte) round-trip: got %q, want %q", viaBytes, original)
	}
}

func TestAmount_ScanNullIsAnError(t *testing.T) {
	// A NULL must not become the money value 0: Amount cannot say
	// "absent", so a nullable column goes through sql.NullString.
	a := NewAmount(big.NewInt(42))
	err := a.Scan(nil)
	if err == nil {
		t.Fatalf("Scan(nil) returned nil and left %q — a SQL NULL must not read as an Amount", a.String())
	}
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("Scan(nil) error = %v, want it to wrap ErrInvalidAmount", err)
	}
	if got := a.String(); got != "42" {
		t.Fatalf("Scan(nil) mutated the receiver to %q; a failed scan must leave it alone", got)
	}
}

func TestAmount_ScanRejectsUnsupportedTypes(t *testing.T) {
	// Defensive: time.Time, float64, bool, etc. are not NUMERIC-
	// compatible and must surface as errors rather than silently
	// coerce to zero or panic.
	var a Amount
	for _, src := range []any{
		3.14,
		true,
		struct{}{},
	} {
		if err := a.Scan(src); err == nil {
			t.Errorf("Scan(%T) should error, got nil", src)
		}
	}
}

func TestAmount_zeroValueBigIntReturnsNonNil(t *testing.T) {
	// BigInt() on a zero-value Amount (no constructor called) must
	// return a non-nil *big.Int — callers' fmt.Sprintf("%s", x.BigInt())
	// would panic otherwise.
	var a Amount
	bi := a.BigInt()
	if bi == nil {
		t.Fatal("BigInt() returned nil on zero-value Amount")
	}
	if bi.Sign() != 0 {
		t.Errorf("BigInt().Sign() = %d, want 0", bi.Sign())
	}
}

func TestAmount_zeroValueSignIsZero(t *testing.T) {
	var a Amount
	if a.Sign() != 0 {
		t.Errorf("Sign() = %d on zero-value Amount, want 0", a.Sign())
	}
}

func TestAmount_zeroValueStringIsZero(t *testing.T) {
	var a Amount
	if got := a.String(); got != "0" {
		t.Errorf("String() = %q on zero-value Amount, want \"0\"", got)
	}
}

// Edge cases for the Amount zero-value branches amount_test.go skipped: nil-input constructors, nil-receiver accessors,
// and FromUInt256Parts, otherwise exercised only transitively through scval.

func TestNewAmount_nilInput(t *testing.T) {
	a := NewAmount(nil)
	if a.Sign() != 0 {
		t.Errorf("NewAmount(nil).Sign() = %d, want 0", a.Sign())
	}
	if a.String() != "0" {
		t.Errorf("NewAmount(nil).String() = %q, want \"0\"", a.String())
	}
}

func TestNewAmount_copiesInputBigInt(t *testing.T) {
	// Mutating the caller's *big.Int after construction must NOT
	// affect the Amount — guards the docstring's "copy to prevent
	// shared mutation" promise.
	src := big.NewInt(42)
	a := NewAmount(src)
	src.SetInt64(999)
	if a.BigInt().Cmp(big.NewInt(42)) != 0 {
		t.Errorf("Amount.BigInt() = %s, want 42 (caller mutation should not propagate)", a.BigInt())
	}
}

func TestFromUInt256Parts_composesAllFourWords(t *testing.T) {
	// Each word covers a distinct 64-bit slot:
	//   hiHi << 192 + hiLo << 128 + loHi << 64 + loLo.
	// Verify with a value that exercises each word independently.
	got := FromUInt256Parts(0xAA, 0xBB, 0xCC, 0xDD)
	want := new(big.Int).SetUint64(0xAA)
	want.Lsh(want, 64)
	want.Add(want, new(big.Int).SetUint64(0xBB))
	want.Lsh(want, 64)
	want.Add(want, new(big.Int).SetUint64(0xCC))
	want.Lsh(want, 64)
	want.Add(want, new(big.Int).SetUint64(0xDD))
	if got.BigInt().Cmp(want) != 0 {
		t.Errorf("FromUInt256Parts = %s, want %s", got.BigInt(), want)
	}
}

func TestFromUInt256Parts_zeroIsZero(t *testing.T) {
	got := FromUInt256Parts(0, 0, 0, 0)
	if got.Sign() != 0 {
		t.Errorf("FromUInt256Parts(0,0,0,0).Sign() = %d, want 0", got.Sign())
	}
}

// The I256 bounds: the full signed range survives with no narrowing, and -1 is all ones.
func TestFromInt256Parts_signedRange(t *testing.T) {
	maxW := ^uint64(0)
	half := new(big.Int).Lsh(big.NewInt(1), 255)
	cases := []struct {
		name             string
		hiHi             int64
		hiLo, loHi, loLo uint64
		want             *big.Int
	}{
		{"max", int64(^uint64(0) >> 1), maxW, maxW, maxW, new(big.Int).Sub(half, big.NewInt(1))},
		{"min", -1 << 63, 0, 0, 0, new(big.Int).Neg(half)},
		{"minus one", -1, maxW, maxW, maxW, big.NewInt(-1)},
		{"mainnet ETH/USD answer, 18 decimals", 0, 0, 0xe1, 0xe2c1dc8ba1da8000, func() *big.Int { v, _ := new(big.Int).SetString("4166857000000000000000", 10); return v }()},
	}
	for _, c := range cases {
		got := FromInt256Parts(c.hiHi, c.hiLo, c.loHi, c.loLo)
		if got.BigInt().Cmp(c.want) != 0 {
			t.Errorf("%s: FromInt256Parts = %s, want %s", c.name, got.BigInt(), c.want)
		}
	}
}

func TestFromUInt256Parts_max256(t *testing.T) {
	// All four words at u64-max → 2^256 - 1.
	got := FromUInt256Parts(^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0))
	want := new(big.Int).Lsh(big.NewInt(1), 256)
	want.Sub(want, big.NewInt(1))
	if got.BigInt().Cmp(want) != 0 {
		t.Errorf("FromUInt256Parts(max,...) = %s, want %s", got.BigInt(), want)
	}
}
