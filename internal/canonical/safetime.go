// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"math"
	"time"
)

// SafeUnixFutureWindow bounds how far past the ledger close a decoded timestamp may sit before [SafeUnixSeconds] / [SafeUnixMillis] fall back to the close time.
const SafeUnixFutureWindow = 24 * time.Hour

// safeUnixEpochFloorSeconds (1e9 s, September 2001) is the lower sanity bound: every ingested
// source launched later, so anything earlier is a 0 or sentinel and falls back to the close.
const safeUnixEpochFloorSeconds = 1_000_000_000

// SafeUnixSeconds converts a raw u64 UNIX-seconds timestamp to UTC, falling back to closedAt outside [1e9 s, closedAt+SafeUnixFutureWindow], bound-checking the RAW u64 before the int64 cast.
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

// UnboundedUnixSeconds converts a raw u64 UNIX-seconds value to UTC without a close-time window, guarding only the int64 cast; ok=false means treat the field as absent.
func UnboundedUnixSeconds(raw uint64) (t time.Time, ok bool) {
	if raw > uint64(math.MaxInt64) {
		return time.Time{}, false
	}
	return time.Unix(int64(raw), 0).UTC(), true
}

// SafeUnixMillis is [SafeUnixSeconds] for raw u64 UNIX-millisecond timestamps.
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
