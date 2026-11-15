package v1

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCarryPrev_MarksStaleAndExpires: a carried figure is labelled stale, and one past the carry
// age is not republished at all.
func TestCarryPrev_MarksStaleAndExpires(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	prev := map[string]ProtocolTVLView{
		"fresh": {TVLUSD: "1.00", AsOf: now.Add(-time.Hour).Format(time.RFC3339)},
		"old":   {TVLUSD: "2.00", AsOf: now.Add(-dexTVLCarryMaxAge - time.Minute).Format(time.RFC3339)},
	}
	next := map[string]ProtocolTVLView{}
	if reason := carryPrev(next, prev, "fresh", now); reason != "" {
		t.Fatalf("fresh entry not carried: %q", reason)
	}
	if got := next["fresh"]; !got.Stale || got.TVLUSD != "1.00" {
		t.Errorf("carried entry = %+v, want the previous figure marked stale", got)
	}
	if prev["fresh"].Stale {
		t.Error("carrying mutated the previous snapshot")
	}
	if reason := carryPrev(next, prev, "old", now); reason != dexTVLUnavailableCarryExpired {
		t.Errorf("expired carry reason = %q", reason)
	}
	if _, ok := next["old"]; ok {
		t.Error("a figure past the carry age was republished")
	}
	if reason := carryPrev(next, prev, "never", now); reason != dexTVLUnavailableReadFailed {
		t.Errorf("first-cycle failure reason = %q", reason)
	}
}

// TestDEXTVLCache_UnwiredProtocolIsNamed: a derived protocol with no reader on this process is
// named in the headline's exclusions and makes it a lower bound, instead of leaving unmentioned.
func TestDEXTVLCache_UnwiredProtocolIsNamed(t *testing.T) {
	src := tvlTestSources()
	src.PhoenixPools = nil
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := c.Protocol("phoenix"); ok {
		t.Fatal("phoenix published a figure with no pool set")
	}
	total := c.Total()
	ex, ok := tvlExclusionFor(total, "phoenix")
	if !ok || !strings.Contains(ex.Reason, "not wired") {
		t.Fatalf("excluded = %+v ok=%v, want phoenix named as not wired", ex, ok)
	}
	if !total.LowerBound {
		t.Error("tvl_total.lower_bound = false while a derived protocol is missing from the sum")
	}
}

// TestMarkLakeStaleTVL: a stale lake marks every figure whose reserves come from the lake (aquarius through the
// projector) and a total that sums one, on copies; the cached values stay as they were.
func TestMarkLakeStaleTVL(t *testing.T) {
	snap := map[string]ProtocolTVLView{
		"soroswap": {TVLUSD: "10.00"},
		"aquarius": {TVLUSD: "5.00"},
	}
	total := &DEXTVLTotalView{TVLUSD: "15.00", Protocols: []string{"aquarius", "soroswap"}}

	out, outTotal, stale := markLakeStaleTVL(snap, total, true)
	if !stale || !out["soroswap"].Stale || !out["aquarius"].Stale || !outTotal.Stale {
		t.Errorf("stale lake: stale=%v soroswap=%v aquarius=%v total=%v", stale, out["soroswap"].Stale, out["aquarius"].Stale, outTotal.Stale)
	}
	if snap["soroswap"].Stale || total.Stale {
		t.Error("the cached snapshot or total was mutated")
	}

	out, outTotal, stale = markLakeStaleTVL(snap, total, false)
	if stale || out["soroswap"].Stale || outTotal.Stale {
		t.Errorf("fresh lake marked something stale: %v %+v %+v", stale, out, outTotal)
	}

	carried := map[string]ProtocolTVLView{"aquarius": {TVLUSD: "5.00", Stale: true}}
	if _, _, stale = markLakeStaleTVL(carried, nil, false); !stale {
		t.Error("a carried figure did not raise the stale signal")
	}
}

