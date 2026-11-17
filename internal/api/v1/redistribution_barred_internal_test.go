// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func TestRedistributionBarred_RegistryFacts(t *testing.T) {
	for src, want := range map[string]bool{
		"coinmarketcap": true, "cryptocompare": true, "tiingo": true,
		"coingecko": false, "reflector-dex": false, "not-registered": false,
	} {
		if got := external.RedistributionBarred(src); got != want {
			t.Errorf("RedistributionBarred(%q) = %v, want %v", src, got, want)
		}
	}
}

// /v1/assets and /v1/assets/{id}: a barred aggregator's row never becomes global_market.price_usd; CoinGecko's still does.
func TestGlobalMarket_BarredAggregatorNotServed(t *testing.T) {
	for _, src := range []string{"coinmarketcap", "cryptocompare"} {
		s := globalMarketServer(t, refUpdate(t, src, "crypto:USDC", "fiat:USD", "97000000", 8, time.Now()))
		rows := []AssetDetail{{AssetID: globalTestUSDC}}
		s.fillDeclaredPegPricesInListing(context.Background(), rows)
		if rows[0].GlobalMarket != nil {
			t.Errorf("listing: %s served as global_market %+v", src, rows[0].GlobalMarket)
		}
		d := AssetDetail{AssetID: globalTestUSDC}
		s.fillDeclaredPegPrice(context.Background(), &d, nil)
		if d.GlobalMarket != nil || d.PriceBasis == priceBasisGlobalMarket {
			t.Errorf("detail: %s served as global_market %+v (basis %q)", src, d.GlobalMarket, d.PriceBasis)
		}
	}
	s := globalMarketServer(t, refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now()))
	d := AssetDetail{AssetID: globalTestUSDC}
	s.fillDeclaredPegPrice(context.Background(), &d, nil)
	if d.GlobalMarket == nil {
		t.Error("coingecko no longer fills global_market: its behaviour must not change")
	}
}

// /v1/oracle/streams: a barred oracle-class source is dropped beside a servable one.
func TestOracleStreams_DropsBarredSource(t *testing.T) {
	native := canonical.NativeAsset()
	usd, _ := canonical.ParseAsset("fiat:USD")
	mk := func(src string) canonical.OracleUpdate {
		return canonical.OracleUpdate{
			Source: src, Timestamp: time.Now().UTC(), Asset: native, Quote: usd,
			Price: canonical.NewAmount(big.NewInt(12_000_000)), Decimals: 8,
		}
	}
	s := New(Options{Oracle: &streamOracle{streams: []canonical.OracleUpdate{mk("reflector-dex"), mk("tiingo")}}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/oracle/streams", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Data []OracleReading `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data) != 1 || env.Data[0].Source != "reflector-dex" {
		t.Errorf("streams = %+v, want only reflector-dex", env.Data)
	}
}

// /v1/rwa/assets reference block: a Tiingo RWA row publishes no instrument reference; the history series refuses its buckets.
func TestRWAReference_BarredSourceNotServed(t *testing.T) {
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "tiingo", "rwa:USTRY", "fiat:USD", "104000000", 8, time.Now()),
	})
	if len(snap.byFeed) != 0 {
		t.Errorf("tiingo RWA row published: byFeed=%v", snap.byFeed)
	}
	rwa, _ := canonical.ParseAsset("rwa:USTRY")
	if rwaAdmissibleFeedRow(timescale.OracleDayPoint{Source: "tiingo", Asset: rwa, Price: big.NewInt(104), Decimals: 2}) {
		t.Error("a tiingo day bucket entered an RWA reference series")
	}
}
