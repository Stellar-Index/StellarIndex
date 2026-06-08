package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestIsSupplyFlowSym(t *testing.T) {
	for _, s := range []string{"mint", "burn", "clawback"} {
		if !IsSupplyFlowSym(s) {
			t.Errorf("IsSupplyFlowSym(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"transfer", "approve", "set_admin", "set_authorized", "swap", ""} {
		if IsSupplyFlowSym(s) {
			t.Errorf("IsSupplyFlowSym(%q) = true, want false", s)
		}
	}
}

func TestMustBig(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"123", "123"},
		{"-45", "-45"},
		{"", "0"},
		{"not-a-number", "0"},
		{"170141183460469231731687303715884105727", "170141183460469231731687303715884105727"}, // i128 max
	}
	for _, tt := range tests {
		if got := mustBig(tt.in).String(); got != tt.want {
			t.Errorf("mustBig(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestDecodeSupplyAmount(t *testing.T) {
	// Bare i128, positive value 1_000_000 (Hi=0, Lo=value).
	i128 := xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: 1_000_000}}
	amt, _, ok := DecodeSupplyAmount(i128)
	if !ok {
		t.Fatal("DecodeSupplyAmount(i128) ok=false, want true")
	}
	if amt.String() != "1000000" {
		t.Errorf("DecodeSupplyAmount(i128) = %s, want 1000000", amt.String())
	}

	// A non-amount type (bool) is undecodable → ok=false, not a panic.
	b := xdr.ScVal{Type: xdr.ScValTypeScvBool, B: new(bool)}
	if _, reason, ok := DecodeSupplyAmount(b); ok {
		t.Errorf("DecodeSupplyAmount(bool) ok=true, want false (reason=%q)", reason)
	}
}
