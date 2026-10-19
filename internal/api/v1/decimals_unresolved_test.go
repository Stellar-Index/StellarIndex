// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A FAILED decimals() read (error or deadline) is not "no declaration": the
// scale is unknown, and every surface below publishes a figure scaled by it.
// None may silently serve the default 7 as if it had been read.

var errDecimalsRead = errors.New("clickhouse: context deadline exceeded")

type countingDecStub struct {
	calls atomic.Int32
	d     uint32
	found bool
	err   error
}

func (s *countingDecStub) TokenDecimals(_ context.Context, _ string) (uint32, bool, error) {
	s.calls.Add(1)
	return s.d, s.found, s.err
}

type oneRowSEP41Transfers struct{}

func (oneRowSEP41Transfers) ListSEP41Transfers(
	_ context.Context, _, _, _ string, _ int,
) ([]timescale.SEP41TransferRow, error) {
	return []timescale.SEP41TransferRow{{Kind: "transfer"}}, nil
}

// /v1/contracts/{id}/transfers: `decimals` is the conversion factor for
// every amount, so a failed read is a retryable 503, not decimals: 7. An
// undeclared scale (found=false) keeps the documented default.
func TestSEP41Transfers_DecimalsReadFailed_Returns503(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dec      *countingDecStub
		status   int
		contains string
	}{
		{"read failed", &countingDecStub{err: errDecimalsRead}, http.StatusServiceUnavailable, "decimals"},
		{"not declared", &countingDecStub{}, http.StatusOK, `"decimals":7`},
		{"read", &countingDecStub{d: 18, found: true}, http.StatusOK, `"decimals":18`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{SEP41Transfers: oneRowSEP41Transfers{}, TokenDecimals: tc.dec})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/contracts/"+decTestContract+"/transfers")
			body, _ := readAll(resp)
			if resp.StatusCode != tc.status || !strings.Contains(body, tc.contains) {
				t.Errorf("status = %d, want %d; body must contain %s: %s",
					resp.StatusCode, tc.status, tc.contains, body)
			}
		})
	}
}

// A loopback prewarm re-renders a fresh cached asset instead of being served it, so its cadence keeps the entry inside
// the TTL; a client inside the TTL is still served the cache.
func TestAssetDetail_PrewarmRefreshesFreshCacheEntry(t *testing.T) {
	dec := &countingDecStub{d: 7, found: true}
	srv := v1.New(v1.Options{Prices: lockstepPrices(), Supply: lockstepSupply(), TokenDecimals: dec})
	tsrv := startHTTPTest(t, srv.Handler())
	get := func(ua string) int32 {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, tsrv.URL+"/v1/assets/"+flaggedAsset, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", ua)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		return dec.calls.Load()
	}
	cold := get("client/1")
	if get("client/1") != cold {
		t.Fatal("a second client request inside the TTL re-rendered; the cache is not serving")
	}
	warm := get("stellarindex-prewarm/1")
	if warm == cold {
		t.Error("the prewarm was served the cached entry; it must re-render to refresh it")
	}
	if get("client/1") != warm {
		t.Error("the client request after a prewarm re-rendered; the refreshed entry is not serving")
	}
}
