package v1

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// TestCacheKey_ScalarGrammar pins the wire format so a future edit
// that changes the separator or dimension order is caught (the key is
// an in-process detail, but stability makes the prewarm/handler
// equality tests below meaningful).
func TestCacheKey_ScalarGrammar(t *testing.T) {
	got := newCacheKey("SourceMarkets").
		str("binance").str("cur").int(200).
		order(int(timescale.MarketsOrderVolume24hDesc)).build()
	want := "SourceMarkets|binance|cur|200|" +
		strconv.Itoa(int(timescale.MarketsOrderVolume24hDesc))
	if got != want {
		t.Errorf("scalar grammar\n got %q\nwant %q", got, want)
	}
}

// TestCacheKey_StrSetNormalisesOrder is the core anti-drift guarantee:
// a set-valued dimension built from the same members in a different
// order produces the SAME key. This is exactly the Sources-order
// footgun the AllPools prewarm previously relied on a convention
// (registry-sorted slices) to avoid.
func TestCacheKey_StrSetNormalisesOrder(t *testing.T) {
	a := newCacheKey("AllPools").strSet([]string{"soroswap", "aquarius", "phoenix"}).build()
	b := newCacheKey("AllPools").strSet([]string{"phoenix", "soroswap", "aquarius"}).build()
	if a != b {
		t.Errorf("reordered set must yield identical key:\n a=%q\n b=%q", a, b)
	}
}

// TestCacheKey_StrSetNilAndEmptyCollapse: nil and empty both mean "no
// filter" and must share one slot.
func TestCacheKey_StrSetNilAndEmptyCollapse(t *testing.T) {
	nilKey := newCacheKey("AllPools").strSet(nil).build()
	emptyKey := newCacheKey("AllPools").strSet([]string{}).build()
	if nilKey != emptyKey {
		t.Errorf("nil vs empty set:\n nil=%q\n empty=%q", nilKey, emptyKey)
	}
}

// TestCacheKey_StrSetDoesNotMutateInput guards the defensive copy —
// callers pass the ORIGINAL slice on to the upstream query, so the key
// builder must not sort it in place.
func TestCacheKey_StrSetDoesNotMutateInput(t *testing.T) {
	in := []string{"soroswap", "aquarius"}
	_ = newCacheKey("AllPools").strSet(in).build()
	if in[0] != "soroswap" || in[1] != "aquarius" {
		t.Errorf("strSet mutated caller slice: %v", in)
	}
}

// TestCacheKey_DistinctDimensionsDistinctKeys: every result-changing
// dimension must move the key, or two different queries collide and
// one serves the other's rows.
func TestCacheKey_DistinctDimensionsDistinctKeys(t *testing.T) {
	base := newCacheKey("ListCoinsExt").int(100).str("iss").str("code").
		str("cur").str("q").order(int(timescale.CoinsOrderObservationCountDesc)).build()
	variants := map[string]string{
		"limit":  newCacheKey("ListCoinsExt").int(101).str("iss").str("code").str("cur").str("q").order(int(timescale.CoinsOrderObservationCountDesc)).build(),
		"issuer": newCacheKey("ListCoinsExt").int(100).str("OTHER").str("code").str("cur").str("q").order(int(timescale.CoinsOrderObservationCountDesc)).build(),
		"cursor": newCacheKey("ListCoinsExt").int(100).str("iss").str("code").str("NEXT").str("q").order(int(timescale.CoinsOrderObservationCountDesc)).build(),
	}
	for dim, v := range variants {
		if v == base {
			t.Errorf("changing %s did not change the key (%q)", dim, base)
		}
	}
}

// TestCachedMarketsReader_AllPoolsSourcesOrderCollapse is the
// end-to-end proof of the fix: AllPools with the same Sources set in
// two different orders must hit ONE upstream call — the drift bug this
// hardening closes would show 2. Uses the fakeMarketsReader defined in
// markets_cache_test.go (same package).
func TestCachedMarketsReader_AllPoolsSourcesOrderCollapse(t *testing.T) {
	up := &fakeMarketsReader{}
	c := NewCachedMarketsReader(up, 60*time.Second)

	_, _, err := c.AllPools(context.Background(),
		timescale.PoolsFilter{Sources: []string{"aquarius", "soroswap", "phoenix"}},
		"", 50, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatal(err)
	}
	// Same set, reversed order — must land on the warmed slot.
	_, _, err = c.AllPools(context.Background(),
		timescale.PoolsFilter{Sources: []string{"phoenix", "soroswap", "aquarius"}},
		"", 50, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatal(err)
	}
	if got := up.allPoolsCalls.Load(); got != 1 {
		t.Errorf("reordered Sources hit upstream %d times; want 1 (key must be order-normalised)", got)
	}
}
