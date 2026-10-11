// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"math"
	"time"
)

// SafeUnixFutureWindow bounds how far past the ledger close a decoded timestamp may sit before
// [SafeUnixSeconds] / [SafeUnixMillis] fall back to the close time. It absorbs clock skew without
// admitting sentinel or overflow values that error the timestamptz INSERT.
const SafeUnixFutureWindow = 24 * time.Hour

// safeUnixEpochFloorSeconds (1e9 s, September 2001) is the lower sanity bound: every ingested
// source launched later, so anything earlier is a 0 or sentinel and falls back to the close.
const safeUnixEpochFloorSeconds = 1_000_000_000

// SafeUnixSeconds converts a raw u64 UNIX-seconds timestamp to UTC, falling back to closedAt
// outside [1e9 s, closedAt+SafeUnixFutureWindow].
//
// The bound check runs on the RAW u64, BEFORE the int64 cast: a value above math.MaxInt64 wraps
// negative and would stamp a far-PAST time that a cast-first future-only guard misses. The
// ceiling is int64-checked before its own uint64 cast, since a pre-1970 closedAt would wrap it
// and disable the guard. Decoders share this rather than hand-rolling one; see
// [SafeUnixMillis].
func SafeUnixSeconds(raw uint64, closedAt time.Time) time.Time {
	ceil := closedAt.Add(SafeUnixFutureWindow).Unix()
	if ceil < 0 {
		// A pre-1970 closedAt makes the ceiling negative; an unchecked uint64() cast would wrap
		// it to ~1.8e19, letting every raw value through to stamp a far-PAST time. No production
		// caller reaches this, but the guard must not depend on that.
		return closedAt.UTC()
	}
	if raw < safeUnixEpochFloorSeconds || raw > uint64(ceil) {
		return closedAt.UTC()
	}
	return time.Unix(int64(raw), 0).UTC()
}

// UnboundedUnixSeconds converts a raw u64 UNIX-seconds value to UTC WITHOUT a close-time window,
// for fields legitimately far in the future (a swap deadline, a timelock unlock) that
// [SafeUnixSeconds]'s ceiling would wrongly clamp.
//
// It only guards the int64 cast: a value near math.MaxUint64 wraps to a small negative, a bogus
// time near 1970 that looks plausible and that the timestamptz range check does not catch.
// ok is false for any value that would wrap; treat it as absent and leave the field unset.
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
