package chops

import "testing"

// TestValidateCreatorsBoundary pins RLT-191: creatorsBoundary previously
// returned a configured or fallback ledger with no sanity check at all, so
// a mistyped or stale stellar.movements_floor_ledger silently mis-split
// the classic/CAP-67 creation arms instead of erroring — the classic arm
// scans nothing below a boundary the lake never reaches.
func TestValidateCreatorsBoundary(t *testing.T) {
	for _, tc := range []struct {
		name              string
		boundary, lakeMin uint32
		wantErr           bool
	}{
		{"boundary below the lake's first ledger is rejected", 1, 2, true},
		{"pubnet boundary against pubnet's populated lake is fine", 58_762_517, 2, false},
		{"boundary equal to the lake's first ledger is fine", 2, 2, false},
		{"boundary above the lake's first ledger is fine", 4_500_000, 2, false},
		{"empty lake (lakeMin=0) skips the check", 0, 0, false},
		{"zero boundary against a real lake is rejected", 0, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCreatorsBoundary(tc.boundary, tc.lakeMin)
			if tc.wantErr && err == nil {
				t.Fatalf("validateCreatorsBoundary(boundary=%d, lakeMin=%d) = nil, want an error",
					tc.boundary, tc.lakeMin)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateCreatorsBoundary(boundary=%d, lakeMin=%d) = %v, want nil",
					tc.boundary, tc.lakeMin, err)
			}
		})
	}
}
