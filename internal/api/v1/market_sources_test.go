package v1_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubMarketSourceReader struct {
	pair  []timescale.SourceStats
	asset []timescale.SourceStats
	err   error

	// Captured args from the last call, for asserting alias expansion.
	gotBase  []string
	gotQuote []string
	gotAsset []string
}

func (r *stubMarketSourceReader) PairSourceStats(_ context.Context, base, quote []string) ([]timescale.SourceStats, error) {
	r.gotBase, r.gotQuote = base, quote
	if r.err != nil {
		return nil, r.err
	}
	return r.pair, nil
}

func (r *stubMarketSourceReader) AssetSourceStats(_ context.Context, asset []string) ([]timescale.SourceStats, error) {
	r.gotAsset = asset
	if r.err != nil {
		return nil, r.err
	}
	return r.asset, nil
}

func TestMarketSources_PairBreakdownShares(t *testing.T) {
	reader := &stubMarketSourceReader{pair: []timescale.SourceStats{
		{Source: "sdex", TradeCount24h: 10, VolumeUSD24h: sql.NullString{String: "300", Valid: true}},
		{Source: "soroswap", TradeCount24h: 5, VolumeUSD24h: sql.NullString{String: "100", Valid: true}},
	}}
	srv := v1.New(v1.Options{MarketSources: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets/sources?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct{ Data v1.MarketSourcesResp }
	mustDecode(t, resp, &env)
	if env.Data.Base != "native" || env.Data.Quote != "fiat:USD" {
		t.Errorf("echoed base/quote = %q/%q", env.Data.Base, env.Data.Quote)
	}
	if len(env.Data.Sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(env.Data.Sources))
	}
	// sdex = 300/400 = 75%, soroswap = 25%.
	if got := env.Data.Sources[0]; got.Source != "sdex" || got.SharePct < 74.9 || got.SharePct > 75.1 {
		t.Errorf("sdex row = %+v, want ~75%% share", got)
	}
	if got := env.Data.Sources[1]; got.Source != "soroswap" || got.SharePct < 24.9 || got.SharePct > 25.1 {
		t.Errorf("soroswap row = %+v, want ~25%% share", got)
	}
}

func TestMarketSources_AssetForm(t *testing.T) {
	reader := &stubMarketSourceReader{asset: []timescale.SourceStats{
		{Source: "binance", TradeCount24h: 42, VolumeUSD24h: sql.NullString{String: "1000", Valid: true}},
	}}
	srv := v1.New(v1.Options{MarketSources: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/markets/sources?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct{ Data v1.MarketSourcesResp }
	mustDecode(t, resp, &env)
	if env.Data.Asset != "native" || len(env.Data.Sources) != 1 || env.Data.Sources[0].Source != "binance" {
		t.Errorf("asset breakdown = %+v", env.Data)
	}
}

func TestMarketSources_ExpandsXLMAliasForms(t *testing.T) {
	// A `native` query must reach storage as BOTH canonical XLM forms so
	// the per-source aggregate counts the SDEX legs (native) and the CEX
	// legs (crypto:XLM) together, not one at a time.
	reader := &stubMarketSourceReader{asset: []timescale.SourceStats{
		{Source: "binance", TradeCount24h: 1, VolumeUSD24h: sql.NullString{String: "1", Valid: true}},
	}}
	srv := v1.New(v1.Options{MarketSources: reader})
	ts := httpTestServer(t, srv)

	// asset= variant.
	resp := mustGet(t, ts.URL+"/v1/markets/sources?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if !containsAll(reader.gotAsset, "native", "crypto:XLM") {
		t.Errorf("AssetSourceStats got forms %v, want both native and crypto:XLM", reader.gotAsset)
	}

	// base/quote variant — the crypto:XLM base must ALSO expand to native.
	resp = mustGet(t, ts.URL+"/v1/markets/sources?base=crypto:XLM&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if !containsAll(reader.gotBase, "crypto:XLM", "native") {
		t.Errorf("PairSourceStats base got forms %v, want both crypto:XLM and native", reader.gotBase)
	}
	// A single-form asset (fiat:USD) stays a single form — no spurious aliases.
	if len(reader.gotQuote) != 1 || reader.gotQuote[0] != "fiat:USD" {
		t.Errorf("PairSourceStats quote got %v, want exactly [fiat:USD]", reader.gotQuote)
	}
}

// TestMarketSources_MembershipMatchesSACValuation is the C4-012
// regression guard (audit-2026-07-23). The per-source breakdown SQL has
// always VALUED a `CAS3J7GY…` leg as XLM (its volume CASE applies the
// XLM/USD rate to it), while the handler's form set passed only
// `native` + `crypto:XLM` — so every Soroban XLM trade was excluded
// from the population it was willing to price, and
// /v1/markets/sources?asset=native undercounted Soroban XLM volume in
// production. Membership and valuation must name the SAME set of forms.
func TestMarketSources_MembershipMatchesSACValuation(t *testing.T) {
	reader := &stubMarketSourceReader{asset: []timescale.SourceStats{
		{Source: "aquarius", TradeCount24h: 1, VolumeUSD24h: sql.NullString{String: "1", Valid: true}},
	}}
	srv := v1.New(v1.Options{MarketSources: reader})
	ts := httpTestServer(t, srv)

	// The SQL's own XLM literal — the same constant the query embeds.
	const xlmSAC = canonical.XLMSacContractID

	for _, q := range []string{"native", "crypto:XLM", xlmSAC} {
		resp := mustGet(t, ts.URL+"/v1/markets/sources?asset="+q)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("asset=%s: status=%d want 200", q, resp.StatusCode)
		}
		if !containsAll(reader.gotAsset, "native", "crypto:XLM", xlmSAC) {
			t.Errorf("asset=%s: AssetSourceStats got forms %v, want all three XLM forms "+
				"(the volume CASE prices the SAC leg as XLM, so the membership filter must match it)",
				q, reader.gotAsset)
		}
	}

	// Both LEGS of a pair query expand too — XLM is a quote as often as
	// it is a base (AQUA/XLM), and the pair query's CASE checks
	// quote_asset against the same SAC literal.
	resp := mustGet(t, ts.URL+"/v1/markets/sources?base=AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA&quote=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair: status=%d want 200", resp.StatusCode)
	}
	if !containsAll(reader.gotQuote, "native", "crypto:XLM", xlmSAC) {
		t.Errorf("PairSourceStats quote got forms %v, want all three XLM forms", reader.gotQuote)
	}
	// The non-XLM leg stays a single form — no spurious alias fan-out.
	if len(reader.gotBase) != 1 {
		t.Errorf("PairSourceStats base got %v, want exactly one form for a non-aliased asset", reader.gotBase)
	}
}

func containsAll(got []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestMarketSources_MissingParamsIs400(t *testing.T) {
	srv := v1.New(v1.Options{MarketSources: &stubMarketSourceReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/markets/sources")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestMarketSources_ConflictingFiltersIs400(t *testing.T) {
	srv := v1.New(v1.Options{MarketSources: &stubMarketSourceReader{}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/markets/sources?asset=native&base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestMarketSources_NilReaderEmpty(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/markets/sources?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var env struct{ Data v1.MarketSourcesResp }
	mustDecode(t, resp, &env)
	if len(env.Data.Sources) != 0 {
		t.Errorf("want empty sources, got %d", len(env.Data.Sources))
	}
}
