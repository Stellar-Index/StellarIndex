// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"net/netip"
	"sync"
	"time"
)

// localStore is the in-process fixed-window counter behind a [Bucket] built with a nil Redis
// client: the fail-CLOSED fallback for a boot without Redis, which would otherwise omit the
// rate-limit middleware and leave the API uncapped. Semantics match [Bucket]'s Redis path (the
// Nth+1 request per (key x window) is rejected) but it can never fail open.
//
// # Memory bound
//
// Stale entries are swept lazily, once per window: a second sweep in the SAME window frees
// nothing, and a size-triggered one would rescan O(n) under the global mutex on EVERY call past
// [localStoreMaxKeys].
//
// At [localStoreMaxKeys] entries an untracked key goes to ONE shared fail-CLOSED overflow bucket
// ([localOverflowKey]); tracked clients keep their own. IPv6 keys are masked to /64, so one cheap
// IPv6 holder could fill the cap and starve NEW clients: each /48 may insert at most
// [localStoreMaxKeysPer48] keys per window, then shares a per-/48 bucket.
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

// localPer48BucketTag marks a per-/48 shared bucket; NUL keeps it disjoint from real keys.
const localPer48BucketTag = "\x00per48\x00"

// localOverflowKey is the shared bucket every key beyond the cap folds into. Its NUL bytes make
// it unreachable as a real limiter key (callers pass IPs, key hashes and `<prefix>:<value>`
// strings), so a client cannot target it, and it would gain nothing since the overflow budget is
// smaller than a private one.
const localOverflowKey = "\x00overflow\x00"

func newLocalStore() *localStore {
	return &localStore{
		entries:  make(map[string]localEntry),
		maxKeys:  localStoreMaxKeys,
		per48:    make(map[string]localEntry),
		maxPer48: localStoreMaxKeysPer48,
	}
}

// take increments key's counter for the named window and reports the post-increment count and
// whether it fits within max. window is the caller's `unix / window_seconds` bucket; an entry
// naming an older window is reset, which is the rollover that makes this a FIXED-window limiter.
// An untracked key at capacity counts against the shared overflow bucket (see [localStore]).
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
// /48 is out of allowance, or [localOverflowKey] once the map is full. Caller holds s.mu.
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

// slash48Key masks a trailing IPv6 address in key to its /48, keeping any caller prefix ("anon:",
// "ip:") so namespaces stay disjoint. A hex-looking prefix can only widen the grouping.
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

// gcLocked deletes entries from windows strictly older than current; current-window entries are
// the live counters and are kept. Caller holds s.mu.
//
// It runs at most once per window: after a scan at W every surviving and later-written entry
// names W (all callers share one clock), so a rescan would walk live entries and delete none,
// an O(n)-per-request waste. Past that guard it runs on the per-window cadence, or immediately
// at capacity so a flood's stale entries are reclaimed on the first call of the next window.
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
