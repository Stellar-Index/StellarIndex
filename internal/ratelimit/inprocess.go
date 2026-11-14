// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"net/netip"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// localStore is the in-process fixed-window counter behind a [Bucket] with a nil Redis client: the fail-CLOSED fallback for a boot
// without Redis. Semantics match the Redis path but it can never fail open.
//
// Memory bound: stale entries are swept lazily, once per window (a size-triggered sweep would rescan O(n) under the global mutex).
// At [localStoreMaxKeys] an untracked address key shares a fail-CLOSED bucket with its region (IPv4 /16, IPv6 /32), so a flood
// that fills the map throttles its own regions and not every new client; any other key, and an address once
// [localStoreMaxRegions] region buckets exist, goes to ONE shared overflow bucket ([localOverflowKey]). IPv6 keys are masked to
// /64, so each /48 may insert at most [localStoreMaxKeysPer48] keys per window, then shares a per-/48 bucket.
type localStore struct {
	mu      sync.Mutex
	entries map[string]localEntry
	lastGC  time.Time

	// lastSweptWindow is the window gcLocked last scanned; a second scan at the same value can
	// only re-walk live entries, so it is skipped.
	lastSweptWindow int64

	// maxKeys caps tracked entries. A field, not a const, so tests can drive the overflow path
	// at a small size instead of allocating 100k entries.
	maxKeys int

	// per48 counts, per window, the keys each IPv6 /48 (see [slash48Key]) has inserted;
	// maxPer48 caps it. A field for the same test-sizing reason as maxKeys.
	per48    map[string]localEntry
	maxPer48 int

	// maxRegions caps the region buckets admitted past maxKeys, so the map stays bounded.
	maxRegions int

	// sweeps counts full scans so a test can pin "at most one sweep per window".
	sweeps int
}

// localEntry is one key's counter for the window it names.
type localEntry struct {
	window int64
	count  int
}

// localStoreMaxKeys caps distinct tracked keys; past it new keys share [localOverflowKey].
const localStoreMaxKeys = 100_000

// localStoreMaxKeysPer48 caps the keys one IPv6 /48 may insert per window.
const localStoreMaxKeysPer48 = 1024

// localStoreMaxRegions caps the region buckets a full map may still add.
const localStoreMaxRegions = 10_000

// localRegionBucketTag marks a region's shared bucket; NUL keeps it disjoint from real keys.
const localRegionBucketTag = "\x00region\x00"

// localPer48BucketTag marks a per-/48 shared bucket; NUL keeps it disjoint from real keys.
const localPer48BucketTag = "\x00per48\x00"

// localOverflowKey is the shared bucket every key beyond the cap folds into. Its NUL bytes make it unreachable as a real limiter
// key, so a client cannot target it (and gains nothing: the overflow budget is smaller than a private one).
const localOverflowKey = "\x00overflow\x00"

func newLocalStore() *localStore {
	return &localStore{
		entries:    make(map[string]localEntry),
		maxKeys:    localStoreMaxKeys,
		per48:      make(map[string]localEntry),
		maxPer48:   localStoreMaxKeysPer48,
		maxRegions: localStoreMaxRegions,
	}
}

// take increments key's counter for the named window and reports the post-increment count and whether it fits within max. An entry
// naming an older window is reset (the FIXED-window rollover). An untracked key at capacity counts against the overflow bucket.
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

// admitLocked picks the bucket for an untracked key: its own, its /48's shared bucket once that
// /48 is out of allowance, or once the map is full its region's bucket, else [localOverflowKey]. Caller holds s.mu.
func (s *localStore) admitLocked(key string, window int64) string {
	orig := key
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
	if _, tracked := s.entries[key]; tracked || len(s.entries) < s.maxKeys {
		return key
	}
	obs.RateLimitInProcessOverflowTotal.Inc()
	// From the caller's key: the /48 tag would give one /32 a second region bucket.
	if region, ok := regionKey(orig); ok {
		bucket := localRegionBucketTag + region
		if _, tracked := s.entries[bucket]; tracked || len(s.entries) < s.maxKeys+s.maxRegions {
			return bucket
		}
	}
	return localOverflowKey
}

// trailingAddr finds the IP address a key ends with, after any caller prefix ("anon:", "ip:").
func trailingAddr(key string) (start int, addr netip.Addr, ok bool) {
	for i := 0; i < len(key); i++ {
		if i > 0 && key[i-1] != ':' {
			continue
		}
		if a, err := netip.ParseAddr(key[i:]); err == nil {
			return i, a.WithZone(""), true
		}
	}
	return 0, netip.Addr{}, false
}

// regionKey masks a trailing address to the block one network operator typically holds: /16 for IPv4, /32 for IPv6.
func regionKey(key string) (string, bool) {
	i, addr, ok := trailingAddr(key)
	if !ok {
		return "", false
	}
	bits := 32
	if addr = addr.Unmap(); addr.Is4() {
		bits = 16
	}
	return key[:i] + netip.PrefixFrom(addr, bits).Masked().String(), true
}

// slash48Key masks a trailing IPv6 address in key to its /48, keeping any caller prefix ("anon:",
// "ip:") so namespaces stay disjoint. A hex-looking prefix can only widen the grouping.
func slash48Key(key string) (string, bool) {
	i, addr, ok := trailingAddr(key)
	if !ok || !addr.Is6() || addr.Is4In6() {
		return "", false
	}
	return key[:i] + netip.PrefixFrom(addr, 48).Masked().Addr().String(), true
}

// gcLocked deletes entries from windows strictly older than current. Caller holds s.mu. It runs at most once per window (after a
// scan every survivor names the current window, so a rescan is an O(n) waste), or immediately at capacity.
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
