// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import "sync/atomic"

// installedMovementsFloor overrides [SEP41MovementsFloorLedger] process-wide (zero = not installed); written once at start-up by
// [InstallMovementsFloor], the canonical.InstallAliasRegistry idiom for a network-derived value with no config in scope.
var installedMovementsFloor atomic.Uint32

// InstallMovementsFloor publishes floor as the process-wide movements boundary [MovementsFloor] resolves to; call once at start-up after config load, and floor==0 means not installed so a mis-wired config never zeroes the boundary.
func InstallMovementsFloor(floor uint32) { installedMovementsFloor.Store(floor) }

// MovementsFloor returns the installed boundary, or the pubnet default [SEP41MovementsFloorLedger] when none is installed; use it instead of the const on /movements read paths.
func MovementsFloor() uint32 {
	if f := installedMovementsFloor.Load(); f != 0 {
		return f
	}
	return SEP41MovementsFloorLedger
}
