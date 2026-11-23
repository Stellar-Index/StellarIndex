package main

// The production /v1/price/at reader must hand the served bucket's sources to the exclusion gate; without them every
// exclusion touching the asset withholds a bucket the excluded venue never fed.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	sourcesUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sourcesEURC = "EURC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// sourcedStore answers ClosedVWAPAtOrBefore with one bucket; a non-nil trailingErr fails the served-VWAP guard's fetch, which
// serves the candidate.
type sourcedStore struct {
	row         timescale.VWAPAtRow
	trailingErr error
}

func (s sourcedStore) ClosedVWAPAtOrBefore(context.Context, canonical.Pair, time.Time, time.Duration) (timescale.VWAPAtRow, error) {
	return s.row, nil
}

func (s sourcedStore) ClosedVWAP1mCombinedBefore(context.Context, canonical.Pair, time.Time, int) ([]timescale.Vwap1mRow, error) {
	return nil, s.trailingErr
}

type sourcesExclTable []timescale.SourceExclusion

func (e sourcesExclTable) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	return e, nil
}

func TestStorePriceAtReaderReturnsBucketSources(t *testing.T) {
	ts := time.Now().UTC().Add(-2 * time.Hour)
	pair, err := canonical.NewPair(canonical.NativeAsset(), mustParseAsset(t, sourcesUSDC))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []timescale.HistoryGranularity{timescale.Granularity1h, timescale.Granularity1m} {
		store := sourcedStore{
			row:         timescale.VWAPAtRow{VWAP: "0.25", Bucket: ts.Truncate(time.Hour).Add(-time.Hour), Resolution: g, Sources: []string{"sdex"}},
			trailingErr: errors.New("trailing fetch down"),
		}
		if g == timescale.Granularity1m {
			store.row.Bucket = ts.Truncate(time.Minute).Add(-time.Minute)
		}
		_, _, _, sources, err := storePriceAtReader{s: store}.PriceAt(context.Background(), pair, ts, time.Hour)
		if err != nil || !slices.Equal(sources, []string{"sdex"}) {
			t.Errorf("%s rung: sources = %v, err = %v; want [sdex]", g, sources, err)
		}
	}
}

// TestPriceAtJudgesProductionBucketSources drives /v1/price/at through storePriceAtReader.
func TestPriceAtJudgesProductionBucketSources(t *testing.T) {
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute).Add(30 * time.Second)
	bucket := ts.Truncate(time.Hour).Add(-time.Hour)
	excl := func(base, quote string) sourcesExclTable {
		return sourcesExclTable{{ID: 1, Source: "soroswap", Base: base, Quote: quote, From: ts.Add(-3 * time.Hour)}}
	}
	for _, tc := range []struct {
		name     string
		sources  []string
		rows     sourcesExclTable
		withheld bool
	}{
		{"soroswap on XLM/EURC, sdex bucket", []string{"sdex"}, excl("native", sourcesEURC), false},
		{"source-wide soroswap, sdex bucket", []string{"sdex"}, excl("", ""), false},
		{"source-wide soroswap, bucket it fed", []string{"sdex", "soroswap"}, excl("", ""), true},
		{"no sources recorded: unattributed", nil, excl("native", sourcesEURC), true},
	} {
		reader := storePriceAtReader{s: sourcedStore{row: timescale.VWAPAtRow{
			VWAP: "0.25", Bucket: bucket, Resolution: timescale.Granularity1h, Sources: tc.sources,
		}}}
		srv := httptest.NewServer(v1.New(v1.Options{
			PriceAt:    reader,
			Exclusions: pricingguard.NewSourceExclusionGate(tc.rows, pricingguard.SourceExclusionGateOptions{}),
		}).Handler())
		resp, err := http.Get(srv.URL + "/v1/price/at?asset=native&quote=" + sourcesUSDC + "&ts=" + ts.Format(time.RFC3339))
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		srv.Close()
		served := resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"price":"0.25"`)
		refused := resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), `"source_excluded"`)
		if (tc.withheld && !refused) || (!tc.withheld && !served) {
			t.Errorf("%s: %d %s, want withheld=%v", tc.name, resp.StatusCode, body, tc.withheld)
		}
	}
}

func mustParseAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
