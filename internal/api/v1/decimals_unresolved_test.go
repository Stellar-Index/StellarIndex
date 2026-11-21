// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
) ([]timescale.SEP41TransferRow, time.Time, error) {
	return []timescale.SEP41TransferRow{{Kind: "transfer", Amount: big.NewInt(123456789)}}, time.Time{}, nil
}

// An unwired lake reader leaves the scale unread, not failed: both raw-unit surfaces still answer 200 with their amounts (and
// history's table-normalised price), omit only the unread `decimals`, and flag the response stale+degraded so no cache keeps it.
func TestRawUnitSurfaces_UnwiredDecimalsReader_ServesFlaggedWithoutDecimals(t *testing.T) {
	pair, err := canonical.NewPair(mustParseAsset(t, decTestContract), mustParseAsset(t, "native"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	tr := mkHistTrade(100)
	tr.Pair = pair
	ts := httpTestServer(t, v1.New(v1.Options{
		History:        &stubHistoryReader{trades: []canonical.Trade{tr}},
		SEP41Transfers: oneRowSEP41Transfers{},
	}))
	for _, tc := range []struct {
		path, absent string
		present      []string
	}{
		{
			"/v1/history?base=" + decTestContract + "&quote=native&limit=50", `"base_decimals"`,
			[]string{`"base_amount":"1"`, `"quote_amount":"100"`, `"price":"`, `"quote_decimals":7`},
		},
		{"/v1/contracts/" + decTestContract + "/transfers", `"decimals"`, []string{`"amount":"123456789"`}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp := mustGet(t, ts.URL+tc.path)
			body, _ := readAll(resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (an unwired scale is not a failed read): %s", resp.StatusCode, body)
			}
			if strings.Contains(body, tc.absent) {
				t.Errorf("unread scale served as a number: %s", body)
			}
			for _, want := range append(tc.present, `"stale":true`) {
				if !strings.Contains(body, want) {
					t.Errorf("body must contain %s: %s", want, body)
				}
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store: Degraded is wire-invisible and surfaces only here", cc)
			}
		})
	}
}

// /v1/contracts/{id}/transfers: `decimals` is the conversion factor for
// every amount, so a failed read is a retryable 503, not decimals: 7. An
// undeclared scale (found=false) or an unwired reader omits the field.
func TestSEP41Transfers_DecimalsReadFailed_Returns503(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dec      v1.TokenDecimalsReader
		status   int
		contains string
	}{
		{"read failed", &countingDecStub{err: errDecimalsRead}, http.StatusServiceUnavailable, "decimals"},
		{"not declared", &countingDecStub{}, http.StatusOK, ""},
		{"no reader", nil, http.StatusOK, ""},
		{"read", &countingDecStub{d: 18, found: true}, http.StatusOK, `"decimals":18`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{SEP41Transfers: oneRowSEP41Transfers{}, TokenDecimals: tc.dec})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/contracts/"+decTestContract+"/transfers")
			body, _ := readAll(resp)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, body)
			}
			if tc.contains == "" {
				if strings.Contains(body, `"decimals"`) {
					t.Errorf("unread scale served as a number: %s", body)
				}
			} else if !strings.Contains(body, tc.contains) {
				t.Errorf("body must contain %s: %s", tc.contains, body)
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
