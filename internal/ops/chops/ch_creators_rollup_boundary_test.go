package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestValidateCreatorsBoundary: the boundary is rejected only when the
// chain has not reached it. Test nets legitimately set it to 1, below the
// lake's first ledger (2).
func TestValidateCreatorsBoundary(t *testing.T) {
	const (
		testnetTip = 1_500_000
		pubnetTip  = 63_000_000
	)
	for _, tc := range []struct {
		name              string
		boundary, lakeTip uint32
		wantErr           bool
	}{
		{"pubnet boundary on a test net that never reached it", clickhouse.P23BoundaryLedger, testnetTip, true},
		{"boundary one past the tip", testnetTip + 1, testnetTip, true},
		{"test-net setting movements_floor_ledger=1", 1, testnetTip, false},
		{"boundary equal to the lake's first ledger", 2, testnetTip, false},
		{"boundary equal to the tip", testnetTip, testnetTip, false},
		{"pubnet boundary on pubnet", clickhouse.P23BoundaryLedger, pubnetTip, false},
		{"empty lake has nothing to split", clickhouse.P23BoundaryLedger, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCreatorsBoundary(tc.boundary, tc.lakeTip)
			if tc.wantErr && err == nil {
				t.Fatalf("validateCreatorsBoundary(boundary=%d, lakeTip=%d) = nil, want an error",
					tc.boundary, tc.lakeTip)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateCreatorsBoundary(boundary=%d, lakeTip=%d) = %v, want nil",
					tc.boundary, tc.lakeTip, err)
			}
		})
	}
}
