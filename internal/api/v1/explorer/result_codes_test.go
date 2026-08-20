package explorer

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestTxResultNamesExhaustive asserts every VALID transaction result code the
// SDK knows maps to a non-empty, non-fallback friendly slug. It ranges a code
// window and gates on the SDK's own ValidEnum, so when a future SDK adds a new
// result code this test goes RED until result_codes.go maps it — a blank/
// "unknown" slug for a real code must never reach the wire.
func TestTxResultNamesExhaustive(t *testing.T) {
	var probe xdr.TransactionResultCode
	seen := 0
	for c := int32(-64); c <= 16; c++ {
		if !probe.ValidEnum(c) {
			continue
		}
		seen++
		got := txResultName(c)
		if got == "" || strings.Contains(got, "unknown") {
			t.Errorf("valid tx result code %d (%s) has no friendly slug, got %q",
				c, xdr.TransactionResultCode(c).String(), got)
		}
	}
	if seen < 15 { // guard the probe window actually covered the enum
		t.Fatalf("only %d valid tx result codes discovered; probe window too narrow?", seen)
	}
}

// TestOpResultNamesExhaustive is the operation-code analog.
func TestOpResultNamesExhaustive(t *testing.T) {
	var probe xdr.OperationResultCode
	seen := 0
	for c := int32(-32); c <= 8; c++ {
		if !probe.ValidEnum(c) {
			continue
		}
		seen++
		got := opResultName(c)
		if got == "" || strings.Contains(got, "unknown") {
			t.Errorf("valid op result code %d (%s) has no friendly slug, got %q",
				c, xdr.OperationResultCode(c).String(), got)
		}
	}
	if seen < 5 {
		t.Fatalf("only %d valid op result codes discovered; probe window too narrow?", seen)
	}
}

func TestResultNameSpecifics(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"tx success", txResultName(int32(xdr.TransactionResultCodeTxSuccess)), "tx_success"},
		{"tx failed", txResultName(int32(xdr.TransactionResultCodeTxFailed)), "tx_failed"},
		{"tx insufficient fee", txResultName(int32(xdr.TransactionResultCodeTxInsufficientFee)), "tx_insufficient_fee"},
		{"tx no source account", txResultName(int32(xdr.TransactionResultCodeTxNoAccount)), "tx_no_source_account"},
		{"op inner", opResultName(int32(xdr.OperationResultCodeOpInner)), "op_inner"},
		{"op bad auth", opResultName(int32(xdr.OperationResultCodeOpBadAuth)), "op_bad_auth"},
		{"op no source account", opResultName(int32(xdr.OperationResultCodeOpNoAccount)), "op_no_source_account"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestResultNameUnknownFallback pins the fallback: an unmapped code is never
// blank (which would read as "no failure") and never implies success — it
// surfaces the raw integer truthfully.
func TestResultNameUnknownFallback(t *testing.T) {
	// 99 is not a valid TransactionResultCode.
	if got := txResultName(99); got == "" || !strings.Contains(got, "99") {
		t.Errorf("unknown tx code fallback = %q, want a non-empty slug carrying the raw code", got)
	}
	if got := opResultName(99); got == "" || !strings.Contains(got, "99") {
		t.Errorf("unknown op code fallback = %q, want a non-empty slug carrying the raw code", got)
	}
}
