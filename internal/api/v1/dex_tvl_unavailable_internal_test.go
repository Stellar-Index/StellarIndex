// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// tvlExclusionFor returns the headline's excluded entry for subject.
func tvlExclusionFor(total *DEXTVLTotalView, subject string) (DEXTVLExclusion, bool) {
	if total == nil {
		return DEXTVLExclusion{}, false
	}
	for _, ex := range total.Excluded {
		if ex.Subject == subject {
			return ex, true
		}
	}
	return DEXTVLExclusion{}, false
}

// tvlNotDerivedDetail GETs /v1/protocols/{name}/tvl and returns the
// status and problem detail.
func tvlNotDerivedDetail(t *testing.T, c *DEXTVLCache, name string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Options{DEXTVL: c}).Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/protocols/"+name+"/tvl", nil))
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, p.Detail
}

// TestDEXTVLCache_FirstCycleReadFailureIsNamedNotVanished is the #675
// regression. A protocol whose FIRST reserve read fails has no previous
// figure to carry, so it used to vanish: absent from the snapshot, never
// reached reconcileDEXTVLTotal's loop, so tvl_total dropped it with no
// excluded entry and lower_bound=false, and its drill-down 404 blamed
// "not wired on this deployment" for a derivation that IS wired.
func TestDEXTVLCache_FirstCycleReadFailureIsNamedNotVanished(t *testing.T) {
	src := tvlTestSources()
	src.SoroswapPairs = stubTVLPairsReader{err: errors.New("registry down")}
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh returned nil for a failed reserve read")
	}
	if _, ok := c.Protocol("soroswap"); ok {
		t.Fatal("soroswap published a figure on a first-cycle read failure")
	}
	total := c.Total()
	ex, ok := tvlExclusionFor(total, "soroswap")
	if !ok {
		t.Fatal("tvl_total.excluded does not name soroswap, whose read failed this cycle")
	}
	if !strings.Contains(ex.Reason, "read failed") {
		t.Errorf("soroswap exclusion reason = %q, want it to say the read failed", ex.Reason)
	}
	if !total.LowerBound {
		t.Error("tvl_total.lower_bound = false while a derived protocol is missing from the sum")
	}
	status, detail := tvlNotDerivedDetail(t, c, "soroswap")
	if status != http.StatusNotFound || !strings.Contains(detail, "read failed") || strings.Contains(detail, "not wired") {
		t.Errorf("GET soroswap/tvl = %d %q, want 404 saying the read failed this cycle", status, detail)
	}
}

// TestDEXTVLCache_AquariusBasisStatesItsIdentityLimits pins the #675
// doc-truth half: aquarius carries most of the headline, and its token
// identities are positional recovery over a table with no transaction
// order. The Basis every consumer reads must say so.
func TestDEXTVLCache_AquariusBasisStatesItsIdentityLimits(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, ok := c.Protocol("aquarius")
	if !ok {
		t.Fatal("aquarius missing")
	}
	for _, want := range []string{"recovered by position", "either one's post-state"} {
		if !strings.Contains(snap.TVL.Basis, want) {
			t.Errorf("aquarius basis = %q, want it to state %q", snap.TVL.Basis, want)
		}
	}
}

// TestDEXTVLCache_EmptyReserveReadIsUnavailableNotZero is the
// CA2-A03-correct-1 regression. A configured pool set whose reader
// returns no pools at all (a network where the curated mainnet ids are
// not in the lake, an empty registry) used to publish tvl_usd "0.00"
// with pools_total 0 as a fresh, exact figure admitted to the headline
// with lower_bound=false — the "zero TVL" the reader contract forbids.
func TestDEXTVLCache_EmptyReserveReadIsUnavailableNotZero(t *testing.T) {
	src := tvlTestSources()
	src.PhoenixReserves = stubPhoenixReserveReader{states: map[string]clickhouse.PhoenixPoolState{}}
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v (an empty read is not a read failure)", err)
	}
	if snap, ok := c.Protocol("phoenix"); ok {
		t.Fatalf("phoenix published %q with %d pools from an empty read, want no figure",
			snap.TVL.TVLUSD, snap.TVL.PoolsTotal)
	}
	total := c.Total()
	if total == nil {
		t.Fatal("tvl_total absent")
	}
	for _, name := range total.Protocols {
		if name == "phoenix" {
			t.Error("tvl_total sums phoenix from an empty read")
		}
	}
	ex, ok := tvlExclusionFor(total, "phoenix")
	if !ok || !strings.Contains(ex.Reason, "no pools") {
		t.Errorf("phoenix exclusion = %+v (found %v), want a reason naming the empty read", ex, ok)
	}
	if !total.LowerBound {
		t.Error("tvl_total.lower_bound = false while phoenix is missing from the sum")
	}
	status, detail := tvlNotDerivedDetail(t, c, "phoenix")
	if status != http.StatusNotFound || !strings.Contains(detail, "no pools") {
		t.Errorf("GET phoenix/tvl = %d %q, want 404 naming the empty read", status, detail)
	}
}
