package v1_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/RatesEngine/rates-engine/internal/api/v1"
	"github.com/RatesEngine/rates-engine/internal/currency"
)

// otherRealIssuer is a real, CRC-valid Stellar G-strkey unrelated to
// Circle. The collision tests reuse it as a stand-in "someone else
// issuing USDC on Stellar" so the asset_id passes the canonical
// strkey validator (which checks the CRC, not just the prefix).
// Borrowed from internal/api/v1/known_issuers.go — Aquarius's AQUA
// issuer; we're not testing AQUA here, just needing a different
// real G-strkey to pair with the USDC code.
const otherRealIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

func newTestCatalogue(t *testing.T) *currency.Catalogue {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	return cat
}

func TestAssetGet_VerifiedAsset_NoWarning(t *testing.T) {
	// The real Circle USDC matches the catalogue's verified entry
	// exactly — no warning, no flag.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	url := ts.URL + "/v1/assets/USDC-" + testUSDCIssuer
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.UnverifiedWarning != nil {
		t.Errorf("UnverifiedWarning attached to verified USDC: %+v", env.Data.UnverifiedWarning)
	}
	if env.Flags.UnverifiedTickerCollision {
		t.Error("flags.unverified_ticker_collision = true on verified asset")
	}
}

func TestAssetGet_TickerCollision_AttachesWarning(t *testing.T) {
	// USDC ticker with a fake issuer — warning + flag should fire.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	url := ts.URL + "/v1/assets/USDC-" + otherRealIssuer
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if !env.Flags.UnverifiedTickerCollision {
		t.Error("flags.unverified_ticker_collision = false on collision")
	}
	if env.Data.UnverifiedWarning == nil {
		t.Fatal("UnverifiedWarning not attached")
	}
	w := env.Data.UnverifiedWarning
	if w.VerifiedSlug != "usdc" {
		t.Errorf("verified_slug = %q, want usdc", w.VerifiedSlug)
	}
	if w.VerifiedAssetID != "USDC-"+testUSDCIssuer {
		t.Errorf("verified_asset_id = %q, want USDC-%s", w.VerifiedAssetID, testUSDCIssuer)
	}
	if w.VerifiedName != "USD Coin" {
		t.Errorf("verified_name = %q, want USD Coin", w.VerifiedName)
	}
	if w.VerifiedIssuer == "" {
		t.Error("verified_issuer empty — expected an attribution label")
	}
	if !strings.Contains(w.Note, "USDC") || !strings.Contains(w.Note, testUSDCIssuer) {
		t.Errorf("note doesn't mention USDC + issuer: %q", w.Note)
	}
}

func TestAssetGet_NoCatalogue_NoWarning(t *testing.T) {
	// When the catalogue isn't wired (operator hasn't set
	// VerifiedCurrencies on Options), no warning surface appears —
	// even for known collisions. This is the pre-Phase-1.1 behaviour.
	srv := v1.New(v1.Options{}) // no VerifiedCurrencies
	ts := httpTestServer(t, srv)

	url := ts.URL + "/v1/assets/USDC-" + otherRealIssuer
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.UnverifiedWarning != nil {
		t.Errorf("UnverifiedWarning attached without a catalogue: %+v", env.Data.UnverifiedWarning)
	}
	if env.Flags.UnverifiedTickerCollision {
		t.Error("flags.unverified_ticker_collision = true without a catalogue")
	}
}

func TestAssetGet_NativeAndFiat_NoWarning(t *testing.T) {
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/assets/native", "/v1/assets/fiat:USD"} {
		t.Run(path, func(t *testing.T) {
			resp := mustGet(t, ts.URL+path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var env struct {
				Data  v1.AssetDetail `json:"data"`
				Flags v1.Flags       `json:"flags"`
			}
			mustDecode(t, resp, &env)
			if env.Data.UnverifiedWarning != nil {
				t.Errorf("warning attached: %+v", env.Data.UnverifiedWarning)
			}
			if env.Flags.UnverifiedTickerCollision {
				t.Error("collision flag set")
			}
		})
	}
}

func TestAssetGet_UnknownCode_NoWarning(t *testing.T) {
	// A code that no verified currency claims on Stellar → no
	// warning, even with a syntactically-valid-but-unknown issuer.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/XYZWHATEVER-"+otherRealIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.UnverifiedWarning != nil {
		t.Errorf("warning attached on unknown code: %+v", env.Data.UnverifiedWarning)
	}
	if env.Flags.UnverifiedTickerCollision {
		t.Error("collision flag set on unknown code")
	}
}

func TestAssetGet_WarningSerialisationShape(t *testing.T) {
	// Lock the exact JSON keys the explorer + Freighter will consume.
	// Renaming any field is a wire-shape break.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+otherRealIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw["data"], &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	warning, ok := data["unverified_warning"]
	if !ok {
		t.Fatal("data.unverified_warning missing from JSON body")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(warning, &keys); err != nil {
		t.Fatalf("decode warning: %v", err)
	}
	for _, k := range []string{"verified_slug", "verified_asset_id", "verified_name", "verified_issuer", "note"} {
		if _, present := keys[k]; !present {
			t.Errorf("warning missing key %q", k)
		}
	}

	var flags map[string]json.RawMessage
	if err := json.Unmarshal(raw["flags"], &flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	if _, ok := flags["unverified_ticker_collision"]; !ok {
		t.Error("flags.unverified_ticker_collision missing from JSON body")
	}
}
