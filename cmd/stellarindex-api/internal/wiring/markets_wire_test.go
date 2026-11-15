package wiring

import (
	"reflect"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Every field the store fills must reach the wire on every listing: the lower-bound flag was copied on the
// source listing only, so the default and ?asset= listings served a partial volume as a total.
func TestWireMarkets_CarriesEveryStoreField(t *testing.T) {
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	vol, last := "12.5", "0.11"
	row := timescale.Market{
		Pair: pair, LastTradeAt: time.Unix(1700000000, 0).UTC(), BucketCloseAt: time.Unix(1700000060, 0).UTC(),
		TradeCount24h: 7, Volume24hUSD: &vol, VolumeLowerBound: true, LastPrice: &last,
	}
	rv := reflect.ValueOf(row)
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).IsZero() {
			t.Fatalf("fixture leaves timescale.Market.%s zero: a new store field needs a value here and a line in wireMarkets", rv.Type().Field(i).Name)
		}
	}
	got := wireMarkets([]timescale.Market{row})[0]
	if !got.VolumeLowerBound {
		t.Error("volume_lower_bound dropped")
	}
	if got.Volume24hUSD == nil || *got.Volume24hUSD != vol || got.LastPrice == nil || *got.LastPrice != last || got.TradeCount24h != 7 {
		t.Errorf("wire row = %+v", got)
	}
	if got.Base != pair.Base.String() || got.Quote != pair.Quote.String() {
		t.Errorf("pair = %s/%s", got.Base, got.Quote)
	}
}
