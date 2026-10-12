package explorer

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// The directory listing (opViewLight, behind /v1/operations and ledger-scoped listings) must serve the same snake_case
// op-type vocabulary as the decoded path: xdrjson.OpTypeName gives "manage_sell_offer", not a stripped
// "manageselloffer".
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
