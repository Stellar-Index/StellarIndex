package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// assetExtensionDetail serves GET /v1/assets/{id} for a Soroban contract
// whose whole price picture comes from the asset-catalogue overlay (no
// canonical price reader wired), and returns the decoded detail.
func assetExtensionDetail(t *testing.T, assetID string, cache *v1.NonstandardDecimalsCache, overlay *stubAssetsReaderExt) v1.AssetDetail {
	t.Helper()
	srv := v1.New(v1.Options{
		Assets: &stubAssetReader{byID: map[string]v1.AssetDetail{
			assetID: {AssetID: assetID, Type: "soroban"},
		}},
		AssetsReader:        overlay,
		NonstandardDecimals: cache,
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+assetID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

// rawOverlay is what the catalogue readers return for a market whose RAW
// prices_1m ratio is 41.32 — the runbook's real CC2RB… incident value.
func rawOverlay(assetID string) *stubAssetsReaderExt {
	return &stubAssetsReaderExt{
		row: timescale.AssetRow{AssetID: assetID, PriceUSD: sptr("41.3200000000")},
		hist24: []timescale.AssetPricePoint{
			{T: "2026-09-17T00:00:00Z", P: sptr("41.3200000000")},
			{T: "2026-09-17T01:00:00Z", P: nil}, // an hour with no trades stays a gap
			{T: "2026-09-17T02:00:00Z", P: sptr("40.0000000000")},
		},
		hist7d: []timescale.AssetPricePoint{
			{T: "2026-09-11T00:00:00Z", P: sptr("39.5000000000")},
		},
		ath: &timescale.AssetATH{USD: "55.125", At: "2026-08-01T00:00:00Z"},
	}
}

// The overlay's row price, both price histories and the ATH are RAW CAGG
// ratios. For a confirmed 9-decimals token they were published verbatim
// — 100x low — beside a price_usd the canonical path normalises, so one
// payload disagreed with itself by a power of ten.
func TestAssetGet_AssetExtension_NormalisesNonstandardDecimals(t *testing.T) {
	d := assetExtensionDetail(t, flaggedAsset,
		nonstandardDecimalsCacheWith(t, flaggedAsset, 9), rawOverlay(flaggedAsset))

	if d.PriceUSD == nil || *d.PriceUSD != "4132.0000000000" {
		t.Errorf("price_usd = %v, want 4132.0000000000", deref(d.PriceUSD))
	}
	if len(d.PriceHistory24h) != 3 {
		t.Fatalf("price_history_24h has %d points, want 3 (the bucket grid must not change)", len(d.PriceHistory24h))
	}
	if got := deref(d.PriceHistory24h[0].P); got != "4132.0000000000" {
		t.Errorf("price_history_24h[0] = %q, want 4132.0000000000", got)
	}
	if d.PriceHistory24h[1].P != nil {
		t.Errorf("price_history_24h[1] = %q, want a null gap", *d.PriceHistory24h[1].P)
	}
	if got := deref(d.PriceHistory24h[2].P); got != "4000.0000000000" {
		t.Errorf("price_history_24h[2] = %q, want 4000.0000000000", got)
	}
	if len(d.PriceHistory7d) != 1 || deref(d.PriceHistory7d[0].P) != "3950.0000000000" {
		t.Errorf("price_history_7d = %+v, want one point at 3950.0000000000", d.PriceHistory7d)
	}
	if d.ATH == nil || d.ATH.USD != "5512.5000000000" {
		t.Errorf("ath = %+v, want usd 5512.5000000000", d.ATH)
	}
	if d.ATH != nil && d.ATH.At != "2026-08-01T00:00:00Z" {
		t.Errorf("ath.at = %q, want it carried through unchanged", d.ATH.At)
	}
}

// The same overlay for an asset with NO confirmed row must leave the wire
// bytes alone, even with the table populated for someone else: the
// catalogue's text rendering is not ratToDecimal's, so reformatting
// unconditionally would move every already-correct 7dp price.
func TestAssetGet_AssetExtension_SevenDecimalsByteIdentical(t *testing.T) {
	const plain = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	overlay := rawOverlay(plain)
	overlay.row.PriceUSD = sptr("41.32")
	d := assetExtensionDetail(t, plain, nonstandardDecimalsCacheWith(t, flaggedAsset, 9), overlay)

	if got := deref(d.PriceUSD); got != "41.32" {
		t.Errorf("price_usd = %q, want 41.32 byte-identical", got)
	}
	if len(d.PriceHistory24h) != 3 || deref(d.PriceHistory24h[0].P) != "41.3200000000" ||
		d.PriceHistory24h[1].P != nil || deref(d.PriceHistory24h[2].P) != "40.0000000000" {
		t.Errorf("price_history_24h = %+v, want it byte-identical", d.PriceHistory24h)
	}
	if len(d.PriceHistory7d) != 1 || deref(d.PriceHistory7d[0].P) != "39.5000000000" {
		t.Errorf("price_history_7d = %+v, want it byte-identical", d.PriceHistory7d)
	}
	if d.ATH == nil || d.ATH.USD != "55.125" {
		t.Errorf("ath = %+v, want usd 55.125 byte-identical", d.ATH)
	}
}
