package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const exclFOO = "FOO-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

// swapGate lets a test change the exclusion set between requests, past the production gate's 15s re-read.
type swapGate struct {
	mu sync.Mutex
	g  v1.PriceExclusionGate
}

func (s *swapGate) set(g v1.PriceExclusionGate) { s.mu.Lock(); s.g = g; s.mu.Unlock() }
func (s *swapGate) cur() v1.PriceExclusionGate  { s.mu.Lock(); defer s.mu.Unlock(); return s.g }

func (s *swapGate) Withholding(ctx context.Context, b, q canonical.Asset, src []string, from, to time.Time) pricingguard.Withholding {
	return s.cur().Withholding(ctx, b, q, src, from, to)
}

func (s *swapGate) InstantWithholding(ctx context.Context, b, q canonical.Asset, src string, ts time.Time) pricingguard.Withholding {
	return s.cur().InstantWithholding(ctx, b, q, src, ts)
}

func (s *swapGate) UnattributedWithholding(ctx context.Context, b, q canonical.Asset, from, to time.Time) pricingguard.Withholding {
	return s.cur().UnattributedWithholding(ctx, b, q, from, to)
}

func (s *swapGate) MultiHopWithholding(ctx context.Context, from, to time.Time) pricingguard.Withholding {
	return s.cur().MultiHopWithholding(ctx, from, to)
}

func (s *swapGate) Snapshot(ctx context.Context) (pricingguard.ExclusionSet, pricingguard.Withholding) {
	return s.cur().Snapshot(ctx)
}

func assetExclGate(e exclTable) v1.PriceExclusionGate {
	return pricingguard.NewSourceExclusionGate(e, pricingguard.SourceExclusionGateOptions{})
}

// aquaExcluded excludes one venue on AQUA/USD from an hour ago: it names AQUA, not FOO or XLM.
func aquaExcluded() v1.PriceExclusionGate {
	return assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "soroswap", Base: exclAQUA, Quote: "fiat:USD", From: time.Now().Add(-time.Hour),
	}}})
}

// listingRows is a listing reader serving two priced rows.
type listingRows struct{ stubAssetsReaderExt }

func (listingRows) ListAssetsExt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	row := func(id, code string) timescale.AssetRow {
		return timescale.AssetRow{
			AssetID: id, Code: code, IssuerGStrkey: strings.SplitN(id, "-", 2)[1], ObservationCount: 10,
			PriceUSD: sptr("0.5"), Volume24hUSD: sptr("1000"), MarketCapUSD: sptr("5000"), CirculatingSupply: sptr("10000"),
			Change1hPct: sptr("+1.00"), Change24hPct: sptr("-2.00"), Change7dPct: sptr("+3.00"),
		}
	}
	return []timescale.AssetRow{row(exclAQUA, "AQUA"), row(exclFOO, "FOO")}, nil
}

type assetRowWire map[string]any

