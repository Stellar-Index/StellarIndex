// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"math"
	"time"
)

// SafeUnixFutureWindow bounds how far past the ledger close a decoded
// on-chain timestamp may sit before [SafeUnixSeconds] /
// [SafeUnixMillis] treat it as garbage and fall back to the ledger
// close time. Absorbs relayer/oracle clock skew without admitting
// sentinel / overflow values that error the timestamptz INSERT
// (cf. the soroswap-router deadline_ts fix).
const SafeUnixFutureWindow = 24 * time.Hour

// safeUnixEpochFloorSeconds is the lower sanity bound for a decoded
// raw timestamp: 1_000_000_000 s = 2001-09-09. Anything before it
// (0 / sentinel / pre-epoch garbage) falls back to the ledger close —
// every oracle/DeFi source we ingest launched well after 2001.
const safeUnixEpochFloorSeconds = 1_000_000_000

// SafeUnixSeconds converts a raw u64 UNIX-seconds timestamp (as
// decoded from contract events / op args) to a UTC time, falling back
// to closedAt when the value is outside the sane window
// [2001-09-09, closedAt+SafeUnixFutureWindow].
//
// The bound check happens on the RAW u64, BEFORE the int64 cast:
//   - too small (0 / pre-2001) → bogus old timestamp.
//   - too large (> close+24h) → far-future sentinel; and crucially
//     anything > math.MaxInt64 (~9.2e18) WRAPS NEGATIVE in an int64()
//     cast and would stamp a far-PAST time that a cast-first
//     future-only After() guard misses in both directions — the same
//     overflow class as the router deadline_ts bug. Bound-checking the
//     raw u64 first catches both ends and keeps the cast provably in
//     range. The ceiling itself is int64-checked before its own uint64
//     cast — a pre-1970 closedAt would otherwise wrap it and disable the
//     whole guard (cold audit 2026-08-04).
//
// One copy each for the three oracle decoders (reflector / band /
// redstone) that previously hand-rolled this guard (D3 cluster 9).
func SafeUnixSeconds(raw uint64, closedAt time.Time) time.Time {
	ceil := closedAt.Add(SafeUnixFutureWindow).Unix()
	if ceil < 0 {
		// closedAt is pre-1970, so the ceiling is negative and an
		// unchecked uint64() cast would wrap it to ~1.8e19 — silently
		// disabling the guard this function exists to be. Every raw value
		// including 2^63 would then pass and int64(raw) would stamp a
		// far-PAST time, the exact overflow class the router deadline_ts
		// bug was. No production caller can reach it today (all three
		// LedgerClosedAt producers are >=1970 and all three call sites
		// fail closed on a missing close time), but the guard must not
		// depend on that (cold audit 2026-08-04).
		return closedAt.UTC()
	}
	if raw < safeUnixEpochFloorSeconds || raw > uint64(ceil) {
		return closedAt.UTC()
	}
	return time.Unix(int64(raw), 0).UTC()
}

// UnboundedUnixSeconds converts a raw u64 UNIX-seconds value to a UTC
// time WITHOUT clamping it to a close-time window, for fields that are
// legitimately far in the future relative to the ledger they were
// observed on — a swap/router deadline or a timelock unlock time — where
// [SafeUnixSeconds]'s tight ceiling would wrongly clamp a real value.
//
// It only guards the int64 cast itself: a raw value above math.MaxInt64
// wraps NEGATIVE and, for raw values near math.MaxUint64 specifically,
// wraps to a small negative number — a bogus time near the 1970 epoch
// that reads as perfectly plausible downstream and is NOT caught by a
// postgres-timestamptz-range check (unlike a wrap deep into the past,
// which lands billions of years before 4713 BC and is caught that way).
// ok is false for any raw value that would wrap; callers should treat
// that the same as an already-absent value (leave the field unset).
func UnboundedUnixSeconds(raw uint64) (t time.Time, ok bool) {
	if raw > uint64(math.MaxInt64) {
		return time.Time{}, false
	}
	return time.Unix(int64(raw), 0).UTC(), true
}

// SafeUnixMillis is [SafeUnixSeconds] for raw u64 UNIX-milliseconds
// timestamps (Reflector topic[2], Redstone PackageTimestamp).
func SafeUnixMillis(raw uint64, closedAt time.Time) time.Time {
	ceil := closedAt.Add(SafeUnixFutureWindow).UnixMilli()
	if ceil < 0 {
		return closedAt.UTC() // see SafeUnixSeconds
	}
	if raw < safeUnixEpochFloorSeconds*1000 || raw > uint64(ceil) {
		return closedAt.UTC()
	}
	return time.UnixMilli(int64(raw)).UTC()
}
