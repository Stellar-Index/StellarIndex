package explorer

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// TestOpViewLight_UsesControlledOpTypeVocabulary pins GH-1136: the directory
// listing (opViewLight, backing /v1/operations and the ledger-scoped
// listings) must serve the same snake_case op-type vocabulary as the
// decoded happy path, not the naive lowercase-and-strip-prefix fallback.
// Before the fix a multi-word lake type like "OperationTypeManageSellOffer"
// came out as "manageselloffer" here while xdrjson.OpTypeName gave
// "manage_sell_offer" everywhere else.
func TestOpViewLight_UsesControlledOpTypeVocabulary(t *testing.T) {
	row := clickhouse.OpRow{OpType: "OperationTypeManageSellOffer"}
	got := opViewLight(row).Type
	want := xdrjson.OpTypeNameFromEnumString("OperationTypeManageSellOffer")
	if got != want {
		t.Fatalf("opViewLight.Type = %q, want %q", got, want)
	}
	if got != "manage_sell_offer" {
		t.Fatalf("opViewLight.Type = %q, want the controlled vocabulary value %q", got, "manage_sell_offer")
	}
}
