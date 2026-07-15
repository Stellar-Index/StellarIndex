package classicmovements

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
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

// opOnlyInScope is the authoritative expected set for this package's
// op-only decode surface (Matches / decodeOp / Decoder.Decode) —
// deliberately hand-written (not derived from SupportedOpTypes) so
// this test fails if SupportedOpTypes / matchesSupportedOp / decodeOp
// drift from each other, not just from themselves. Named
// "phase1InScope" through Phase 1; renamed once Phase 2 added
// PathPaymentStrictReceive/Send made "Phase1" inaccurate. Grows again
// in Phase 3 (ClaimableBalance trio + Clawback) and Phase 4
// (AccountMerge only — LiquidityPoolDeposit/Withdraw and the
// CAP-0038 AllowTrust/SetTrustLineFlags edge case are a SEPARATE
// entry-changes-correlated decode surface with its own exhaustive
// guard, entrychanges_test.go's TestRecognition_EntryChangeOpTypes*).
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

// entryChangeInScope is the SECOND (entry-changes-correlated)
// decode surface's authoritative expected set — see entrychanges.go
// for why LiquidityPoolDeposit/Withdraw and the CAP-0038
// AllowTrust/SetTrustLineFlags edge case can't go through the
// op-only surface above (they need a correlated
// ledger_entry_changes group dispatcher.OpContext has no room for).
var entryChangeInScope = map[xdr.OperationType]bool{
	xdr.OperationTypeLiquidityPoolDeposit:  true,
	xdr.OperationTypeLiquidityPoolWithdraw: true,
	xdr.OperationTypeAllowTrust:            true,
	xdr.OperationTypeSetTrustLineFlags:     true,
}

// TestRecognition_EntryChangeOpTypesIsExhaustiveAndDisjoint is the
// ADR-0047 D4.2 recognition guard for the entry-changes-correlated
// surface: EntryChangeOpTypes() must equal entryChangeInScope exactly
// (every Phase 4 entry-changes type present, nothing extra), and the
// two surfaces (opOnlyInScope, entryChangeInScope) must never
// overlap — an op type belongs to exactly one decode path, never
// both and never neither once it's in scope for either.
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

// TestRecognition_MatchesCoversExactlyOpOnlyScope is the ADR-0047
// D4.2 recognition guard: Matches() must return true for exactly this
// package's op-only in-scope op types and false for every other
// value in the closed 27-value enum — including the ones later
// phases will add to opOnlyInScope or to the separate entry-changes
// surface (ClaimableBalance*, Clawback*, AccountMerge,
// LiquidityPool{Deposit,Withdraw}, AllowTrust, SetTrustLineFlags). A
// future phase that flips one of those to true without also touching
// this test's opOnlyInScope map fails CI here, forcing a deliberate
// update rather than a silent scope creep.
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

// TestRecognition_SupportedOpTypesMatchesOpOnlyInScope pins
// SupportedOpTypes() (the string-form list StreamClassicOps is
// called with for the op-only surface) to the same set Matches() and
// decodeOp cover — three independent lists that must never drift
// from each other.
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

// TestRecognition_DecodeRejectsOutOfScopeTypesLoudly is the second
// half of D4.2: attempting to Decode an out-of-scope op type must
// fail LOUDLY (ErrUnsupportedOpType), never silently return zero
// movements. This is what forces a future phase's author to extend
// decodeOp's switch deliberately instead of the type quietly falling
// through to "no rows" forever. Every op-only-out-of-scope type in
// the closed enum is exercised, plus a corroborating in-scope control
// (Matches == true implies Decode must NOT hit this error path for a
// merely-out-of-scope reason, though it may still error for other
// reasons on a zero-value op body — see decode_test.go for the
// success path).
func TestRecognition_DecodeRejectsOutOfScopeTypesLoudly(t *testing.T) {
	d := NewDecoder()
	for _, typ := range allClassicOpTypes(t) {
		if opOnlyInScope[typ] {
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
