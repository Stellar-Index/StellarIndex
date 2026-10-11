package xdrjson_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// TestOpTypeNameFromEnumString_MatchesOpTypeName: the lake-string vocabulary (directory and
// op_type_stats) must give the same snake_case name OpTypeName gives the typed enum, for every
// known xdr.OperationType. A lowercase-and-strip-prefix fallback would diverge on multi-word
// types (invokehostfunction vs invoke_host_function).
func TestOpTypeNameFromEnumString_MatchesOpTypeName(t *testing.T) {
	for i := int32(0); i <= int32(xdr.OperationTypeRestoreFootprint); i++ {
		opType := xdr.OperationType(i)
		want := xdrjson.OpTypeName(opType)
		got := xdrjson.OpTypeNameFromEnumString(opType.String())
		if got != want {
			t.Errorf("OpTypeNameFromEnumString(%q) = %q, want %q (OpTypeName(%v))",
				opType.String(), got, want, opType)
		}
	}
}
