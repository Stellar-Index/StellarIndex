package canonical

import (
	"math/big"
	"testing"
)

// Edge-case tests for the Amount-zero-value branches that the
// existing amount_test.go suite skipped: nil-input constructors,
// nil-receiver accessors, and the FromUInt256Parts entry point
// that's currently exercised only transitively through scval.

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

func TestFromUInt256Parts_max256(t *testing.T) {
	// All four words at u64-max → 2^256 - 1.
	got := FromUInt256Parts(^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0))
	want := new(big.Int).Lsh(big.NewInt(1), 256)
	want.Sub(want, big.NewInt(1))
	if got.BigInt().Cmp(want) != 0 {
		t.Errorf("FromUInt256Parts(max,...) = %s, want %s", got.BigInt(), want)
	}
}
