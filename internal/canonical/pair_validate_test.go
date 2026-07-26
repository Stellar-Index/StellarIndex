package canonical_test

import (
	"errors"
	"strings"
	"testing"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Pair.Validate has three branches (base error, quote error,
// identity collision) that the existing pair_test.go suite only
// hits transitively via NewPair. Direct tests pin each branch so
// a refactor that drops any of them surfaces here, not in a
// downstream caller's confusing stack trace.

func TestPair_Validate_zeroValueSurfacesBaseError(t *testing.T) {
	// Zero-value Pair: both Base and Quote are zero-value Asset.
	// Validate hits the Base branch first and fails there.
	var p c.Pair
	err := p.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "base") {
		t.Errorf("error %q missing \"base\" fragment", err.Error())
	}
}

func TestPair_Validate_validBaseInvalidQuote(t *testing.T) {
	// Base is valid (native XLM); Quote is zero-value → "quote"-
	// tagged error.
	p := c.Pair{
		Base:  c.NativeAsset(),
		Quote: c.Asset{}, // zero-value, fails Validate
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "quote") {
		t.Errorf("error %q missing \"quote\" fragment", err.Error())
	}
}

func TestPair_Validate_identityCollision(t *testing.T) {
	// Both sides are valid native XLM — Validate must catch the
	// identity collision and return ErrPairMismatch.
	p := c.Pair{Base: c.NativeAsset(), Quote: c.NativeAsset()}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected validation error on identity pair, got nil")
	}
	if !errors.Is(err, c.ErrPairMismatch) {
		t.Errorf("error chain missing ErrPairMismatch: %v", err)
	}
}

// Asset.Value with a zero-value Asset must fail the inner Validate
// — the validation error propagates out through driver.Valuer.
func TestAsset_Value_zeroValueRejected(t *testing.T) {
	var a c.Asset
	_, err := a.Value()
	if err == nil {
		t.Error("expected error from Value() on zero-value Asset, got nil")
	}
}

// Asset.Scan against a NULL source must ERROR and leave the receiver
// untouched (C4-068, audit-2026-07-23).
//
// The pre-C4-068 contract — and this test's original assertion — was the
// opposite: Scan(nil) returned nil and zeroed the receiver, so a NULL
// asset column produced an Asset with Type=="" that reads as valid
// everywhere except Validate. Value() will not write such an Asset, so a
// NULL can only come from a schema/query defect (a LEFT JOIN that
// missed, a column that should be NOT NULL); failing closed surfaces it
// at the read instead of laundering it into the domain. A genuinely
// nullable column must be scanned through sql.NullString + ParseAsset.
func TestAsset_Scan_nullIsAnError(t *testing.T) {
	a := c.NativeAsset()
	err := a.Scan(nil)
	if err == nil {
		t.Fatal("Scan(nil) returned nil — a SQL NULL must not silently become a zero Asset")
	}
	if !errors.Is(err, c.ErrInvalidAsset) {
		t.Errorf("Scan(nil) error = %v, want it to wrap ErrInvalidAsset", err)
	}
	if !a.Equal(c.NativeAsset()) {
		t.Errorf("after a failed Scan(nil), Asset = %+v, want the receiver untouched", a)
	}
}
