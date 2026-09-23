package xdrjson

import (
	"reflect"
	"regexp"
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
		got := TxResultName(c)
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
		got := OpResultName(c)
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
		{"tx success", TxResultName(int32(xdr.TransactionResultCodeTxSuccess)), "tx_success"},
		{"tx failed", TxResultName(int32(xdr.TransactionResultCodeTxFailed)), "tx_failed"},
		{"tx insufficient fee", TxResultName(int32(xdr.TransactionResultCodeTxInsufficientFee)), "tx_insufficient_fee"},
		{"tx no source account", TxResultName(int32(xdr.TransactionResultCodeTxNoAccount)), "tx_no_source_account"},
		{"op inner", OpResultName(int32(xdr.OperationResultCodeOpInner)), "op_inner"},
		{"op bad auth", OpResultName(int32(xdr.OperationResultCodeOpBadAuth)), "op_bad_auth"},
		{"op no source account", OpResultName(int32(xdr.OperationResultCodeOpNoAccount)), "op_no_source_account"},
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
	if got := TxResultName(99); got == "" || !strings.Contains(got, "99") {
		t.Errorf("unknown tx code fallback = %q, want a non-empty slug carrying the raw code", got)
	}
	if got := OpResultName(99); got == "" || !strings.Contains(got, "99") {
		t.Errorf("unknown op code fallback = %q, want a non-empty slug carrying the raw code", got)
	}
}

// opResultB64 marshals an applied (op_inner) OperationResult.
func opResultB64(t *testing.T, tr xdr.OperationResultTr) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &tr})
	if err != nil {
		t.Fatalf("marshal op result: %v", err)
	}
	return b64
}

// TestOpInnerResultName pins that an op_inner operation's reason is decoded
// from its stored result XDR: without it a failed tx's operations all read
// "op_inner" and nothing says which one failed or why.
func TestOpInnerResultName(t *testing.T) {
	underfunded := opResultB64(t, xdr.OperationResultTr{
		Type:          xdr.OperationTypePayment,
		PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentUnderfunded},
	})
	if got, ok := OpInnerResultName(underfunded); !ok || got != "payment_underfunded" {
		t.Fatalf("payment underfunded = (%q, %v), want (payment_underfunded, true)", got, ok)
	}
	trapped := opResultB64(t, xdr.OperationResultTr{
		Type: xdr.OperationTypeInvokeHostFunction,
		InvokeHostFunctionResult: &xdr.InvokeHostFunctionResult{
			Code: xdr.InvokeHostFunctionResultCodeInvokeHostFunctionTrapped,
		},
	})
	if got, ok := OpInnerResultName(trapped); !ok || got != "invoke_host_function_trapped" {
		t.Fatalf("invoke trapped = (%q, %v), want (invoke_host_function_trapped, true)", got, ok)
	}
	badAuth, err := xdr.MarshalBase64(xdr.OperationResult{Code: xdr.OperationResultCodeOpBadAuth})
	if err != nil {
		t.Fatalf("marshal op_bad_auth: %v", err)
	}
	if got, ok := OpInnerResultName(badAuth); ok || got != "" {
		t.Fatalf("op_bad_auth has no inner result, got (%q, %v)", got, ok)
	}
	if got, ok := OpInnerResultName("not-xdr"); ok || got != "" {
		t.Fatalf("undecodable XDR = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestOpInnerResultNamesExhaustive walks every op type's result union and
// every valid code in it: each must name to a clean snake_case slug, so a new
// op type or code arriving with an SDK bump cannot reach the wire unnamed.
func TestOpInnerResultNamesExhaustive(t *testing.T) {
	var opProbe xdr.OperationType
	slug := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$`)
	seen := 0
	for ot := int32(0); ot <= 64; ot++ {
		if !opProbe.ValidEnum(ot) {
			continue
		}
		tr := xdr.OperationResultTr{Type: xdr.OperationType(ot)}
		arm, ok := tr.ArmForSwitch(ot)
		if !ok {
			t.Fatalf("op type %d has no result arm", ot)
		}
		field := reflect.ValueOf(&tr).Elem().FieldByName(arm)
		for c := int32(-32); c <= 8; c++ {
			body := reflect.New(field.Type().Elem())
			code := body.Elem().FieldByName("Code")
			if !code.Addr().Interface().(interface{ ValidEnum(v int32) bool }).ValidEnum(c) {
				continue
			}
			seen++
			code.SetInt(int64(c))
			field.Set(body)
			got, ok := opInnerCodeSlug(tr)
			// The regex also rejects the "<type>_unknown(<n>)" fallback.
			if !ok || !slug.MatchString(got) {
				t.Errorf("op type %s code %d named (%q, %v), want a snake_case slug",
					xdr.OperationType(ot), c, got, ok)
			}
		}
	}
	if seen < 150 {
		t.Fatalf("only %d op-type result codes walked; probe window too narrow?", seen)
	}
}
