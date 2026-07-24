// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"sync"
	"time"
)

// localStore is the in-process fixed-window counter that backs a
// [Bucket] constructed with a nil Redis client. It is the fail-CLOSED
// fallback for the C3-13 / C3-22 gap: when Redis is absent at boot the
// old code omitted the rate-limit middleware entirely and the whole
// API ran uncapped (an anonymous flood had no limiter at all). With
// this fallback the anon / key tiers stay enforced — degraded to
// single-instance accounting, which is correct for the R1
// single-instance deployment.
//
// Semantics match [Bucket]'s Redis path: a per-(key × window) integer
// counter, where the window is `unix_seconds / window_seconds`. The
// Nth+1 request inside a window is rejected. Unlike the Redis path it
// can never fail open — there is no backend to error, so
// [Bucket.TakeN] on a local bucket always returns a nil error and an
// authoritative allow/deny.
//
// # Memory bound
//
// Stale entries (whose window has rolled over) are swept lazily at most
// once per window — never more often, because a second sweep inside the
// SAME window is provably incapable of freeing anything (every entry
// left after a sweep names the current window, and every entry written
// since names it too). REL-05 / CON-04 (audit-2026-07-23): the previous
// size-triggered branch re-ran a full O(n) map scan under the global
// mutex on EVERY call once the map passed [localStoreMaxKeys], deleting
// nothing and turning the fallback limiter into a self-inflicted DoS at
// exactly the moment it was under flood.
//
// Growth inside a window is bounded instead by a hard cap: once the map
// holds [localStoreMaxKeys] entries, a key that isn't already tracked is
// routed to ONE shared overflow bucket ([localOverflowKey]) rather than
// inserted. Overflow is therefore fail-CLOSED — past the cap the whole
// overflow population shares a single limit-sized budget for the rest of
// the window — which is the right degradation for a limiter whose only
// job is to survive a flood: >100k distinct keys inside one window on a
// single process IS the flood, and already-tracked clients keep their
// own independent counters. Resident memory is bounded to
// localStoreMaxKeys+1 entries (a few MB), full stop.
//
// Anonymous keys resolve to the forge-resistant client IP, so distinct
// keys track distinct real clients — not attacker-rotatable values.
type localStore struct {
	mu      sync.Mutex
	entries map[string]localEntry
	lastGC  time.Time

	// lastSweptWindow is the window value gcLocked last ran a full scan
	// for. A second scan at the same value can only re-walk live
	// entries, so it is skipped (REL-05 / CON-04).
	lastSweptWindow int64

	// maxKeys is the hard cap on tracked entries; newLocalStore sets it
	// to [localStoreMaxKeys]. A field rather than a bare const so the
	// package's own tests can drive the overflow path at a small size
	// instead of allocating 100k entries.
	maxKeys int

	// sweeps counts completed full scans. Observability for the
	// invariant test that pins "at most one sweep per window" — the
	// property REL-05 / CON-04 is about. Cheap: incremented at most
	// once per window.
	sweeps int
}

// localEntry is one key's counter for the window it names.
type localEntry struct {
	window int64
	count  int
}

// localStoreMaxKeys is the hard cap on distinct tracked keys. Past it,
// new keys share [localOverflowKey]'s bucket instead of growing the map.
const localStoreMaxKeys = 100_000

// localOverflowKey is the shared bucket every key beyond the cap is
// folded into. The NUL bytes make it unreachable as a real limiter key
// (callers pass IPs, API-key hashes and `<prefix>:<value>` strings), so
// a client can never aim itself at the overflow bucket to dodge its own
// counter — and could not gain anything if it did, the overflow budget
// being strictly smaller than a private one.
const localOverflowKey = "\x00overflow\x00"

func newLocalStore() *localStore {
	return &localStore{entries: make(map[string]localEntry), maxKeys: localStoreMaxKeys}
}

// take increments key's counter for the named window and reports the
// post-increment count plus whether it fits within max. window is the
// caller's `unix / window_seconds` bucket; a key whose stored entry
// names an older window is reset to the new one (that is the
// window-rollover that makes this a FIXED-window limiter).
//
// A key that is not already tracked once the map is at capacity is
// counted against the shared overflow bucket — see [localStore].
func (s *localStore) take(key string, window int64, limit int, now time.Time, windowDur time.Duration) (count int, allowed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.gcLocked(window, now, windowDur)

	e, tracked := s.entries[key]
	if !tracked && len(s.entries) >= s.maxKeys {
		key = localOverflowKey
		e = s.entries[key]
	}
	if e.window != window {
		e = localEntry{window: window, count: 0}
	}
	e.count++
	s.entries[key] = e
	return e.count, e.count <= limit
}

// gcLocked deletes entries belonging to a window strictly older than
// current. Current-window entries are always retained — they are the
// live counters. Caller holds s.mu.
//
// Runs at most once per window: after a scan at window W every surviving
// entry names W, and every entry written afterwards names W as well
// (window is derived from the same clock for all callers), so re-scanning
// at W can only walk live entries and delete none — that is exactly the
// wasted O(n)-per-request scan REL-05 / CON-04 flagged. Past that guard
// the sweep runs either on the per-window cadence or immediately when the
// map is at capacity (so a flood's stale entries are reclaimed on the
// first call of the next window rather than a full windowDur later).
func (s *localStore) gcLocked(current int64, now time.Time, windowDur time.Duration) {
	if current == s.lastSweptWindow {
		return // a scan here is provably incapable of freeing anything
	}
	if now.Sub(s.lastGC) < windowDur && len(s.entries) < s.maxKeys {
		return
	}
	for k, e := range s.entries {
		if e.window < current {
			delete(s.entries, k)
		}
	}
	s.lastGC = now
	s.lastSweptWindow = current
	s.sweeps++
}
