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

// TestAssembleTokenSupply_NegativeMarkedIncomplete pins the source-of-truth
// guard: a lake-flows net total below zero (Σ(burn+clawback) > Σmint) is
// physically impossible for a real token — the flows are incompletely seeded
// (e.g. pre-Soroban SAC-wrapper mints not yet CAP-67-replayed) — so
// assembleTokenSupply flags Incomplete. Both serving consumers
// (/v1/assets/{id} F2 fallback and /v1/assets/{id}/supply) refuse an
// Incomplete supply rather than publish a negative (ADR-0003; migration 0005
// enforces total_supply/circulating_supply >= 0). Mirrors
// SEP41Computer.Compute refusing ErrNegativeTotalSupply.
func TestAssembleTokenSupply_NegativeMarkedIncomplete(t *testing.T) {
	// burn alone exceeds mint → negative net total.
	if ts := assembleTokenSupply("C_BURN", "100", "150", "0", 3); ts.Total.String() != "-50" || !ts.Incomplete {
		t.Errorf("mint=100 burn=150: Total=%s Incomplete=%v, want -50/true", ts.Total, ts.Incomplete)
	}
	// clawback alone pushes it negative → exercised independently.
	if ts := assembleTokenSupply("C_CLAW", "100", "0", "150", 3); ts.Total.String() != "-50" || !ts.Incomplete {
		t.Errorf("mint=100 clawback=150: Total=%s Incomplete=%v, want -50/true", ts.Total, ts.Incomplete)
	}
	// A normal token (mint >= burn+clawback) is NOT flagged and Total is
	// byte-identical to the pre-guard behaviour.
	if ts := assembleTokenSupply("C_OK", "1000", "80", "20", 7); ts.Total.String() != "900" || ts.Incomplete {
		t.Errorf("normal token: Total=%s Incomplete=%v, want 900/false", ts.Total, ts.Incomplete)
	}
	// Exact zero (fully burned) is a legitimate non-negative reading — the
	// guard must not over-reach into the real zero case.
	if ts := assembleTokenSupply("C_ZERO", "100", "100", "0", 2); ts.Total.Sign() != 0 || ts.Incomplete {
		t.Errorf("fully-burned token: Total=%s Incomplete=%v, want 0/false", ts.Total, ts.Incomplete)
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
