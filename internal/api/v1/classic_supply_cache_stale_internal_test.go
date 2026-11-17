// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"
	"time"
)

// A broad map served past its TTL because refresh keeps failing is a stale read, and the listing must say so.
func TestClassicSupplyMap_ExpiredEntryReportsStale(t *testing.T) {
	for name, tc := range map[string]struct {
		age       time.Duration
		wantStale bool
	}{
		"fresh":              {age: time.Minute, wantStale: false},
		"expired, servable":  {age: classicSupplyTTL + time.Minute, wantStale: true},
		"just under max age": {age: classicSupplyMaxAge - time.Minute, wantStale: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := lakeSupplyServer(t, "")
			s.Explorer = failingClassicExplorer{}
			s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
			s.classicSupplyAt = time.Now().Add(-tc.age)
			s.classicSupplyAttemptAt = time.Now() // retry gap open: no refresh can replace the entry
			ctx, rep := withReadReport(context.Background())
			m, unread := s.classicSupplyMap(ctx)
			if m == nil || unread {
				t.Fatalf("map = %v, unread = %v; want the cached map served", m, unread)
			}
			if got := rep.stale.Load(); got != tc.wantStale {
				t.Errorf("stale = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

func TestFillMarketCapsFromSupply_ExpiredBroadMapFlagsStale(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = failingClassicExplorer{}
	s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
	s.classicSupplyAt = time.Now().Add(-(classicSupplyTTL + time.Minute))
	s.classicSupplyAttemptAt = time.Now()
	ctx, rep := withReadReport(context.Background())
	s.fillMarketCapsFromSupply(ctx, cetesRow(), map[string]int{})
	if !rep.stale.Load() {
		t.Error("market caps built from an expired broad supply map left the response unflagged")
	}
}

// cachedClassicSupply never reports, even for an expired entry: its only caller is the background prewarm, which serves
// nothing. Request paths use classicSupplyMap.
func TestCachedClassicSupply_ExpiredEntryDoesNotReport(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = failingClassicExplorer{}
	s.classicSupplyCache = map[string]string{cetesAsset: "400000000"}
	s.classicSupplyAt = time.Now().Add(-(classicSupplyTTL + time.Minute))
	s.classicSupplyAttemptAt = time.Now()
	ctx, rep := withReadReport(context.Background())
	if m := s.cachedClassicSupply(ctx); m == nil || rep.stale.Load() {
		t.Errorf("map = %v, stale = %v; want the map served and nothing reported", m, rep.stale.Load())
	}
}