func decodeAssetRows(t *testing.T, body string) ([]assetRowWire, map[string]any) {
	t.Helper()
	var env struct {
		Data  []assetRowWire `json:"data"`
		Flags map[string]any `json:"flags"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env.Data, env.Flags
}

// priceDerived are the row fields a withheld price takes with it.
var priceDerived = []string{
	"price_usd", "market_cap_usd", "fdv_usd", "change_1h_pct", "change_24h_pct", "change_7d_pct", "ath",
	"price_history_24h", "price_history_7d", "volume_24h_usd",
}

func wantWithheld(t *testing.T, row assetRowWire, reason string) {
	t.Helper()
	if row["price_withheld_reason"] != reason {
		t.Errorf("%v: price_withheld_reason = %v, want %s", row["asset_id"], row["price_withheld_reason"], reason)
	}
	for _, f := range priceDerived {
		if v, ok := row[f]; ok {
			if s, isSeries := v.([]any); isSeries && seriesUnpriced(s) {
				continue
			}
			t.Errorf("%v: %s = %v served while withheld", row["asset_id"], f, v)
		}
	}
}

func seriesUnpriced(pts []any) bool {
	for _, p := range pts {
		if m, ok := p.(map[string]any); ok && m["p"] != nil {
			return false
		}
	}
	return true
}

// TestAssetListing_SourceExclusionWithholdsCoveredRows: an exclusion naming AQUA withholds AQUA's listing price and every
// value derived from it, leaves FOO's row byte-identical, marks the page and keeps it out of every cache; an empty set
// serves the body unchanged, and an unreadable one withholds every priced row as a stale page.
func TestAssetListing_SourceExclusionWithholdsCoveredRows(t *testing.T) {
	gate := &swapGate{g: assetExclGate(exclTable{})}
	srv := v1.New(v1.Options{AssetsReader: &listingRows{}, Exclusions: gate})
	ts := startHTTPTest(t, srv.Handler())
	get := func() (string, string) {
		resp := mustGet(t, ts.URL+"/v1/assets")
		body, _ := readAll(resp)
		return exclAsOf.ReplaceAllString(body, `"as_of":""`), resp.Header.Get("Cache-Control")
	}
	_, plain := exclGet(t, v1.New(v1.Options{AssetsReader: &listingRows{}}), "/v1/assets")
	clean, _ := get() // also fills the listing cache with a clean page
	if want := exclAsOf.ReplaceAllString(plain, `"as_of":""`); clean != want {
		t.Fatalf("empty exclusion set changed the body:\n got %s\nwant %s", clean, want)
	}
	cleanRows, _ := decodeAssetRows(t, clean)

	gate.set(aquaExcluded())
	body, cc := get()
	rows, flags := decodeAssetRows(t, body)
	if len(rows) != 2 || rows[0]["asset_id"] != exclAQUA {
		t.Fatalf("withholding changed the page: %s", body)
	}
	wantWithheld(t, rows[0], "source_excluded")
	if rows[0]["circulating_supply"] == nil {
		t.Errorf("circulating_supply is not price-derived and must stay: %v", rows[0])
	}
	if a, b := mustJSON(t, rows[1]), mustJSON(t, cleanRows[1]); a != b {
		t.Errorf("FOO row changed:\n got %s\nwant %s", a, b)
	}
	if flags["fills_withheld_reason"] != "source_excluded" || cc != "no-store" {
		t.Errorf("flags %v, Cache-Control %q: want fills_withheld_reason source_excluded and no-store", flags, cc)
	}

	gate.set(assetExclGate(exclTable{err: errors.New("db down")}))
	body, _ = get()
	rows, flags = decodeAssetRows(t, body)
	for _, r := range rows {
		wantWithheld(t, r, "unattributed")
	}
	if flags["stale"] != true {
		t.Errorf("unread set: flags %v, want stale", flags)
	}

	gate.set(assetExclGate(exclTable{}))
	if body, _ = get(); body != clean {
		t.Errorf("after the exclusion the clean page is not served:\n got %s\nwant %s", body, clean)
	}
}

// TestAssetListing_SourceExclusionCoversNonUSDPegFX: a classic declared pegged to EUR is priced through EUR/USD, so an
// exclusion of an FX vendor's EUR reading withholds it and not an unpegged row.
func TestAssetListing_SourceExclusionCoversNonUSDPegFX(t *testing.T) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	gate := assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "ecb", Base: "fiat:EUR", Quote: "fiat:USD", From: time.Now().Add(-time.Hour),
	}}})
	srv := v1.New(v1.Options{AssetsReader: &listingRows{}, Exclusions: gate, FiatPeggedClassics: map[string]canonical.Asset{exclFOO: eur}})
	_, body := exclGet(t, srv, "/v1/assets")
	rows, _ := decodeAssetRows(t, body)
	if len(rows) != 2 || rows[1]["asset_id"] != exclFOO {
		t.Fatalf("page changed: %s", body)
	}
	wantWithheld(t, rows[1], "source_excluded")
	if rows[0]["price_usd"] != "0.5" {
		t.Errorf("unpegged row withheld: %v", rows[0])
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAssetDetail_SourceExclusionWithholdsCataloguePrice: the catalogue-row price, its change, volume, series and ATH are
// withheld while an exclusion names the asset, including when a clean body was cached before it began.
func TestAssetDetail_SourceExclusionWithholdsCataloguePrice(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	overlay := &stubAssetsReaderExt{
		row: timescale.AssetRow{AssetID: exclAQUA, PriceUSD: sptr("0.5"), Change24hPct: sptr("+1.00"), Volume24hUSD: sptr("1000")},
		hist24: []timescale.AssetPricePoint{
			{T: at(3 * time.Hour), P: sptr("0.4")}, {T: at(2 * time.Hour), P: sptr("0.45")}, {T: at(time.Minute), P: sptr("0.5")},
		},
		ath: &timescale.AssetATH{USD: "0.9", At: at(30 * time.Minute)},
	}
	newSrv := func(g v1.PriceExclusionGate) *v1.Server {
		return v1.New(v1.Options{
			Assets:       &stubAssetReader{byID: map[string]v1.AssetDetail{exclAQUA: {AssetID: exclAQUA, Code: "AQUA", Type: "credit_alphanum4"}}},
			AssetsReader: overlay, Exclusions: g,
		})
	}
	_, plain := exclGet(t, newSrv(nil), "/v1/assets/"+exclAQUA)
	if !strings.Contains(plain, `"price_usd":"0.5"`) || !strings.Contains(plain, `"ath":`) {
		t.Fatalf("fixture serves no catalogue price: %s", plain)
	}
	gate := &swapGate{g: assetExclGate(exclTable{})}
	ts := startHTTPTest(t, newSrv(gate).Handler())
	get := func() (assetRowWire, map[string]any, string) {
		resp := mustGet(t, ts.URL+"/v1/assets/"+exclAQUA)
		body, _ := readAll(resp)
		var env struct {
			Data  assetRowWire   `json:"data"`
			Flags map[string]any `json:"flags"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return env.Data, env.Flags, resp.Header.Get("Cache-Control")
	}
	clean, _, _ := get() // cached clean
	if clean["price_usd"] != "0.5" {
		t.Fatalf("empty set withheld the price: %v", clean)
	}
	gate.set(aquaExcluded())
	d, flags, cc := get()
	wantWithheld(t, d, "source_excluded")
	if flags["fills_withheld_reason"] != "source_excluded" || cc != "no-store" {
		t.Errorf("flags %v, Cache-Control %q: want fills_withheld_reason source_excluded and no-store", flags, cc)
	}
	// An exclusion that ended 140 minutes ago leaves the headline but takes what spans it: the change, the volume, the ATH
	// beside it and the one series point whose hour holds it.
	gate.set(assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 2, Source: "soroswap", Base: exclAQUA, Quote: "fiat:USD", From: now.Add(-150 * time.Minute), Until: now.Add(-140 * time.Minute),
	}}}))
	d, flags, _ = get()
	pts, _ := d["price_history_24h"].([]any)
	if d["price_usd"] != "0.5" || d["change_24h_pct"] != nil || d["volume_24h_usd"] != nil || d["ath"] != nil || len(pts) != 3 ||
		pts[0].(map[string]any)["p"] != "0.4" || pts[1].(map[string]any)["p"] != nil || pts[2].(map[string]any)["p"] != "0.5" ||
		flags["fills_withheld_reason"] != "source_excluded" {
		t.Errorf("ended exclusion: %v flags %v", d, flags)
	}
	gate.set(assetExclGate(exclTable{err: errors.New("db down")}))
	if d, flags, _ = get(); flags["stale"] != true {
		t.Errorf("unread set: flags %v, want stale", flags)
	}
	wantWithheld(t, d, "unattributed")
}
