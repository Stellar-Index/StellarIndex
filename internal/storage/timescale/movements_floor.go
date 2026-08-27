// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import "sync/atomic"

// installedMovementsFloor overrides the pubnet-default
// [SEP41MovementsFloorLedger] process-wide. Zero means "not installed" →
// the const default is used. Written at most once, at binary start-up,
// before any read path runs (see [InstallMovementsFloor]); the atomic
// makes that publish race-free (go test -race) and lets tests set/reset
// it without a lock.
//
// This is the canonical.InstallAliasRegistry idiom applied to the P23
// boundary: a NETWORK-derived value is needed in a leaf-storage position
// (the /movements read path + [Store.ListSEP41TransfersByAddress]) that
// has no config.Config in scope, so it is published once at start-up
// rather than threaded through every signature.
var installedMovementsFloor atomic.Uint32

// InstallMovementsFloor publishes floor as the process-wide P23 / CAP-67
// movements boundary that [MovementsFloor] — and every /movements read
// path keyed off it — resolves to. Call it ONCE, at binary start-up,
// after config load and before serving, from
// cfg.Stellar.MovementsFloorLedger:
//
//   - pubnet installs the const value (a no-op — identical behaviour).
//   - testnet/futurenet install 1 (genesis), because a reset test net is
//     entirely post-P23 and the pubnet boundary would floor the tail
//     ABOVE every ledger the net has, emptying the movements feed.
//
// floor==0 is treated as "not installed" (the const default holds), so a
// mis-wired empty config never zeroes the boundary to a bogus value.
func InstallMovementsFloor(floor uint32) { installedMovementsFloor.Store(floor) }

// MovementsFloor returns the installed boundary, or the pubnet default
// [SEP41MovementsFloorLedger] when none is installed (unit tests, leaf
// callers that never install). It is the network-aware replacement for
// reading SEP41MovementsFloorLedger directly on the /movements read path.
func MovementsFloor() uint32 {
	if f := installedMovementsFloor.Load(); f != 0 {
		return f
	}
	return SEP41MovementsFloorLedger
}
