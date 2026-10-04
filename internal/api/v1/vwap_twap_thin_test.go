package v1_test

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// A seeded market below the substance floor still serves on /v1/vwap and
// /v1/twap (ADR-0018 raw tier), but must say so: flags.thin_market plus the
// same substance block /v1/price withholds with.
func TestVWAPTWAPFlagThinMarketWithEvidence(t *testing.T) {
	native := canonical.NativeAsset()
	thin, err := canonical.ParseAsset(thinAssetID)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := canonical.ParseAsset(clearedAssetID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &pairAwareHistoryReader{tradesByPair: map[string][]canonical.Trade{
		thin.String() + "/native":    {scamTestTrade(t, thin, native)},
		cleared.String() + "/native": {scamTestTrade(t, cleared, native)},
	}}
	gate := pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAssetID: true}}, pricingguard.SubstanceGateOptions{})
	ts := startHTTPTest(t, v1.New(v1.Options{History: reader, Substance: gate}).Handler())

	for _, endpoint := range []string{"/v1/vwap", "/v1/twap"} {
		status, body := fetch(t, ts.URL+endpoint+"?base="+thinAssetID+"&quote=native")
		if status != http.StatusOK {
			t.Fatalf("%s thin: status %d, want 200 (raw tier serves): %s", endpoint, status, body)
		}
		env := decodeMap(t, body)
		if flags, _ := env["flags"].(map[string]any); flags["thin_market"] != true {
			t.Errorf("%s thin: flags.thin_market = %v, want true", endpoint, flags["thin_market"])
		}
		data, _ := env["data"].(map[string]any)
		assertEvidence(t, data["substance"], thinAssetID)

		status, body = fetch(t, ts.URL+endpoint+"?base="+clearedAssetID+"&quote=native")
		if status != http.StatusOK {
			t.Fatalf("%s cleared: status %d, want 200: %s", endpoint, status, body)
		}
		if strings.Contains(string(body), "thin_market") || strings.Contains(string(body), "substance") {
			t.Errorf("%s cleared pair carries a thin marker: %s", endpoint, body)
		}
	}
}
