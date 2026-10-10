// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMovementsFloor covers the install contract. A test net installs
// genesis (=1) so the /movements tail never floors above every ledger it
// has. 0 means "not installed" and falls back to the pubnet const, so a
// mis-wired empty config can never collapse the floor to 0 (the PG tail
// would double-count with the CH archive below the boundary).
func TestMovementsFloor(t *testing.T) {
	cases := []struct {
		name    string
		install []uint32
		want    uint32
	}{
		{"nothing installed is the pubnet const", nil, timescale.SEP41MovementsFloorLedger},
		{"install overrides", []uint32{1}, 1},
		{"zero is a no-op", []uint32{12345, 0}, timescale.SEP41MovementsFloorLedger},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timescale.InstallMovementsFloor(0)
			t.Cleanup(func() { timescale.InstallMovementsFloor(0) })
			for _, v := range tc.install {
				timescale.InstallMovementsFloor(v)
			}
			if got := timescale.MovementsFloor(); got != tc.want {
				t.Errorf("MovementsFloor() after installing %v = %d, want %d", tc.install, got, tc.want)
			}
		})
	}
}
