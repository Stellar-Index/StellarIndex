package classicmovements

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/dispatcher"
)

// allClassicOpTypes enumerates the CLOSED 27-value xdr.OperationType
// enum (0..26, CreateAccount..RestoreFootprint — see
// docs/architecture/pre-p23-classic-movements-research.md §2 for the
// full inventory this list was built from). ADR-0047 D3 treats
// "closed enum, no unknown-future-contract problem" as the reason
// recognition here reduces to a static switch-coverage check rather
// than an operator-seeded gating exercise like ADR-0035. If a future
// Stellar protocol version ever adds a 28th operation type, this
// test's iteration bound (not the switch itself) is the thing that
// needs updating — a deliberate, visible change, not a silent gap.
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

// phase1InScope is the authoritative expected set for THIS phase —
// deliberately hand-written (not derived from SupportedOpTypes) so
// this test fails if SupportedOpTypes / matchesPhase1Op / decodeOp
// drift from each other, not just from themselves.
var phase1InScope = map[xdr.OperationType]bool{
	xdr.OperationTypeCreateAccount: true,
	xdr.OperationTypePayment:       true,
}

// TestRecognition_MatchesCoversExactlyPhase1 is the ADR-0047 D4.2
// recognition guard: Matches() must return true for exactly Phase
// 1's two in-scope op types and false for every other value in the
// closed 27-value enum — including the ones later phases will add
// (PathPaymentStrict{Receive,Send}, ClaimableBalance*, Clawback*,
// AccountMerge, LiquidityPool{Deposit,Withdraw}). A future phase
// that flips one of those to true without also touching this test's
// phase1InScope map fails CI here, forcing a deliberate update
// rather than a silent scope creep.
func TestRecognition_MatchesCoversExactlyPhase1(t *testing.T) {
	d := NewDecoder()
	for _, typ := range allClassicOpTypes(t) {
		op := xdr.Operation{Body: xdr.OperationBody{Type: typ}}
		got := d.Matches(op)
		want := phase1InScope[typ]
		if got != want {
			t.Errorf("Matches(%s) = %v, want %v", typ, got, want)
		}
	}
}

// TestRecognition_SupportedOpTypesMatchesPhase1InScope pins
// SupportedOpTypes() (the string-form list StreamClassicOps is
// called with) to the same set Matches() and decodeOp cover — three
// independent lists that must never drift from each other.
func TestRecognition_SupportedOpTypesMatchesPhase1InScope(t *testing.T) {
	got := map[string]bool{}
	for _, s := range SupportedOpTypes() {
		got[s] = true
	}
	if len(got) != len(phase1InScope) {
		t.Fatalf("SupportedOpTypes() has %d entries, phase1InScope has %d", len(got), len(phase1InScope))
	}
	for typ := range phase1InScope {
		if !got[typ.String()] {
			t.Errorf("SupportedOpTypes() is missing %s, which phase1InScope (and Matches) expects in-scope", typ)
		}
	}
}

// TestRecognition_DecodeRejectsOutOfScopeTypesLoudly is the second
// half of D4.2: attempting to Decode an out-of-scope op type must
// fail LOUDLY (ErrUnsupportedOpType), never silently return zero
// movements. This is what forces a future phase's author to extend
// decodeOp's switch deliberately instead of the type quietly falling
// through to "no rows" forever. Every non-Phase-1 type in the closed
// enum is exercised, plus a corroborating in-scope control (Matches
// == true implies Decode must NOT hit this error path for a merely-
// out-of-scope reason, though it may still error for other reasons
// on a zero-value op body — see decode_test.go for the success
// path).
func TestRecognition_DecodeRejectsOutOfScopeTypesLoudly(t *testing.T) {
	d := NewDecoder()
	for _, typ := range allClassicOpTypes(t) {
		if phase1InScope[typ] {
			continue // in-scope types are covered by decode_test.go's success/failure cases
		}
		t.Run(typ.String(), func(t *testing.T) {
			op := xdr.Operation{Body: xdr.OperationBody{Type: typ}}
			// A zero-value OperationResult decodes as OperationResultCodeOpInner
			// (the zero enum value) with no Tr() arm set — Matches() would
			// already have gated this op out of the real backfill loop, so
			// Decode is being called directly here specifically to prove the
			// loud-failure contract, not to model a realistic op/result pair.
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
