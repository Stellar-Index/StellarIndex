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
// Stale entries (whose window has rolled over) are swept lazily: at
// most once per window under normal load, or immediately once the map
// grows past [localStoreMaxKeys]. So resident memory is bounded to the
// distinct keys seen in the CURRENT window (plus at most one window of
// lag). Anonymous keys resolve to the forge-resistant client IP, so
// distinct keys track distinct real clients — not attacker-rotatable
// values. Under an extreme distinct-IP flood the current window's
// entries cannot be swept (they are still live), but each key is a few
// dozen bytes and every key remains individually limited; the fallback
// degrades gracefully without ever falling open.
type localStore struct {
	mu      sync.Mutex
	entries map[string]localEntry
	lastGC  time.Time
}

// localEntry is one key's counter for the window it names.
type localEntry struct {
	window int64
	count  int
}

// localStoreMaxKeys forces a stale-entry sweep once the map grows past
// this size even if a full window hasn't elapsed since the last sweep.
// A backstop against unbounded growth; the common case sweeps on the
// per-window cadence below.
const localStoreMaxKeys = 100_000

func newLocalStore() *localStore {
	return &localStore{entries: make(map[string]localEntry)}
}

// take increments key's counter for the named window and reports the
// post-increment count plus whether it fits within max. window is the
// caller's `unix / window_seconds` bucket; a key whose stored entry
// names an older window is reset to the new one (that is the
// window-rollover that makes this a FIXED-window limiter).
func (s *localStore) take(key string, window int64, limit int, now time.Time, windowDur time.Duration) (count int, allowed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.gcLocked(window, now, windowDur)

	e := s.entries[key]
	if e.window != window {
		e = localEntry{window: window, count: 0}
	}
	e.count++
	s.entries[key] = e
	return e.count, e.count <= limit
}

// gcLocked deletes entries belonging to a window strictly older than
// current. Runs at most once per window (nowFn - lastGC >= windowDur)
// unless the map has grown past [localStoreMaxKeys], in which case it
// sweeps immediately. Current-window entries are always retained — they
// are the live counters. Caller holds s.mu.
func (s *localStore) gcLocked(current int64, now time.Time, windowDur time.Duration) {
	if len(s.entries) < localStoreMaxKeys && now.Sub(s.lastGC) < windowDur {
		return
	}
	for k, e := range s.entries {
		if e.window < current {
			delete(s.entries, k)
		}
	}
	s.lastGC = now
}
