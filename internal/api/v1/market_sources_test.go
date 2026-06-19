package v1_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

type stubMarketSourceReader struct {
	pair  []timescale.SourceStats
	asset []timescale.SourceStats
	err   error
}

func (r *stubMarketSourceReader) PairSourceStats(_ context.Context, _, _ string) ([]timescale.SourceStats, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.pair, nil
}

func (r *stubMarketSourceReader) AssetSourceStats(_ context.Context, _ string) ([]timescale.SourceStats, error) {
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
