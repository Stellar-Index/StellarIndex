package divergence

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestDefaultCoinGeckoIDMap_ServedAssetKeysAreCanonicalIDs(t *testing.T) {
	m := defaultCoinGeckoIDMap()
	for id, slug := range servedStellarCoinGeckoSlugs {
		asset, err := canonical.ParseAsset(id)
		if err != nil {
			t.Errorf("%s: not a canonical asset id: %v", id, err)
			continue
		}
		if got := asset.String(); got != id {
			t.Errorf("%s: round-trips as %q", id, got)
		}
		for _, form := range canonical.AssetAliasStrings(asset) {
			if m[form] != slug {
				t.Errorf("%s: alias form %q maps to %q, want %q", id, form, m[form], slug)
			}
		}
	}
	if m[canonical.XLMSacContractID] != "stellar" {
		t.Errorf("native SAC maps to %q, want stellar", m[canonical.XLMSacContractID])
	}
}

func TestCoinGeckoReference_ServedClassicAssetResolvesWithoutOperatorMap(t *testing.T) {
	var gotIDs string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIDs = r.URL.Query().Get("ids")
		_, _ = fmt.Fprintf(w, `{"aquarius":{"usd":0.00033,"last_updated_at":%d}}`, time.Now().Unix())
	}))
	defer ts.Close()
	ref := NewCoinGeckoReference(CoinGeckoOptions{BaseURL: ts.URL})
	aqua, err := canonical.ParseAsset("AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("parse AQUA: %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	q, err := ref.LookupQuote(context.Background(), canonical.Pair{Base: aqua, Quote: usd}, time.Now())
	if err != nil {
		t.Fatalf("LookupQuote: %v", err)
	}
	if q.Price != 0.00033 {
		t.Errorf("price = %g, want 0.00033", q.Price)
	}
	if !strings.Contains(","+gotIDs+",", ",aquarius,") {
		t.Errorf("ids query %q does not request aquarius", gotIDs)
	}
}
