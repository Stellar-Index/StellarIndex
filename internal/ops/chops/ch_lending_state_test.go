package chops

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestCheckLendingRegistry_RefusesAMissingKnownPool(t *testing.T) {
	fixed := clickhouse.LendingContract{ID: blnt.MainnetFixedPool, Protocol: clickhouse.LendingBLNT}
	yield := clickhouse.LendingContract{ID: blnt.MainnetYieldBlentPool, Protocol: clickhouse.LendingBLNT}
	if err := checkLendingRegistry([]clickhouse.LendingContract{fixed, yield}, 70_000_000); err != nil {
		t.Fatalf("complete registry refused: %v", err)
	}
	err := checkLendingRegistry([]clickhouse.LendingContract{fixed}, 70_000_000)
	if err == nil || !strings.Contains(err.Error(), blnt.MainnetYieldBlentPool) {
		t.Fatalf("registry without the yield pool: err = %v, want it named", err)
	}
	// A pool deployed after the registry's ledger is not required yet.
	if err := checkLendingRegistry([]clickhouse.LendingContract{fixed}, 65_200_000); err != nil {
		t.Fatalf("registry before the yield pool's deploy refused: %v", err)
	}
	// Listed under the wrong protocol, or only as a backstop, is still missing.
	wrong := clickhouse.LendingContract{ID: blnt.MainnetYieldBlentPool, Protocol: clickhouse.LendingBlendV2}
	if err := checkLendingRegistry([]clickhouse.LendingContract{fixed, wrong}, 70_000_000); err == nil {
		t.Fatal("yield pool under blend_v2 accepted")
	}
}
