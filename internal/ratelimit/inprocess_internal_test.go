// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

// TestLocalStore_SweepsAtMostOncePerWindow pins that invariant.
//
// The attack it encodes: hold the fallback limiter above its key cap
// inside a single window (a distinct-key flood is the *only* way to get
// there, i.e. exactly when the limiter matters most). A size-triggered branch
// that re-ran a full O(n) scan of the map under the
// global mutex on EVERY subsequent take would do so — a scan that provably deletes
// nothing, because every surviving entry names the current window — so
// each request paid for a walk of every other client's counter and the
// limiter became the cheapest way to stall the process.
//
// A window's scan must happen once. The counter asserts the exact
// number, not "few".
func TestLocalStore_SweepsAtMostOncePerWindow(t *testing.T) {
	const windowDur = time.Minute
	now := time.Unix(1_700_000_000, 0)
	window := now.Unix() / int64(windowDur.Seconds())

	s := newLocalStore()
	s.maxKeys = 4 // drive the at-capacity path without 100k allocations

	// Fill to capacity, then keep hammering distinct keys in the SAME
	// window — the shape of a distinct-key flood.
	for i := 0; i < 200; i++ {
		s.take("flood-"+strconv.Itoa(i), window, 1, now, windowDur)
	}
	if s.sweeps != 1 {
		t.Fatalf("sweeps = %d after 200 same-window takes, want exactly 1 "+
			"(a second scan inside one window can free nothing — CON-04)", s.sweeps)
	}

	// The next window legitimately gets one sweep, which reclaims every
	// entry from the previous one.
	next := now.Add(windowDur)
	s.take("after-rollover", next.Unix()/int64(windowDur.Seconds()), 1, next, windowDur)
	if s.sweeps != 2 {
		t.Fatalf("sweeps = %d after window rollover, want 2 (one per window)", s.sweeps)
	}
	if len(s.entries) != 1 {
		t.Errorf("entries = %d after rollover sweep, want 1 (stale window reclaimed)", len(s.entries))
	}
}

// TestLocalStore_OverflowIsBoundedAndFailsClosed pins the memory
// bound at the unit level: past the cap the map does not grow, and the
// overflow population shares one fail-CLOSED budget rather than each
// flood key minting a private one. The exported-API twin of this test
// (TestInProcess_FloodBeyondKeyCap_SharesFailClosedBucket) drives the
// real 100k cap.
func TestLocalStore_OverflowIsBoundedAndFailsClosed(t *testing.T) {
	const windowDur = time.Minute
	now := time.Unix(1_700_000_000, 0)
	window := now.Unix() / int64(windowDur.Seconds())

	s := newLocalStore()
	s.maxKeys = 3

	for i := 0; i < 3; i++ {
		if _, allowed := s.take("tracked-"+strconv.Itoa(i), window, 1, now, windowDur); !allowed {
			t.Fatalf("tracked key %d should be within a limit of 1 on first touch", i)
		}
	}

	// First overflow key gets the shared bucket's first slot.
	if _, allowed := s.take("flood-a", window, 1, now, windowDur); !allowed {
		t.Error("first overflow key should be allowed (shared bucket count 1, limit 1)")
	}
	// Every further overflow key shares that bucket — fail-closed.
	for _, k := range []string{"flood-b", "flood-c", "flood-d"} {
		if _, allowed := s.take(k, window, 1, now, windowDur); allowed {
			t.Errorf("overflow key %q must be DENIED — past the cap the flood shares one budget", k)
		}
	}
	if got := len(s.entries); got != 4 { // 3 tracked + 1 overflow bucket
		t.Errorf("entries = %d, want 4 (maxKeys 3 + the single overflow bucket)", got)
	}

	// Collateral check: a key tracked before the cap keeps its OWN
	// counter and is unaffected by the flood.
	if _, allowed := s.take("tracked-0", window, 2, now, windowDur); !allowed {
		t.Error("a key tracked before the cap must keep its private counter")
	}
}

// TestLocalStore_OneIPv6AllocationCannotFillTheKeyCap: anonymous IPv6 keys
// are /64 prefixes, so one /48 holder can mint 65,536 distinct keys. It must
// not be able to fill the cap and fold every new client into the shared
// overflow bucket.
func TestLocalStore_OneIPv6AllocationCannotFillTheKeyCap(t *testing.T) {
	const windowDur = time.Minute
	now := time.Unix(1_700_000_000, 0)
	window := now.Unix() / int64(windowDur.Seconds())

	s := newLocalStore()
	s.maxKeys = 8
	s.maxPer48 = 2

	for i := 0; i < 200; i++ {
		s.take("anon:2001:db8:1:"+strconv.FormatInt(int64(i), 16)+"::", window, 1, now, windowDur)
	}
	if got := len(s.entries); got != 3 { // 2 private /64s + the /48's shared bucket
		t.Fatalf("entries = %d after a one-/48 flood, want 3", got)
	}

	for _, k := range []string{"anon:192.0.2.1", "anon:192.0.2.2", "anon:2001:db8:2:1::", "anon:2001:db8:3:1::"} {
		if _, allowed := s.take(k, window, 1, now, windowDur); !allowed {
			t.Errorf("new client %q denied: a single /48 must not exhaust other clients' budgets", k)
		}
	}
	// The flooding /48's further /64s stay fail-closed in their shared bucket.
	if _, allowed := s.take("anon:2001:db8:1:ffff::", window, 1, now, windowDur); allowed {
		t.Error("a fresh /64 from the flooding /48 must share that /48's exhausted bucket")
	}

	// The per-/48 allowance is per window and is swept with the entries.
	next := now.Add(windowDur)
	s.take("anon:2001:db8:1:ffff::", next.Unix()/int64(windowDur.Seconds()), 1, next, windowDur)
	if got := len(s.per48); got != 1 {
		t.Errorf("per48 = %d after rollover, want 1 (stale window reclaimed)", got)
	}
}

func TestSlash48Key(t *testing.T) {
	for _, tc := range []struct {
		key, want string
		ok        bool
	}{
		{"anon:2001:db8:1:2::", "anon:2001:db8:1::", true},
		{"2001:db8:1:2::", "2001:db8:1::", true},
		{"rl:ip:2001:db8:abcd:ef01::", "rl:ip:2001:db8:abcd::", true},
		{"anon:192.0.2.1", "", false},
		{"anon:::ffff:192.0.2.1", "", false},
		{"anon", "", false},
		{"flood-1", "", false},
		{"mail:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "", false},
	} {
		got, ok := slash48Key(tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("slash48Key(%q) = (%q, %v), want (%q, %v)", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}
