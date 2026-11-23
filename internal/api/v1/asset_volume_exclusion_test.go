package v1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubVolumeCharacter struct{}

func (stubVolumeCharacter) AssetVolumeCharacterRollup(context.Context, string) (timescale.AssetVolumeCharacter, bool, error) {
	return timescale.AssetVolumeCharacter{WindowDays: 14, VolumeUSD: "987654.32", Character: "wash"}, true, nil
}

// TestAssetVolume_ExclusionOnAnXLMMarketWithholdsXLMVolume: XLM's 24h and 14-day USD volumes sum every pair XLM sits on, so an
// exclusion on AQUA/XLM withholds both on the listing and the detail, while XLM's price (read from its USD books) still serves.
func TestAssetVolume_ExclusionOnAnXLMMarketWithholdsXLMVolume(t *testing.T) {
	price, vol, char := "0.25", "1000000.00", "market"
	native := v1.AssetDetail{
		AssetID: "native", Type: "native", Code: "XLM", Decimals: 7, Sep1Status: "not_applicable",
		PriceUSD: &price, VolumeUSD24h: &vol, VolumeCharacter: char,
	}
	gate := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{
		Source: "soroswap", Base: "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", Quote: canonical.NativeAsset().String(),
		From: time.Now().Add(-2 * time.Hour), Until: time.Now().Add(-time.Hour),
	}}}, pricingguard.SourceExclusionGateOptions{})
	srv := v1.New(v1.Options{
		Assets:          &stubAssetReader{byID: map[string]v1.AssetDetail{"native": native}, page: []v1.AssetDetail{native}},
		VolumeCharacter: stubVolumeCharacter{},
		Exclusions:      gate,
	})
	ts := startHTTPTest(t, srv.Handler())
	for _, path := range []string{"/v1/assets", "/v1/assets/native"} {
		body, _ := readAll(mustGet(t, ts.URL+path))
		if strings.Contains(body, `"volume_24h_usd":"`) || strings.Contains(body, `"volume_usd":"`) ||
			strings.Contains(body, `"volume_character":"`) {
			t.Errorf("%s serves a volume the AQUA/XLM exclusion covers: %s", path, body)
		}
		if !strings.Contains(body, `"price_usd":"0.25`) {
			t.Errorf("%s withheld XLM's price, which no XLM/USD book exclusion covers: %s", path, body)
		}
		if !strings.Contains(body, `"fills_withheld_reason":"source_excluded"`) {
			t.Errorf("%s names no withheld fill: %s", path, body)
		}
	}
}

// TestAssetVolume_ExclusionOnTheDerivedSACWithholdsClassicVolume: a classic's volume may include its derived SAC's trades, which
// sit outside the classic's alias family, so an exclusion on the SAC's market withholds the classic's volume, not its price.
func TestAssetVolume_ExclusionOnTheDerivedSACWithholdsClassicVolume(t *testing.T) {
	const id = "FOO-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	a, err := canonical.ParseAsset(id)
	if err != nil {
		t.Fatal(err)
	}
	sac, err := a.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	price, vol := "2.50", "5000.00"
	foo := v1.AssetDetail{AssetID: id, Type: "classic", Code: "FOO", Decimals: 7, PriceUSD: &price, VolumeUSD24h: &vol}
	gate := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{
		Source: "soroswap", Base: sac, Quote: canonical.NativeAsset().String(), From: time.Now().Add(-time.Hour),
	}}}, pricingguard.SourceExclusionGateOptions{})
	srv := v1.New(v1.Options{
		Assets:     &stubAssetReader{byID: map[string]v1.AssetDetail{id: foo}, page: []v1.AssetDetail{foo}},
		Exclusions: gate,
	})
	ts := startHTTPTest(t, srv.Handler())
	for _, path := range []string{"/v1/assets", "/v1/assets/" + id} {
		body, _ := readAll(mustGet(t, ts.URL+path))
		if strings.Contains(body, `"volume_24h_usd":"`) || !strings.Contains(body, `"fills_withheld_reason":"source_excluded"`) {
			t.Errorf("%s serves a volume the SAC exclusion covers, or names no withheld fill: %s", path, body)
		}
		if !strings.Contains(body, `"price_usd":"2.50"`) {
			t.Errorf("%s withheld FOO's price, which an exclusion on its SAC's XLM market does not cover: %s", path, body)
		}
	}

	// An exclusion on another asset's SAC covers none of FOO's markets, so its volume stays.
	other, err := canonical.ParseAsset("BAR-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	otherSAC, err := other.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	unrelated := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{
		Source: "soroswap", Base: otherSAC, Quote: canonical.NativeAsset().String(), From: time.Now().Add(-time.Hour),
	}}}, pricingguard.SourceExclusionGateOptions{})
	ts = startHTTPTest(t, v1.New(v1.Options{
		Assets:     &stubAssetReader{byID: map[string]v1.AssetDetail{id: foo}, page: []v1.AssetDetail{foo}},
		Exclusions: unrelated,
	}).Handler())
	for _, path := range []string{"/v1/assets", "/v1/assets/" + id} {
		body, _ := readAll(mustGet(t, ts.URL+path))
		if !strings.Contains(body, `"volume_24h_usd":"5000.00"`) || strings.Contains(body, `"fills_withheld_reason"`) {
			t.Errorf("%s withheld FOO's volume over an exclusion on an unrelated SAC: %s", path, body)
		}
	}
}

