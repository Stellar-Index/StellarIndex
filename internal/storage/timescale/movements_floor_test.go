// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMovementsFloor_DefaultsToPubnetConst: with nothing installed (unit
// tests, leaf callers) MovementsFloor resolves to the pubnet const — the
// pre-network-abstraction behaviour is invariant.
func TestMovementsFloor_DefaultsToPubnetConst(t *testing.T) {
	timescale.InstallMovementsFloor(0) // reset to "not installed"
	t.Cleanup(func() { timescale.InstallMovementsFloor(0) })
	if got := timescale.MovementsFloor(); got != timescale.SEP41MovementsFloorLedger {
		t.Errorf("MovementsFloor() with nothing installed = %d, want const %d",
			got, timescale.SEP41MovementsFloorLedger)
	}
}

// TestMovementsFloor_InstallOverrides: a test net installs genesis (=1)
// and MovementsFloor returns it — the fix that keeps the /movements tail
// from flooring above every ledger a reset test net has.
func TestMovementsFloor_InstallOverrides(t *testing.T) {
	t.Cleanup(func() { timescale.InstallMovementsFloor(0) })
	timescale.InstallMovementsFloor(1)
	if got := timescale.MovementsFloor(); got != 1 {
		t.Errorf("MovementsFloor() after InstallMovementsFloor(1) = %d, want 1", got)
	}
}

// TestMovementsFloor_ZeroIsNoOp: InstallMovementsFloor(0) must NOT zero
// the boundary — 0 means "not installed" and falls back to the const, so
// a mis-wired empty config can never collapse the floor to 0 (which would
// let the PG tail double-count with the CH archive below the boundary).
func TestMovementsFloor_ZeroIsNoOp(t *testing.T) {
	t.Cleanup(func() { timescale.InstallMovementsFloor(0) })
	timescale.InstallMovementsFloor(12345)
	timescale.InstallMovementsFloor(0)
	if got := timescale.MovementsFloor(); got != timescale.SEP41MovementsFloorLedger {
		t.Errorf("MovementsFloor() after InstallMovementsFloor(0) = %d, want const %d (0 must not zero the boundary)",
			got, timescale.SEP41MovementsFloorLedger)
	}
}
