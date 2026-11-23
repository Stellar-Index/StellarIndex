package v1

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// scamTaggedDirectory answers every address with a scam-class directory tag.
type scamTaggedDirectory struct{}

func (scamTaggedDirectory) DirectoryEntryByAddress(_ context.Context, addr string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{Address: addr, Tags: []string{timescale.DirectoryScamFlagTags[0]}}, true, nil
}

func (d scamTaggedDirectory) DirectoryEntriesByAddresses(ctx context.Context, addrs []string) (map[string]timescale.DirectoryEntry, error) {
	out := make(map[string]timescale.DirectoryEntry, len(addrs))
	for _, a := range addrs {
		out[a], _, _ = d.DirectoryEntryByAddress(ctx, a)
	}
	return out, nil
}

// TestGlobalAssetSlug_IssuerVerdictMatchesListing: /v1/assets/{slug} judges the entry's Stellar twin on its issuer as the
// listing's catalogue row does, so a flagged issuer's price is withheld as scam_issuer and an unreadable directory withholds
// it as unattributed with flags.stale; with no directory wired the price is served.
func TestGlobalAssetSlug_IssuerVerdictMatchesListing(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	listing := "0.4321"
	get := func(t *testing.T, dir explorerpkg.DirectoryReader) string {
		t.Helper()
		srv := New(Options{
			VerifiedCurrencies: cat,
			GlobalPrice:        newWithheldTierReader(nil),
			AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
			Directory:          dir,
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		resp, err := http.Get(ts.URL + "/v1/assets/usdc") //nolint:noctx // test-local server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		return string(b)
	}

	if body := get(t, nil); !strings.Contains(body, `"price_usd":"0.4321"`) || strings.Contains(body, "price_withheld_reason") {
		t.Fatalf("no directory: %s, want the listing price", body)
	}
	if body := get(t, scamTaggedDirectory{}); strings.Contains(body, "price_usd") ||
		!strings.Contains(body, `"price_withheld_reason":"scam_issuer"`) || strings.Contains(body, "price_sources") {
		t.Errorf("flagged issuer: %s, want no price, reason scam_issuer", body)
	}
	if body := get(t, failingDirectory{}); strings.Contains(body, "price_usd") ||
		!strings.Contains(body, `"price_withheld_reason":"unattributed"`) ||
		!strings.Contains(body, `"stale":true`) {
		t.Errorf("directory down: %s, want no price, reason unattributed, stale", body)
	}

	// Native has no issuer to judge: an unreadable directory must not withhold XLM.
	xlm, ok := cat.LookupBySlug("xlm")
	if !ok || xlm.StellarEntry() == nil || xlm.StellarEntry().AssetID != "native" {
		t.Fatal("embedded catalogue has no native xlm issuance")
	}
	srv := New(Options{VerifiedCurrencies: cat, Directory: failingDirectory{}})
	ctx, rep := withReadReport(context.Background())
	view := srv.withTwinIssuerVerdict(ctx, GlobalAssetView{PriceUSD: &listing}, xlm)
	if view.PriceUSD == nil || view.PriceWithheldReason != "" || rep.unread.Load() {
		t.Errorf("xlm: price=%v reason=%q unread=%v, want the price untouched", view.PriceUSD, view.PriceWithheldReason, rep.unread.Load())
	}
}