// TestProtocolTVLsAndTotal_StaleLakeRaisesTheFlag wires the cache to a lake watermark past the
// freshness threshold.
func TestProtocolTVLsAndTotal_StaleLakeRaisesTheFlag(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	old := &stubWatermark{ledger: 63_000_000, closedAt: time.Now().Add(-lakeStaleThreshold - time.Hour)}
	srv := &Server{Options: Options{DEXTVL: c, LakeWatermark: old}}
	tvls, total, stale := srv.protocolTVLsAndTotal(context.Background())
	if !stale || !tvls["soroswap"].Stale || !tvls["aquarius"].Stale || total == nil || !total.Stale {
		t.Errorf("stale=%v soroswap=%+v aquarius=%+v total=%+v", stale, tvls["soroswap"].Stale, tvls["aquarius"].Stale, total)
	}

	fresh := &Server{Options: Options{DEXTVL: c, LakeWatermark: &stubWatermark{ledger: 63_000_000, closedAt: time.Now()}}}
	if _, total, stale := fresh.protocolTVLsAndTotal(context.Background()); stale || total.Stale {
		t.Errorf("fresh lake: stale=%v total.Stale=%v", stale, total.Stale)
	}
}

// TestDEXTVLCache_ReadFromStaleLakeKeepsTheMark: a figure read while the lake was behind stays
// stale until the next refresh, even when the lake has caught up by the time it is served.
func TestDEXTVLCache_ReadFromStaleLakeKeepsTheMark(t *testing.T) {
	src := tvlTestSources()
	src.LakeWatermark = &stubWatermark{ledger: 63_000_000, closedAt: time.Now().Add(-lakeStaleThreshold - time.Hour)}
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	caughtUp := &Server{Options: Options{DEXTVL: c, LakeWatermark: &stubWatermark{ledger: 63_000_700, closedAt: time.Now()}}}
	tvls, total, stale := caughtUp.protocolTVLsAndTotal(context.Background())
	if !stale || !tvls["soroswap"].Stale || !tvls["aquarius"].Stale || total == nil || !total.Stale {
		t.Errorf("stale=%v soroswap=%v aquarius=%v total=%+v", stale, tvls["soroswap"].Stale, tvls["aquarius"].Stale, total)
	}

	src.LakeWatermark = &stubWatermark{err: context.DeadlineExceeded}
	c = NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if v, _ := c.Protocol("soroswap"); !v.TVL.Stale {
		t.Error("a figure read with no watermark answer was published as current")
	}
}

// stubCursors answers GetCursor per source; a source with no entry is not found.
type stubCursors map[string]timescale.Cursor

func (s stubCursors) GetCursor(_ context.Context, source, _ string) (timescale.Cursor, error) {
	cur, ok := s[source]
	if !ok {
		return timescale.Cursor{}, timescale.ErrNotFound
	}
	return cur, nil
}

// TestDEXTVLCache_AquariusBehindItsWritersIsStale: aquarius_reserves is written by the projector, the dispatcher or
// both. When every writer has stopped or trails the lake, a refresh still succeeds, on old reserves.
func TestDEXTVLCache_AquariusBehindItsWritersIsStale(t *testing.T) {
	const lakeLedger = 63_000_000
	now := time.Now()
	level := timescale.Cursor{LastLedger: lakeLedger, UpdatedAt: now}
	trailing := timescale.Cursor{LastLedger: lakeLedger - dexTVLWriterLagLedgers - 1, UpdatedAt: now}
	stopped := timescale.Cursor{LastLedger: lakeLedger, UpdatedAt: now.Add(-lakeStaleThreshold - time.Minute)}
	both := []TVLWriterCursor{{Source: "projector", Sub: "aquarius"}, {Source: "ledgerstream"}}
	cases := []struct {
		name    string
		writers []TVLWriterCursor
		cur     stubCursors
		want    bool
	}{
		{"level with the lake", both[:1], stubCursors{"projector": level}, false},
		{"a few ledgers behind", both[:1], stubCursors{"projector": {LastLedger: lakeLedger - dexTVLWriterLagLedgers, UpdatedAt: now}}, false},
		{"trailing the lake", both[:1], stubCursors{"projector": trailing}, true},
		{"stopped", both[:1], stubCursors{"projector": stopped}, true},
		{"no cursor", both[:1], stubCursors{}, true},
		{"projector replaying, dispatcher writing", both, stubCursors{"projector": trailing, "ledgerstream": level}, false},
		{"projector off, dispatcher writing", both[1:], stubCursors{"ledgerstream": level}, false},
		{"both stopped", both, stubCursors{"projector": stopped, "ledgerstream": stopped}, true},
		{"no writers named", nil, stubCursors{}, false},
	}
	for _, tc := range cases {
		src := tvlTestSources()
		src.LakeWatermark = &stubWatermark{ledger: lakeLedger, closedAt: now}
		src.Cursors, src.AquariusWriters = tc.cur, tc.writers
		c := NewDEXTVLCache(src)
		if err := c.Refresh(context.Background()); err != nil {
			t.Fatalf("%s: Refresh: %v", tc.name, err)
		}
		aq, _ := c.Protocol("aquarius")
		if aq.TVL.Stale != tc.want {
			t.Errorf("%s: aquarius stale = %v, want %v", tc.name, aq.TVL.Stale, tc.want)
		}
		if sw, _ := c.Protocol("soroswap"); sw.TVL.Stale {
			t.Errorf("%s: soroswap reads the lake itself and was marked stale", tc.name)
		}
		// The served total inherits the mark from the figure it sums.
		srv := &Server{Options: Options{DEXTVL: c, LakeWatermark: &stubWatermark{ledger: lakeLedger, closedAt: now}}}
		if _, total, stale := srv.protocolTVLsAndTotal(context.Background()); total == nil || total.Stale != tc.want || stale != tc.want {
			t.Errorf("%s: served total stale = %v, flag = %v, want %v", tc.name, total != nil && total.Stale, stale, tc.want)
		}
	}
}

