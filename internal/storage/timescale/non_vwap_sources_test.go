package timescale

import (
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

func TestNonVWAPSourcesArg(t *testing.T) {
	names := strings.Split(nonVWAPSourcesArg(), ",")
	if !sort.StringsAreSorted(names) {
		t.Errorf("not sorted: %v", names)
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
		if external.IncludeInVWAP(n) {
			t.Errorf("%s is in the VWAP and must not be in the non-VWAP set", n)
		}
	}
	for _, want := range []string{"coingecko_daily", "poloniex_via_btc"} {
		if !set[want] {
			t.Errorf("missing %s", want)
		}
	}
	for _, venue := range []string{"kraken", "sdex", "soroswap"} {
		if set[venue] {
			t.Errorf("%s is a VWAP venue and must not be in the set", venue)
		}
	}
}
