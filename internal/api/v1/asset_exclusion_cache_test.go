package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const exclBAR = "BAR-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

// countingListing counts upstream listing reads.
type countingListing struct {
	listingRows
	n atomic.Int32
}

func (c *countingListing) ListAssetsExt(ctx context.Context, o timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	c.n.Add(1)
	return c.listingRows.ListAssetsExt(ctx, o)
}

// countingAssets counts upstream detail reads.
type countingAssets struct {
	*stubAssetReader
	n atomic.Int32
}

func (c *countingAssets) GetAsset(ctx context.Context, a canonical.Asset) (v1.AssetDetail, error) {
	c.n.Add(1)
	return c.stubAssetReader.GetAsset(ctx, a)
}

// oldUnrelatedExcluded excludes a venue on BAR/USD for an hour six days ago: it covers nothing the fixtures serve.
func oldUnrelatedExcluded() v1.PriceExclusionGate {
	at := time.Now().Add(-6 * 24 * time.Hour)
	return assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 9, Source: "soroswap", Base: exclBAR, Quote: "fiat:USD", From: at, Until: at.Add(time.Hour),
	}}})
}

// TestAssetListing_ExclusionKeepsTheCache: the listing cache is read and filled whatever the exclusion set holds; a body
// cached before an exclusion is judged on the way out, and an unreadable table still serves from the cache, withheld.
func TestAssetListing_ExclusionKeepsTheCache(t *testing.T) {
	reader := &countingListing{}
	gate := &swapGate{g: oldUnrelatedExcluded()}
	ts := startHTTPTest(t, v1.New(v1.Options{AssetsReader: reader, Exclusions: gate}).Handler())
	get := func() ([]assetRowWire, map[string]any, string) {
		resp := mustGet(t, ts.URL+"/v1/assets")
		body, _ := readAll(resp)
		rows, flags := decodeAssetRows(t, body)
		if len(rows) != 2 {
			t.Fatalf("page: %s", body)
		}
		return rows, flags, resp.Header.Get("Cache-Control")
	}
	rows, _, _ := get()
	once := reader.n.Load()
	for range 4 {
		rows, _, _ = get()
	}
	if got := reader.n.Load(); got != once {
		t.Errorf("unrelated old exclusion: 5 requests made %d upstream reads, want %d", got, once)
	}
	if rows[0]["price_usd"] != "0.5" || rows[1]["price_usd"] != "0.5" {
		t.Errorf("unrelated exclusion withheld a row: %v", rows)
	}

	gate.set(aquaExcluded())
	rows, flags, cc := get()
	wantWithheld(t, rows[0], "source_excluded")
	if rows[1]["price_usd"] != "0.5" || flags["fills_withheld_reason"] != "source_excluded" || cc != "no-store" {
		t.Errorf("cached page judged on the way out: FOO %v, flags %v, Cache-Control %q", rows[1], flags, cc)
	}

	gate.set(assetExclGate(exclTable{err: errors.New("db down")}))
	rows, flags, _ = get()
	for _, r := range rows {
		wantWithheld(t, r, "unattributed")
	}
	if flags["stale"] != true {
		t.Errorf("unread set: flags %v, want stale", flags)
	}
	if got := reader.n.Load(); got != once {
		t.Errorf("exclusions made %d upstream reads, want the cache's %d", got, once)
	}

	gate.set(assetExclGate(exclTable{}))
	if rows, _, _ = get(); rows[0]["price_usd"] != "0.5" {
		t.Errorf("after the exclusion the cached clean page is not served: %v", rows[0])
	}
}

// TestAssetDetail_ExclusionKeepsTheCache: the detail cache holds the unjudged detail, so an exclusion neither bypasses it nor
// leaves the clean value in it once lifted.
func TestAssetDetail_ExclusionKeepsTheCache(t *testing.T) {
	assets := &countingAssets{stubAssetReader: &stubAssetReader{byID: map[string]v1.AssetDetail{
		exclAQUA: {AssetID: exclAQUA, Code: "AQUA", Type: "credit_alphanum4"},
	}}}
	gate := &swapGate{g: oldUnrelatedExcluded()}
	ts := startHTTPTest(t, v1.New(v1.Options{
		Assets: assets, AssetsReader: &stubAssetsReaderExt{row: timescale.AssetRow{AssetID: exclAQUA, PriceUSD: sptr("0.5")}},
		Exclusions: gate,
	}).Handler())
	get := func() (assetRowWire, map[string]any) {
		resp := mustGet(t, ts.URL+"/v1/assets/"+exclAQUA)
		body, _ := readAll(resp)
		var d struct {
			Data  assetRowWire   `json:"data"`
			Flags map[string]any `json:"flags"`
		}
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return d.Data, d.Flags
	}
	d, _ := get()
	once := assets.n.Load()
	for range 4 {
		d, _ = get()
	}
	if d["price_usd"] != "0.5" || assets.n.Load() != once {
		t.Errorf("unrelated old exclusion: price %v, %d upstream reads, want 0.5 and %d", d["price_usd"], assets.n.Load(), once)
	}
	gate.set(aquaExcluded())
	d, flags := get()
	wantWithheld(t, d, "source_excluded")
	if flags["fills_withheld_reason"] != "source_excluded" {
		t.Errorf("flags %v", flags)
	}
	gate.set(assetExclGate(exclTable{err: errors.New("db down")}))
	d, flags = get()
	wantWithheld(t, d, "unattributed")
	if flags["stale"] != true {
		t.Errorf("unread set: flags %v, want stale", flags)
	}
	gate.set(assetExclGate(exclTable{}))
	if d, _ = get(); d["price_usd"] != "0.5" || assets.n.Load() != once {
		t.Errorf("lifted: price %v, %d upstream reads, want 0.5 from the cache (%d)", d["price_usd"], assets.n.Load(), once)
	}
}
