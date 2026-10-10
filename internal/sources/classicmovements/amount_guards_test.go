package classicmovements

import (
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestDecodeOp_zeroAmount_errorsLoudly pins the positive-amount guard on
// every op kind whose protocol amount must be > 0: a zero there is a
// corrupt row, and must not be written as a zero-value movement.
func TestDecodeOp_zeroAmount_errorsLoudly(t *testing.T) {
	usdc := mkAlphanum4Asset(t, "USDC", 0x21)
	native := xdr.MustNewNativeAsset()
	cases := []struct {
		name   string
		op     xdr.Operation
		result xdr.OperationResult
	}{
		{"payment", mkPaymentOp(t, 0x22, usdc, 0), mkPaymentSuccessResult()},
		{"create_claimable_balance", mkCreateClaimableBalanceOp(t, usdc, 0, mkClaimant(t, 0x23)), mkCreateClaimableBalanceSuccessResult(t, mkBalanceID(0x24))},
		{"clawback", mkClawbackOp(t, usdc, 0x25, 0), mkClawbackSuccessResult()},
		{"path_payment_strict_send_zero_dest", mkPathPaymentStrictSendOp(t, native, 100, 0x26, usdc, 0), mkPathPaymentStrictSendSuccessResult(t, 0x26, usdc, 0, nil)},
		{"path_payment_strict_receive_zero_dest", mkPathPaymentStrictReceiveOp(t, usdc, 100, 0x27, usdc, 0), mkPathPaymentStrictReceiveSuccessResult(t, 0x27, usdc, 0, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fuzzDecoder(4).decodeOp(1, time.Time{}, "tx", 0, "GSRC", tc.op, tc.result)
			if !errors.Is(err, ErrMalformedMovement) {
				t.Fatalf("err = %v (movements %d), want ErrMalformedMovement", err, len(got))
			}
		})
	}
}

// TestBuildPathPaymentMovement_zeroSourceAmount_errors pins the helper's
// own source-leg guard; both callers pre-validate, so only a direct call
// reaches it.
func TestBuildPathPaymentMovement_zeroSourceAmount_errors(t *testing.T) {
	_, dest := mkAccount(t, 0x28)
	last := xdr.SimplePaymentResult{Destination: dest, Asset: xdr.MustNewNativeAsset(), Amount: 10}
	_, err := buildPathPaymentMovement(1, time.Time{}, "tx", 0, "GSRC", xdr.MustNewNativeAsset(), 0, last)
	if !errors.Is(err, ErrMalformedMovement) {
		t.Fatalf("err = %v, want ErrMalformedMovement", err)
	}
}
