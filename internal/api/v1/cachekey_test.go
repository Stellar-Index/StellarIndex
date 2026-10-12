package v1

import (
	"strconv"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCacheKey_ScalarGrammar pins the wire format so an edit that changes the field encoding or
// dimension order is caught; stability makes the prewarm/handler equality tests below meaningful.
// Length-prefixed netstring-style fields: "<op-len>:<op><field-len>:<field>…".
func TestCacheKey_ScalarGrammar(t *testing.T) {
	got := newCacheKey("SourceMarkets").
		str("binance").str("cur").int(200).
		order(int(timescale.MarketsOrderVolume24hDesc)).build()
	orderStr := strconv.Itoa(int(timescale.MarketsOrderVolume24hDesc))
	want := "13:SourceMarkets" +
		"7:binance" +
		"3:cur" +
		"3:200" +
		strconv.Itoa(len(orderStr)) + ":" + orderStr
	if got != want {
		t.Errorf("scalar grammar\n got %q\nwant %q", got, want)
	}
}

// TestCacheKey_SeparatorByteCannotForgeFieldBoundary: a value containing the '|' separator must not let two different dimension
// splits collide (str("a|b").str("c") vs str("a").str("b|c")), or one caller's cached page could serve another's.
func TestCacheKey_SeparatorByteCannotForgeFieldBoundary(t *testing.T) {
	crossed := newCacheKey("ListAssetsExt").str("a|b").str("c").build()
	straight := newCacheKey("ListAssetsExt").str("a").str("b|c").build()
	if crossed == straight {
		t.Fatalf("field-boundary forgery: str(%q).str(%q) collided with str(%q).str(%q): both = %q",
			"a|b", "c", "a", "b|c", crossed)
	}
}

// TestCacheKey_StrSetCommaCannotForgeMemberBoundary: strSet analogue; one member containing the old ',' separator must not
// collide with two members splitting the same bytes differently.
func TestCacheKey_StrSetCommaCannotForgeMemberBoundary(t *testing.T) {
	oneMember := newCacheKey("AllPools").strSet([]string{"a,b"}).build()
	twoMembers := newCacheKey("AllPools").strSet([]string{"a", "b"}).build()
	if oneMember == twoMembers {
		t.Fatalf("set-member-boundary forgery: strSet([%q]) collided with strSet([%q,%q]): both = %q",
			"a,b", "a", "b", oneMember)
	}
}

// TestCacheKey_StrSetNormalisesOrder is the core anti-drift guarantee: a set-valued dimension
// built from the same members in a different order produces the SAME key, so the AllPools
// prewarm need not rely on a registry-sorted-slices convention.
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
	base := newCacheKey("ListAssetsExt").int(100).str("iss").str("code").
		str("cur").str("q").order(int(timescale.AssetsOrderObservationCountDesc)).build()
	variants := map[string]string{
		"limit":  newCacheKey("ListAssetsExt").int(101).str("iss").str("code").str("cur").str("q").order(int(timescale.AssetsOrderObservationCountDesc)).build(),
		"issuer": newCacheKey("ListAssetsExt").int(100).str("OTHER").str("code").str("cur").str("q").order(int(timescale.AssetsOrderObservationCountDesc)).build(),
		"cursor": newCacheKey("ListAssetsExt").int(100).str("iss").str("code").str("NEXT").str("q").order(int(timescale.AssetsOrderObservationCountDesc)).build(),
	}
	for dim, v := range variants {
		if v == base {
			t.Errorf("changing %s did not change the key (%q)", dim, base)
		}
	}
}
