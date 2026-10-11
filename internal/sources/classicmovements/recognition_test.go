package classicmovements

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// allClassicOpTypes enumerates the CLOSED 27-value xdr.OperationType enum (0..26). Because the
// enum is closed, recognition is a static switch-coverage check rather than the contract-identity
// gating of ADR-0035. A 28th operation type must fail here as a visible change, not a silent gap.
func allClassicOpTypes(t *testing.T) []xdr.OperationType {
	t.Helper()
	const closedEnumSize = 27
	out := make([]xdr.OperationType, 0, closedEnumSize)
	for i := int32(0); i < closedEnumSize; i++ {
		typ := xdr.OperationType(i)
		if !typ.ValidEnum(i) {
			t.Fatalf("xdr.OperationType(%d) is not a valid enum value — the closed-27 assumption this test pins is stale; update allClassicOpTypes AND classicmovements' phase scope together", i)
		}
		out = append(out, typ)
	}
	return out
}

// opOnlyInScope is the authoritative expected set for the op-only decode surface (Matches /
// decodeOp / Decoder.Decode). It is hand-written, not derived from SupportedOpTypes, so the test
// fails when those three drift apart. LiquidityPoolDeposit/Withdraw and the CAP-0038
// AllowTrust/SetTrustLineFlags edge case belong to the separate entry-changes surface, guarded by
// entrychanges_test.go's TestRecognition_EntryChangeOpTypes*.
var opOnlyInScope = map[xdr.OperationType]bool{
	xdr.OperationTypeCreateAccount:            true,
	xdr.OperationTypePayment:                  true,
	xdr.OperationTypePathPaymentStrictReceive: true,
	xdr.OperationTypePathPaymentStrictSend:    true,
	xdr.OperationTypeCreateClaimableBalance:   true,
	xdr.OperationTypeClaimClaimableBalance:    true,
	xdr.OperationTypeClawbackClaimableBalance: true,
	xdr.OperationTypeClawback:                 true,
	xdr.OperationTypeAccountMerge:             true,
}

// entryChangeInScope is the expected set for the entry-changes-correlated decode surface. Those op
// types need a correlated ledger_entry_changes group that dispatcher.OpContext has no room for
// (see entrychanges.go).
var entryChangeInScope = map[xdr.OperationType]bool{
	xdr.OperationTypeLiquidityPoolDeposit:  true,
	xdr.OperationTypeLiquidityPoolWithdraw: true,
	xdr.OperationTypeAllowTrust:            true,
	xdr.OperationTypeSetTrustLineFlags:     true,
}

// TestRecognition_EntryChangeOpTypesIsExhaustiveAndDisjoint pins EntryChangeOpTypes() to
// entryChangeInScope exactly, and the two surfaces must never overlap: an in-scope op type belongs
// to exactly one decode path.
func TestRecognition_EntryChangeOpTypesIsExhaustiveAndDisjoint(t *testing.T) {
	got := map[string]bool{}
	for _, s := range EntryChangeOpTypes() {
		got[s] = true
	}
	if len(got) != len(entryChangeInScope) {
		t.Fatalf("EntryChangeOpTypes() has %d entries, entryChangeInScope has %d", len(got), len(entryChangeInScope))
	}
	for typ := range entryChangeInScope {
		if !got[typ.String()] {
			t.Errorf("EntryChangeOpTypes() is missing %s", typ)
		}
	}
	for typ := range entryChangeInScope {
		if opOnlyInScope[typ] {
			t.Errorf("%s is in BOTH opOnlyInScope and entryChangeInScope — an op type must belong to exactly one decode surface", typ)
		}
	}
}

// TestRecognition_MatchesCoversExactlyOpOnlyScope requires Matches() to be true for exactly the
// op-only in-scope types and false for every other value in the closed enum. A change that flips
// another type to true without updating opOnlyInScope fails here instead of creeping in silently.
func TestRecognition_MatchesCoversExactlyOpOnlyScope(t *testing.T) {
	d := NewDecoder()
	for _, typ := range allClassicOpTypes(t) {
		op := xdr.Operation{Body: xdr.OperationBody{Type: typ}}
		got := d.Matches(op)
		want := opOnlyInScope[typ]
		if got != want {
			t.Errorf("Matches(%s) = %v, want %v", typ, got, want)
		}
	}
}

// TestRecognition_SupportedOpTypesMatchesOpOnlyInScope pins SupportedOpTypes() (the list
// StreamClassicOps is called with) to the set Matches() and decodeOp cover. The three must not
// drift.
func TestRecognition_SupportedOpTypesMatchesOpOnlyInScope(t *testing.T) {
	got := map[string]bool{}
	for _, s := range SupportedOpTypes() {
		got[s] = true
	}
	if len(got) != len(opOnlyInScope) {
		t.Fatalf("SupportedOpTypes() has %d entries, opOnlyInScope has %d", len(got), len(opOnlyInScope))
	}
	for typ := range opOnlyInScope {
		if !got[typ.String()] {
			t.Errorf("SupportedOpTypes() is missing %s, which opOnlyInScope (and Matches) expects in-scope", typ)
		}
	}
}

// TestRecognition_DecodeRejectsOutOfScopeTypesLoudly requires Decode of an out-of-scope op type to
// fail with ErrUnsupportedOpType rather than silently return zero movements, so a new type cannot
// fall through to "no rows" forever. An in-scope control must not hit that error (it may still
// fail for other reasons on a zero-value body; decode_test.go covers the success path).
func TestRecognition_DecodeRejectsOutOfScopeTypesLoudly(t *testing.T) {
	d := NewDecoder()
	for _, typ := range allClassicOpTypes(t) {
		if opOnlyInScope[typ] {
			continue // in-scope types are covered by decode_test.go's success/failure cases
		}
		t.Run(typ.String(), func(t *testing.T) {
			op := xdr.Operation{Body: xdr.OperationBody{Type: typ}}
			// Matches() would already gate this op out of the real backfill loop; Decode is
			// called directly to prove the loud-failure contract, not a realistic op/result pair.
			ctx := dispatcher.OpContext{Op: op, TxSource: "GTEST"}
			_, err := d.Decode(ctx)
			if err == nil {
				t.Fatalf("Decode(%s) returned no error — an out-of-scope op type must fail loudly (ErrUnsupportedOpType), not silently emit zero movements", typ)
			}
			if !errors.Is(err, ErrUnsupportedOpType) {
				t.Errorf("Decode(%s) error = %v, want errors.Is(err, ErrUnsupportedOpType)", typ, err)
			}
		})
	}
}
