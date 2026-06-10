package dispatcher

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestCaptureEligible verifies the census's capture-eligibility gate
// matches what actually lands in soroban_events: Type=Contract,
// ContractId set, body version 0, ≥1 topic.
func TestCaptureEligible(t *testing.T) {
	t.Parallel()

	base, _ := makeBasicContractEvent(t) // contract event, 1 topic, body v0
	if !captureEligible(base) {
		t.Fatal("a basic contract event with a topic should be eligible")
	}

	t.Run("system type ineligible", func(t *testing.T) {
		ev := base
		ev.Type = xdr.ContractEventTypeSystem
		if captureEligible(ev) {
			t.Error("system-type event must be ineligible")
		}
	})

	t.Run("nil contract id ineligible", func(t *testing.T) {
		ev := base
		ev.ContractId = nil
		if captureEligible(ev) {
			t.Error("event with nil ContractId must be ineligible")
		}
	})

	t.Run("body version != 0 ineligible", func(t *testing.T) {
		ev := base
		ev.Body = xdr.ContractEventBody{V: 99}
		if captureEligible(ev) {
			t.Error("unaudited body version must be ineligible")
		}
	})

	t.Run("zero topics ineligible", func(t *testing.T) {
		ev := base
		v0 := *base.Body.V0 // copy so we don't mutate base's shared pointer
		v0.Topics = nil
		ev.Body = xdr.ContractEventBody{V: 0, V0: &v0}
		if captureEligible(ev) {
			t.Error("zero-topic event must be ineligible (NOT NULL topic_0_xdr)")
		}
	})
}

// TestClaimAtomCount verifies the census counts ClaimAtoms exactly the
// way internal/sources/sdex extracts them — one trade per atom across
// the five trade op types, zero for non-trade or failed ops.
func TestClaimAtomCount(t *testing.T) {
	t.Parallel()

	// realClaims builds n value-moving claims (non-zero amounts) — realTradeCount
	// keeps these. mixedClaims appends `zero` both-zero no-op crosses (dust /
	// rounding artifacts the census must drop to equal COUNT(trades)).
	realClaims := func(n int) []xdr.ClaimAtom {
		c := make([]xdr.ClaimAtom, n)
		for i := range c {
			c[i] = xdr.ClaimAtom{Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook, OrderBook: &xdr.ClaimOfferAtom{AmountSold: 100, AmountBought: 200}}
		}
		return c
	}
	mixedClaims := func(nReal, zero int) []xdr.ClaimAtom {
		c := realClaims(nReal)
		for i := 0; i < zero; i++ {
			c = append(c, xdr.ClaimAtom{Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook, OrderBook: &xdr.ClaimOfferAtom{AmountSold: 0, AmountBought: 0}})
		}
		return c
	}

	mkSellOffer := func(n int) (xdr.Operation, xdr.OperationResult) {
		return xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}},
			xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypeManageSellOffer,
					ManageSellOfferResult: &xdr.ManageSellOfferResult{
						Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
						Success: &xdr.ManageOfferSuccessResult{OffersClaimed: realClaims(n)},
					},
				},
			}
	}
	mkPathSend := func(n int) (xdr.Operation, xdr.OperationResult) {
		return xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypePathPaymentStrictSend}},
			xdr.OperationResult{
				Code: xdr.OperationResultCodeOpInner,
				Tr: &xdr.OperationResultTr{
					Type: xdr.OperationTypePathPaymentStrictSend,
					PathPaymentStrictSendResult: &xdr.PathPaymentStrictSendResult{
						Code:    xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess,
						Success: &xdr.PathPaymentStrictSendResultSuccess{Offers: realClaims(n)},
					},
				},
			}
	}

	op, res := mkSellOffer(3)
	if got := claimAtomCount(op, res); got != 3 {
		t.Errorf("ManageSellOffer 3 claims: got %d, want 3", got)
	}
	op, res = mkPathSend(2)
	if got := claimAtomCount(op, res); got != 2 {
		t.Errorf("PathPaymentStrictSend 2 claims: got %d, want 2", got)
	}
	op, res = mkSellOffer(0)
	if got := claimAtomCount(op, res); got != 0 {
		t.Errorf("ManageSellOffer no claims: got %d, want 0", got)
	}

	// Both-zero no-op crosses are dropped (census MUST equal COUNT(trades)):
	// 4 real + 3 both-zero ⇒ 4.
	op2 := xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}}
	res2 := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer,
			ManageSellOfferResult: &xdr.ManageSellOfferResult{
				Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{OffersClaimed: mixedClaims(4, 3)},
			},
		},
	}
	if got := claimAtomCount(op2, res2); got != 4 {
		t.Errorf("4 real + 3 both-zero claims: got %d, want 4", got)
	}

	// Non-trade op (valid inner result, no claim-bearing arm) → 0.
	payOp := xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypePayment}}
	payRes := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:          xdr.OperationTypePayment,
			PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentSuccess},
		},
	}
	if got := claimAtomCount(payOp, payRes); got != 0 {
		t.Errorf("Payment op: got %d, want 0", got)
	}

	// Failed (non-inner) result → 0.
	failOp := xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}}
	if got := claimAtomCount(failOp, xdr.OperationResult{Code: xdr.OperationResultCodeOpBadAuth}); got != 0 {
		t.Errorf("failed op result: got %d, want 0", got)
	}
}

// TestCensusLedger_emptyLedger proves CensusLedger reads a
// transaction-free ledger to zero counts and still extracts the
// header anchors without error.
func TestCensusLedger_emptyLedger(t *testing.T) {
	t.Parallel()

	lcm := emptyLedgerCloseMeta(t, 42)
	c, err := CensusLedger(lcm, testPassphrase)
	if err != nil {
		t.Fatalf("CensusLedger: %v", err)
	}
	if c.LedgerSeq != 42 {
		t.Errorf("LedgerSeq = %d, want 42", c.LedgerSeq)
	}
	if c.SorobanEventCount != 0 {
		t.Errorf("SorobanEventCount = %d, want 0", c.SorobanEventCount)
	}
	if c.ClassicTradeEffectCount != 0 {
		t.Errorf("ClassicTradeEffectCount = %d, want 0", c.ClassicTradeEffectCount)
	}
	if c.TxReadErrors != 0 {
		t.Errorf("TxReadErrors = %d, want 0", c.TxReadErrors)
	}
}
