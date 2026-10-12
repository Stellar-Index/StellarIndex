// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import "sync/atomic"

// installedMovementsFloor overrides the pubnet-default [SEP41MovementsFloorLedger] process-wide;
// zero means not installed and the const default holds. It is written at most once at start-up
// before any read path runs ([InstallMovementsFloor]); the atomic keeps that publish race-free.
//
// This is the canonical.InstallAliasRegistry idiom: a network-derived value is needed in leaf
// storage (the /movements read path and [Store.ListSEP41TransfersByAddress]) with no config.Config
// in scope, so it is published once instead of threaded through every signature.
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