// TestAssetDetail_TrustReadsTheJudgedPrice: the trust summary is rebuilt after the exclusion judgement, so a withheld price is
// not still scored as an observed market price.
func TestAssetDetail_TrustReadsTheJudgedPrice(t *testing.T) {
	const id = "FOO-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	price := "2.50"
	foo := v1.AssetDetail{AssetID: id, Type: "classic", Code: "FOO", Decimals: 7, PriceUSD: &price}
	gate := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{
		Source: "sdex", Base: id, Quote: exclUSDC, From: time.Now().Add(-time.Hour),
	}}}, pricingguard.SourceExclusionGateOptions{})
	srv := v1.New(v1.Options{Assets: &stubAssetReader{byID: map[string]v1.AssetDetail{id: foo}}, Exclusions: gate})
	body, _ := readAll(mustGet(t, startHTTPTest(t, srv.Handler()).URL+"/v1/assets/"+id))
	if !strings.Contains(body, `"price_withheld_reason":"source_excluded"`) {
		t.Fatalf("price not withheld: %s", body)
	}
	if strings.Contains(body, `"market_priced"`) || !strings.Contains(body, `"no_market_price"`) {
		t.Errorf("trust still scores the withheld price as market-priced: %s", body)
	}
}

// TestAssetVolumeCharacter_JudgedOverItsWindow: the volume-character signals sum 14 days of trades, so an exclusion on the
// asset's market that ended 3 days ago withholds them while the 24h volume, outside it, still serves.
func TestAssetVolumeCharacter_JudgedOverItsWindow(t *testing.T) {
	const id = "FOO-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	vol := "5000.00"
	foo := v1.AssetDetail{AssetID: id, Type: "classic", Code: "FOO", Decimals: 7, VolumeUSD24h: &vol}
	gate := pricingguard.NewSourceExclusionGate(exclTable{rows: []timescale.SourceExclusion{{
		Source: "sdex", Base: id, Quote: exclUSDC, From: time.Now().Add(-4 * 24 * time.Hour), Until: time.Now().Add(-3 * 24 * time.Hour),
	}}}, pricingguard.SourceExclusionGateOptions{})
	srv := v1.New(v1.Options{
		Assets:          &stubAssetReader{byID: map[string]v1.AssetDetail{id: foo}},
		VolumeCharacter: stubVolumeCharacter{},
		Exclusions:      gate,
	})
	body, _ := readAll(mustGet(t, startHTTPTest(t, srv.Handler()).URL+"/v1/assets/"+id))
	if strings.Contains(body, `"volume_character_signals"`) || strings.Contains(body, `"volume_character":"`) {
		t.Errorf("volume character built on the excluded window still serves: %s", body)
	}
	if !strings.Contains(body, `"volume_24h_usd":"5000.00"`) {
		t.Errorf("the 24h volume, outside the exclusion, was withheld: %s", body)
	}
}
