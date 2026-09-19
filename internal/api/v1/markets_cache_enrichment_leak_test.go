// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"strings"
	"testing"
)

// TestMarkets_CachedRows_IncludeEnrichmentDoesNotLeakAcrossRequests —
// finding K038's enrichment leg (the decimals leg is F014, see
// markets_cache_shared_rows_test.go, whose production-shaped server this
// reuses). ?include=sparkline,inception wrote volume_history_24h /
// first_trade_at onto the rows the cache had handed out — which were the
// cache entry's own — so every later request for the same page was served
// enrichment it never opted into (`include` is not part of the cache key).
func TestMarkets_CachedRows_IncludeEnrichmentDoesNotLeakAcrossRequests(t *testing.T) {
	ts, up := sharedRowsServer(t)

	plainBefore, _ := sharedRowsGet(t, ts.URL+"/v1/markets")
	enriched, _ := sharedRowsGet(t, ts.URL+"/v1/markets?include=sparkline,inception")
	if !strings.Contains(enriched, `"volume_history_24h"`) || !strings.Contains(enriched, `"first_trade_at"`) {
		t.Fatalf("opt-in request did not carry the enrichment (test is vacuous): %s", enriched)
	}
	plainAfter, _ := sharedRowsGet(t, ts.URL+"/v1/markets")

	if strings.Contains(plainAfter, `"volume_history_24h"`) || strings.Contains(plainAfter, `"first_trade_at"`) {
		t.Errorf("enrichment leaked into a request that did not opt in: %s", plainAfter)
	}
	if plainAfter != plainBefore {
		t.Errorf("plain page changed after an ?include= request\nbefore: %s\n after: %s", plainBefore, plainAfter)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (all three requests must share one cache entry)", got)
	}
}
