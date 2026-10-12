package xdrjson_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// TestOpTypeNameFromEnumString_MatchesOpTypeName: the lake-string vocabulary (directory, op_type_stats) must give the same
// snake_case name as OpTypeName for every known xdr.OperationType; a lowercase-and-strip-prefix fallback would diverge on multi-
// word types.
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
