package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

// A priced RWA row that serves a circulating supply is never reported as having none, whatever refused its cap, and the
// refusal reason /v1/assets serves for it is on the wire. The status keys on the supply, not the reason, so the unlisted
// reason below stands in for one added later.
func TestRWAValuationOf_ACapRefusedBesideASupplyIsNotSupplyUnavailable(t *testing.T) {
	price, supply := "1.05", "10000000.0000000"
	for _, reason := range []MarketCapWithheldReason{
		MarketCapWithheldPriceUnread, MarketCapWithheldSupplyStale, MarketCapWithheldDecimalsUnknown,
		MarketCapWithheldDecimalsDiffer, MarketCapWithheldTickerCollision, MarketCapWithheldLowLiquidity,
		MarketCapWithheldSupplyFloor, MarketCapWithheldThinMarket, MarketCapWithheldPriceBasis,
		MarketCapWithheldReason("a_reason_added_later"), "",
	} {
		d := AssetDetail{PriceUSD: &price, CirculatingSupply: &supply, MarketCapWithheldReason: reason, PriceBasis: priceBasisGlobalMarket}
		v := rwaValuationOf(d)
		if v.Status == RWAValuationNoSupply || v.Status == RWAValuationPublished {
			t.Errorf("reason %q with a served supply: status %q", reason, v.Status)
		}
		if v.MarketCapWithheldReason != reason {
			t.Errorf("reason %q: served market_cap_withheld_reason %q", reason, v.MarketCapWithheldReason)
		}
	}

	d := AssetDetail{PriceUSD: &price, CirculatingSupply: &supply, MarketCapWithheldReason: MarketCapWithheldTickerCollision}
	buf, err := json.Marshal(rwaValuationOf(d))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf); !strings.Contains(got, `"status":"market_cap_withheld"`) ||
		!strings.Contains(got, `"market_cap_withheld_reason":"ticker_collision"`) {
		t.Errorf("wire valuation = %s", got)
	}

	if v := rwaValuationOf(AssetDetail{PriceUSD: &price}); v.Status != RWAValuationNoSupply {
		t.Errorf("no supply: status %q, want %q", v.Status, RWAValuationNoSupply)
	}
}
