package v1

import (
	"context"
	"strings"
	"testing"
	"time"
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

// TestMarkLakeStaleTVL: a stale lake marks the lake-derived figures and a total that sums one,
// on copies; the served-tier figure and the cached values stay as they were.
func TestMarkLakeStaleTVL(t *testing.T) {
	snap := map[string]ProtocolTVLView{
		"soroswap": {TVLUSD: "10.00"},
		"aquarius": {TVLUSD: "5.00"},
	}
	total := &DEXTVLTotalView{TVLUSD: "15.00", Protocols: []string{"aquarius", "soroswap"}}

	out, outTotal, stale := markLakeStaleTVL(snap, total, true)
	if !stale || !out["soroswap"].Stale || out["aquarius"].Stale || !outTotal.Stale {
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
	if !stale || !tvls["soroswap"].Stale || tvls["aquarius"].Stale || total == nil || !total.Stale {
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
	if !stale || !tvls["soroswap"].Stale || tvls["aquarius"].Stale || total == nil || !total.Stale {
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