// TestDEXTVLCache_CuratedPoolWithNoLakeStateIsCountedUnpriced: a curated pool the reader returned nothing for is in
// the pool count and the breakdown, not dropped from both.
func TestDEXTVLCache_CuratedPoolWithNoLakeStateIsCountedUnpriced(t *testing.T) {
	const missing = "CMISSINGPOOL"
	src := tvlTestSources()
	src.PhoenixPools = append(src.PhoenixPools, missing)
	src.CometPools = append(src.CometPools, missing)
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	base := NewDEXTVLCache(tvlTestSources())
	if err := base.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh (baseline): %v", err)
	}
	for _, name := range []string{"phoenix", "comet"} {
		got, _ := c.Protocol(name)
		want, _ := base.Protocol(name)
		if got.TVL.PoolsTotal != want.TVL.PoolsTotal+1 || got.TVL.UnpricedPools != want.TVL.UnpricedPools+1 {
			t.Errorf("%s: pools_total/unpriced = %d/%d, want %d/%d", name, got.TVL.PoolsTotal, got.TVL.UnpricedPools,
				want.TVL.PoolsTotal+1, want.TVL.UnpricedPools+1)
		}
		if got.TVL.TVLUSD != want.TVL.TVLUSD {
			t.Errorf("%s: tvl_usd = %s, want %s unchanged", name, got.TVL.TVLUSD, want.TVL.TVLUSD)
		}
		found := false
		for _, p := range got.Pools {
			found = found || (p.Pool == missing && p.Excluded == DEXTVLPoolUnavailable && p.TVLUSD == "0.00")
		}
		if !found {
			t.Errorf("%s: the missing pool is not in the breakdown: %+v", name, got.Pools)
		}
	}

	// A reader that returned no pool at all still publishes no figure.
	src = tvlTestSources()
	src.PhoenixReserves = stubPhoenixReserveReader{}
	c = NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := c.Protocol("phoenix"); ok {
		t.Error("phoenix published a figure from a read that returned no pool")
	}
}

// TestBuildProtocolView_UnreadEventsAreAbsent: no count map means no count, never a zero.
func TestBuildProtocolView_UnreadEventsAreAbsent(t *testing.T) {
	meta, ok := protocolByName("soroswap")
	if !ok {
		t.Fatal("soroswap missing from registry")
	}
	if v := buildProtocolView(meta, 1, nil, nil); v.Events24h != nil {
		t.Errorf("events_24h = %d with no read, want absent", *v.Events24h)
	}
	v := buildProtocolView(meta, 1, map[string]int64{}, nil)
	if v.Events24h == nil || *v.Events24h != 0 {
		t.Errorf("events_24h = %v after a read that counted none, want 0", v.Events24h)
	}
}

// TestProtocolsCoverageNote_NamesEachFailedRead: every unread part is stated; a clean build says nothing.
func TestProtocolsCoverageNote_NamesEachFailedRead(t *testing.T) {
	if got := protocolsCoverageNote(nil, true, true); got != "" {
		t.Errorf("clean build note = %q", got)
	}
	got := protocolsCoverageNote([]string{"blend"}, false, false)
	for _, want := range []string{"blend", "events_24h is absent", "completeness verdicts could not be read"} {
		if !strings.Contains(got, want) {
			t.Errorf("note %q does not mention %q", got, want)
		}
	}
}
