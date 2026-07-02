// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import "time"

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
//     range.
//
// One copy each for the three oracle decoders (reflector / band /
// redstone) that previously hand-rolled this guard (D3 cluster 9).
func SafeUnixSeconds(raw uint64, closedAt time.Time) time.Time {
	maxSeconds := uint64(closedAt.Add(SafeUnixFutureWindow).Unix())
	if raw < safeUnixEpochFloorSeconds || raw > maxSeconds {
		return closedAt.UTC()
	}
	return time.Unix(int64(raw), 0).UTC()
}

// SafeUnixMillis is [SafeUnixSeconds] for raw u64 UNIX-milliseconds
// timestamps (Reflector topic[2], Redstone PackageTimestamp).
func SafeUnixMillis(raw uint64, closedAt time.Time) time.Time {
	maxMillis := uint64(closedAt.Add(SafeUnixFutureWindow).UnixMilli())
	if raw < safeUnixEpochFloorSeconds*1000 || raw > maxMillis {
		return closedAt.UTC()
	}
	return time.UnixMilli(int64(raw)).UTC()
}
