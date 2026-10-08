package xdrjson_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// TestOpTypeNameFromEnumString_MatchesOpTypeName: the lake-string
// vocabulary (OpTypeNameFromEnumString, used by the /v1/operations directory
// and op_type_stats) must produce the identical snake_case name OpTypeName
// gives the typed enum (used by the ledger arm, /v1/tx and account ops) for
// every known xdr.OperationType — one schema, one vocabulary. A naive
// lowercase-and-strip-prefix fallback on the directory/stats path would diverge for multi-word types
// (invokehostfunction vs invoke_host_function, manageselloffer vs
// manage_sell_offer).
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
