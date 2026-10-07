// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"net/netip"
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
// [Bucket.Charge] on a local bucket always returns a nil error and an
// authoritative allow/deny.
//
// # Memory bound
//
// Stale entries (whose window has rolled over) are swept lazily at most
// once per window — never more often, because a second sweep inside the
// SAME window is provably incapable of freeing anything (every entry
// left after a sweep names the current window, and every entry written
// since names it too). A size-triggered branch would re-run a full O(n) map scan under the global
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
// job is to survive a flood, and already-tracked clients keep their own
// independent counters. Resident memory is bounded to localStoreMaxKeys+1
// entries (a few MB), plus at most as many per-/48 counters.
//
// Anonymous keys resolve to the forge-resistant client IP, masked to /64
// for IPv6. That makes 100k keys only ~1.5 /48 allocations, so a single
// cheap IPv6 holder could fill the cap and push every NEW client into the
// shared bucket. Each IPv6 /48 may therefore insert at most
// [localStoreMaxKeysPer48] keys per window; its further /64s share one
// per-/48 bucket, and filling the cap takes ~100 distinct /48s.
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

	// per48 counts, per window, how many keys each IPv6 /48 (keyed by
	// [slash48Key]) has inserted; maxPer48 caps it. A field for the same
	// test-sizing reason as maxKeys.
	per48    map[string]localEntry
	maxPer48 int

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

// localStoreMaxKeysPer48 caps the keys one IPv6 /48 may insert per window
// before its further /64s share a single per-/48 bucket.
const localStoreMaxKeysPer48 = 1024

// localPer48BucketTag marks a per-/48 shared bucket key; the NUL byte
// keeps it disjoint from every real /64 key, as with [localOverflowKey].
const localPer48BucketTag = "\x00per48\x00"

// localOverflowKey is the shared bucket every key beyond the cap is
// folded into. The NUL bytes make it unreachable as a real limiter key
// (callers pass IPs, API-key hashes and `<prefix>:<value>` strings), so
// a client can never aim itself at the overflow bucket to dodge its own
// counter — and could not gain anything if it did, the overflow budget
// being strictly smaller than a private one.
const localOverflowKey = "\x00overflow\x00"

func newLocalStore() *localStore {
	return &localStore{
		entries:  make(map[string]localEntry),
		maxKeys:  localStoreMaxKeys,
		per48:    make(map[string]localEntry),
		maxPer48: localStoreMaxKeysPer48,
	}
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
	if !tracked {
		key = s.admitLocked(key, window)
		e = s.entries[key]
	}
	if e.window != window {
		e = localEntry{window: window, count: 0}
	}
	e.count++
	s.entries[key] = e
	return e.count, e.count <= limit
}

// admitLocked picks the bucket an untracked key is counted against: its
// own, its /48's shared bucket once that /48 has used its per-window
// allowance, or [localOverflowKey] once the map is full. Caller holds s.mu.
func (s *localStore) admitLocked(key string, window int64) string {
	p48, isV6 := slash48Key(key)
	if isV6 {
		c := s.per48[p48]
		if c.window != window {
			c = localEntry{window: window}
		}
		if c.count >= s.maxPer48 {
			key = localPer48BucketTag + p48
		} else if len(s.entries) < s.maxKeys {
			c.count++
			s.per48[p48] = c
		}
	}
	if _, tracked := s.entries[key]; !tracked && len(s.entries) >= s.maxKeys {
		return localOverflowKey
	}
	return key
}

// slash48Key returns key with a trailing IPv6 address masked to its /48,
// keeping any caller prefix ("anon:", "ip:") so namespaces stay disjoint.
// The first parseable suffix is the longest, i.e. the whole address; a
// hex-looking prefix can only widen the grouping, never narrow it.
func slash48Key(key string) (string, bool) {
	for i := 0; i < len(key); i++ {
		if i > 0 && key[i-1] != ':' {
			continue
		}
		addr, err := netip.ParseAddr(key[i:])
		if err != nil {
			continue
		}
		if !addr.Is6() || addr.Is4In6() {
			return "", false
		}
		return key[:i] + netip.PrefixFrom(addr.WithZone(""), 48).Masked().Addr().String(), true
	}
	return "", false
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
	for k, c := range s.per48 {
		if c.window < current {
			delete(s.per48, k)
		}
	}
	s.lastGC = now
	s.lastSweptWindow = current
	s.sweeps++
}
